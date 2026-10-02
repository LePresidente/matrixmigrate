package matrix

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/aligundogdu/matrixmigrate/internal/logger"
	"github.com/aligundogdu/matrixmigrate/internal/mattermost"
)

// EventTypePinnedEvents is the room state event holding a room's pinned message list.
const EventTypePinnedEvents = "m.room.pinned_events"

// defaultPinPowerLevel is what the Matrix spec gives state_default when the power levels do
// not set it.
const defaultPinPowerLevel = 50

// PinnedEventsContent is the content of an m.room.pinned_events state event.
type PinnedEventsContent struct {
	Pinned []string `json:"pinned"`
}

// GetPinnedEvents returns the room's current pinned event IDs. A room that has never had a
// pin has no such state event, which is not an error: it returns an empty list.
//
// It joins the admin to the room first, the same guarantee setPinnedEvents makes before its
// PUT: Synapse answers a GET from a user who is not in the room with 403 M_FORBIDDEN, not 404,
// so without this the room would look pin-free and importPins would write back a list that
// drops every pin made on the Matrix side. ensureAdminInRoom is cached per room, so this costs
// nothing once the admin is already joined.
//
// The admin cannot always get in. `import leave-rooms` withdraws it from every migrated room
// at the end of a run, and an invite-only room will not take it back, so on a replay most
// rooms refuse both the join and the read. The Synapse admin API reads room state without
// membership, and that is the fallback: refusing to read would make the room look pin-free,
// which is the one answer that loses data.
func (c *Client) GetPinnedEvents(roomID string) ([]string, error) {
	if err := c.ensureAdminInRoom(roomID); err != nil {
		logger.Debug("GetPinnedEvents: admin cannot join room=%s (%v); reading state through the admin API", roomID, err)
		return c.pinnedEventsViaAdminAPI(roomID, err)
	}

	endpoint := fmt.Sprintf("/_matrix/client/v3/rooms/%s/state/%s",
		url.PathEscape(roomID), EventTypePinnedEvents)

	body, statusCode, err := c.doRequest("GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	if statusCode == http.StatusNotFound {
		return nil, nil
	}
	if statusCode == http.StatusForbidden {
		cause := fmt.Errorf("API error (%d): %s", statusCode, strings.TrimSpace(string(body)))
		logger.Debug("GetPinnedEvents: admin refused for room=%s; reading state through the admin API", roomID)
		return c.pinnedEventsViaAdminAPI(roomID, cause)
	}
	if statusCode != http.StatusOK {
		var resp GenericResponse
		json.Unmarshal(body, &resp)
		return nil, fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}

	var content PinnedEventsContent
	if err := json.Unmarshal(body, &content); err != nil {
		return nil, fmt.Errorf("failed to parse pinned events of room %s: %w", roomID, err)
	}
	return content.Pinned, nil
}

// PinEvents writes the room's pin list, as the admin where it can and through the Application
// Service as a sufficiently powerful local member where it cannot.
//
// With preserve_owner_and_alias enabled the admin is not the room creator and can sit at power
// level 0, while pinning needs state_default (normally 50). That is the case the fallback
// exists for; without an AS token the caller gets an error and reports the room as failed
// rather than the run dying.
func (c *Client) PinEvents(roomID string, eventIDs []string) error {
	err := c.setPinnedEvents(roomID, eventIDs)
	if err == nil {
		return nil
	}
	if !strings.Contains(err.Error(), "M_FORBIDDEN") {
		return err
	}
	logger.Debug("PinEvents: admin refused for room=%s (%v); trying the Application Service", roomID, err)

	if c.asToken == "" {
		return fmt.Errorf("admin lacks power to pin in %s and no Application Service token is configured: %w", roomID, err)
	}

	pl, creator, joined := c.pinAuthority(roomID)
	if pl == nil {
		return fmt.Errorf("admin lacks power to pin in %s and its power levels are unreadable: %w", roomID, err)
	}

	required := requiredPinPowerLevel(pl)
	candidates := pinCandidates(pl, c.homeserver, required, creator, joined)
	if len(candidates) == 0 {
		return fmt.Errorf("admin lacks power to pin in %s and no joined local member has power level %d: %w", roomID, required, err)
	}

	logger.Debug("PinEvents: room=%s needs power %d; candidates %v", roomID, required, candidates)
	return c.pinAsCandidates(roomID, eventIDs, candidates)
}

// pinAsCandidates writes the pin list through the Application Service as each candidate in
// turn, stopping at the first that succeeds. A refusal (4xx) moves on to the next candidate; a
// transport failure or a 5xx ends the walk. When every candidate is refused the last refusal is
// returned.
//
// Power outlives membership: a room's power_levels keeps its entry for someone who has
// left, and the migration bot is usually the highest-powered name in it long after
// leave-rooms withdrew it. Walking the list beats trusting the first name on it.
func (c *Client) pinAsCandidates(roomID string, eventIDs, candidates []string) error {
	var lastErr error
	for _, sender := range candidates {
		asErr := c.setPinnedEventsAsUser(roomID, eventIDs, sender)
		if asErr == nil {
			return nil
		}
		lastErr = asErr
		if !isPinRefusal(asErr) {
			return asErr
		}
		logger.Debug("pinAsCandidates: %s could not pin in room=%s (%v); trying the next candidate", sender, roomID, asErr)
	}
	return lastErr
}

// pinRoomView is what one read of a room's state through the Synapse admin API says about
// pinning in it. The pin pass reads each room once and decides everything from this.
type pinRoomView struct {
	pinned  []string
	levels  *PowerLevelsContent // nil when the room has no power levels
	creator string
	joined  map[string]struct{}
}

// pinRoomViewFromState derives a pinRoomView from an admin API state dump. It fails when the
// room's pinned content does not parse.
func pinRoomViewFromState(state []adminStateEvent) (pinRoomView, error) {
	pinned, err := pinnedFromState(state)
	if err != nil {
		return pinRoomView{}, err
	}
	return pinRoomView{
		pinned:  pinned,
		levels:  powerLevelsFromState(state),
		creator: creatorFromState(state),
		joined:  joinedFromState(state),
	}, nil
}

// readRoomPins returns the room's current pins and, when the admin API could read the room,
// the view a later writeRoomPins decides from. A nil view means the admin API refused, and
// the pins came from GetPinnedEvents instead.
func (c *Client) readRoomPins(roomID string) ([]string, *pinRoomView, error) {
	state, err := c.adminRoomState(roomID)
	if err != nil {
		logger.Debug("readRoomPins: admin API refused room=%s (%v); reading pins as the admin", roomID, err)
		current, getErr := c.GetPinnedEvents(roomID)
		return current, nil, getErr
	}
	view, err := pinRoomViewFromState(state)
	if err != nil {
		return nil, nil, err
	}
	return view.pinned, &view, nil
}

// writeRoomPins writes the room's pin list using what readRoomPins learned about the room.
//
// The admin writes when the room's state shows it joined with the power to pin. Otherwise the
// Application Service writes as a joined local member who has that power. Only when neither
// can work — no AS token, or every candidate refused — is the admin joined to the room and
// made to write, which is what a deployment without an AS token has always done. That last
// step is skipped when the admin was already in the room and refused: joining changes nothing.
//
// adminID is the admin's own user ID, "" when it is not known; the admin is then never
// assumed to be in the room. A room whose state carries no power levels is an error: nobody is
// tried and the admin is not joined.
func (c *Client) writeRoomPins(roomID string, eventIDs []string, view *pinRoomView, adminID string) error {
	if view == nil {
		return c.PinEvents(roomID, eventIDs)
	}
	if view.levels == nil {
		return fmt.Errorf("room %s has no %s in its state; not guessing who may pin", roomID, EventTypePowerLevels)
	}

	required := requiredPinPowerLevel(view.levels)
	var adminErr error
	if adminCanPin(*view, adminID, required) {
		endpoint := fmt.Sprintf("/_matrix/client/v3/rooms/%s/state/%s",
			url.PathEscape(roomID), EventTypePinnedEvents)
		adminErr = c.putPinnedEvents(endpoint, eventIDs, "")
		if adminErr == nil || !isPinRefusal(adminErr) {
			return adminErr
		}
		logger.Debug("writeRoomPins: admin refused in room=%s (%v); trying the Application Service", roomID, adminErr)
	}

	var asErr error
	if c.asToken != "" {
		candidates := pinCandidates(view.levels, c.homeserver, required, view.creator, view.joined)
		if len(candidates) > 0 {
			asErr = c.pinAsCandidates(roomID, eventIDs, candidates)
			if asErr == nil || !isPinRefusal(asErr) {
				return asErr
			}
		}
		logger.Debug("writeRoomPins: no joined member could pin in room=%s (candidates %v, last error %v)",
			roomID, candidates, asErr)
	}

	// An admin that is already in the room and was refused gains nothing from joining it.
	if adminErr != nil {
		if asErr != nil {
			return fmt.Errorf("%w (application service attempt: %v)", adminErr, asErr)
		}
		return adminErr
	}
	err := c.setPinnedEvents(roomID, eventIDs)
	if err != nil && asErr != nil {
		return fmt.Errorf("%w (application service attempt: %v)", err, asErr)
	}
	return err
}

// adminCanPin reports whether the room's state shows the admin joined with enough power to
// pin. A creator absent from content.users is the room version 12 shape, where creators hold
// implicit power.
func adminCanPin(view pinRoomView, adminID string, required int) bool {
	if adminID == "" || view.levels == nil {
		return false
	}
	if _, in := view.joined[adminID]; !in {
		return false
	}
	level, listed := view.levels.Users[adminID]
	if !listed {
		if adminID == view.creator {
			return true
		}
		level = view.levels.UsersDefault
	}
	return level >= required
}

// pinCandidates returns the local members the Application Service may pin as, in the order to
// try them: the explicitly powered ones and the creator, as pinCapableUsers ranks them, then —
// when users_default alone is enough to pin — every other joined local member, by user ID.
//
// A nil joined set means membership is unknown: every candidate pinCapableUsers names is then
// kept, and nobody can be added on the strength of users_default.
func pinCandidates(pl *PowerLevelsContent, homeserver string, required int, creator string, joined map[string]struct{}) []string {
	candidates := pinCapableUsers(pl, homeserver, required, creator)
	if joined == nil {
		return candidates
	}
	candidates = onlyJoined(candidates, joined)
	if pl == nil || pl.UsersDefault < required {
		return candidates
	}

	suffix := ":" + homeserver
	listed := make(map[string]struct{}, len(candidates))
	for _, user := range candidates {
		listed[user] = struct{}{}
	}
	var unlisted []string
	for user := range joined {
		if _, in := listed[user]; in || !strings.HasSuffix(user, suffix) {
			continue
		}
		// An explicit entry overrides users_default, in either direction.
		if _, explicit := pl.Users[user]; explicit {
			continue
		}
		unlisted = append(unlisted, user)
	}
	sort.Strings(unlisted)
	return append(candidates, unlisted...)
}

// onlyJoined keeps the candidates who are currently in the room, in order.
func onlyJoined(candidates []string, joined map[string]struct{}) []string {
	kept := make([]string, 0, len(candidates))
	for _, user := range candidates {
		if _, in := joined[user]; in {
			kept = append(kept, user)
		}
	}
	return kept
}

// adminStateEvent is one entry of the Synapse admin API's room state dump.
type adminStateEvent struct {
	Type     string          `json:"type"`
	StateKey string          `json:"state_key"`
	Sender   string          `json:"sender"`
	Content  json.RawMessage `json:"content"`
}

// adminRoomState reads a room's full state through the Synapse admin API, which does not
// require the caller to be a member of the room.
func (c *Client) adminRoomState(roomID string) ([]adminStateEvent, error) {
	endpoint := "/_synapse/admin/v1/rooms/" + url.PathEscape(roomID) + "/state"
	body, statusCode, err := c.doRequest("GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	if statusCode != http.StatusOK {
		var resp GenericResponse
		json.Unmarshal(body, &resp)
		return nil, fmt.Errorf("admin API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}

	var parsed struct {
		State []adminStateEvent `json:"state"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("failed to parse admin room state of %s: %w", roomID, err)
	}
	return parsed.State, nil
}

// pinnedEventsViaAdminAPI answers GetPinnedEvents from the admin API after the client read was
// refused. cause is the refusal, carried into the error so a failure names both attempts.
func (c *Client) pinnedEventsViaAdminAPI(roomID string, cause error) ([]string, error) {
	state, err := c.adminRoomState(roomID)
	if err != nil {
		return nil, fmt.Errorf("admin cannot read room %s (%v) and the admin API refused too: %w", roomID, cause, err)
	}
	pinned, err := pinnedFromState(state)
	if err != nil {
		return nil, fmt.Errorf("room %s: %w", roomID, err)
	}
	return pinned, nil
}

// pinAuthority reports what decides who may pin in a room: its power levels, its creator —
// who from room version 12 holds implicit power without appearing in content.users — and who
// is currently joined. A nil membership set means it could not be determined, and the caller
// then treats every candidate as reachable rather than discarding them all.
//
// The Synapse admin API is the first choice, not the fallback: it answers whether or not the
// admin is in the room, and it carries the membership the client-side power-level read does
// not. After `import leave-rooms` the admin is in none of these rooms.
func (c *Client) pinAuthority(roomID string) (*PowerLevelsContent, string, map[string]struct{}) {
	creator := c.lookupRoomInfo(roomID).creator

	state, err := c.adminRoomState(roomID)
	if err == nil {
		if pl := powerLevelsFromState(state); pl != nil {
			if creator == "" {
				creator = creatorFromState(state)
			}
			return pl, creator, joinedFromState(state)
		}
		logger.Debug("pinAuthority: admin API state of room=%s carries no power levels", roomID)
	} else {
		logger.Debug("pinAuthority: admin API refused room=%s (%v); reading power levels as admin", roomID, err)
	}

	pl, plErr := c.getPowerLevels(roomID)
	if plErr != nil {
		logger.Debug("pinAuthority: power levels of room=%s unreadable as admin too: %v", roomID, plErr)
		return nil, creator, nil
	}
	return pl, creator, nil
}

// joinedFromState reports which users are currently in the room, from an admin API state dump.
func joinedFromState(state []adminStateEvent) map[string]struct{} {
	joined := make(map[string]struct{})
	for _, event := range state {
		if event.Type != EventTypeRoomMember || event.StateKey == "" {
			continue
		}
		var content struct {
			Membership string `json:"membership"`
		}
		if json.Unmarshal(event.Content, &content) != nil {
			continue
		}
		if content.Membership == "join" {
			joined[event.StateKey] = struct{}{}
		}
	}
	return joined
}

// pinnedFromState picks the pinned event IDs out of an admin API state dump. A room with no
// m.room.pinned_events has never been pinned to, which is an empty list rather than an error.
// Content that does not parse is an error: treating it as "no pins" would let the migrated
// list overwrite whatever it holds.
func pinnedFromState(state []adminStateEvent) ([]string, error) {
	for _, event := range state {
		if event.Type != EventTypePinnedEvents || event.StateKey != "" {
			continue
		}
		var content PinnedEventsContent
		if err := json.Unmarshal(event.Content, &content); err != nil {
			return nil, fmt.Errorf("unparseable %s content: %w", EventTypePinnedEvents, err)
		}
		return content.Pinned, nil
	}
	return nil, nil
}

// powerLevelsFromState picks the power levels out of an admin API state dump. A room without
// them is not a room this code can reason about, so it returns nil and the caller reports the
// room as failed rather than guessing.
func powerLevelsFromState(state []adminStateEvent) *PowerLevelsContent {
	for _, event := range state {
		if event.Type != EventTypePowerLevels || event.StateKey != "" {
			continue
		}
		var content PowerLevelsContent
		if json.Unmarshal(event.Content, &content) != nil {
			return nil
		}
		return &content
	}
	return nil
}

// creatorFromState reports who created the room, for the case where the admin API's room
// summary did not say. From room version 12 the create event carries no creator field, so the
// sender of that event is the answer in every version.
func creatorFromState(state []adminStateEvent) string {
	for _, event := range state {
		if event.Type == EventTypeRoomCreate && event.StateKey == "" {
			return event.Sender
		}
	}
	return ""
}

// setPinnedEvents writes the pin list as the admin user.
func (c *Client) setPinnedEvents(roomID string, eventIDs []string) error {
	if err := c.ensureAdminInRoom(roomID); err != nil {
		return fmt.Errorf("admin join room: %w", err)
	}
	endpoint := fmt.Sprintf("/_matrix/client/v3/rooms/%s/state/%s",
		url.PathEscape(roomID), EventTypePinnedEvents)
	return c.putPinnedEvents(endpoint, eventIDs, "")
}

// setPinnedEventsAsUser writes the pin list as userID through the Application Service.
func (c *Client) setPinnedEventsAsUser(roomID string, eventIDs []string, userID string) error {
	if c.asToken == "" || userID == "" {
		return fmt.Errorf("AS token and userID required")
	}
	params := url.Values{}
	params.Set("user_id", userID)
	endpoint := fmt.Sprintf("/_matrix/client/v3/rooms/%s/state/%s?%s",
		url.PathEscape(roomID), EventTypePinnedEvents, params.Encode())
	return c.putPinnedEvents(endpoint, eventIDs, c.asToken)
}

// putPinnedEvents sends the state event, with the admin token when token is empty.
func (c *Client) putPinnedEvents(endpoint string, eventIDs []string, token string) error {
	content := &PinnedEventsContent{Pinned: eventIDs}
	if content.Pinned == nil {
		content.Pinned = []string{}
	}

	var (
		body       []byte
		statusCode int
		err        error
	)
	if token == "" {
		body, statusCode, err = c.doRequest("PUT", endpoint, content)
	} else {
		body, statusCode, err = c.doRequestWithToken("PUT", endpoint, content, token)
	}
	if err != nil {
		return err
	}
	if statusCode != http.StatusOK {
		var resp GenericResponse
		json.Unmarshal(body, &resp)
		if statusCode >= http.StatusInternalServerError {
			// A 5xx, often a proxy in front of an unreachable homeserver, says nothing about
			// the sender: it is handled like a transport failure, not a refusal.
			return fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
		}
		return &pinRefusal{status: statusCode, errcode: resp.Errcode, message: resp.Error}
	}
	return nil
}

// pinRefusal is the homeserver answering a pin write with a 4xx: a locked or deactivated
// account, a sender without the power, a room it will not touch. It is an answer about that
// sender in that room, unlike a transport failure or a 5xx, which say nothing about the sender
// and everything about whether the next request will get through.
type pinRefusal struct {
	status  int
	errcode string
	message string
}

func (e *pinRefusal) Error() string {
	return fmt.Sprintf("API error (%d): %s - %s", e.status, e.errcode, e.message)
}

// isPinRefusal reports whether err is the homeserver refusing a pin write, as opposed to the
// request failing to get an answer at all.
func isPinRefusal(err error) bool {
	var refusal *pinRefusal
	return errors.As(err, &refusal)
}

// requiredPinPowerLevel reports the power level needed to send m.room.pinned_events.
func requiredPinPowerLevel(pl *PowerLevelsContent) int {
	if pl == nil {
		return defaultPinPowerLevel
	}
	if level, ok := pl.Events[EventTypePinnedEvents]; ok {
		return level
	}
	if pl.stateDefaultSet || pl.StateDefault > 0 {
		return pl.StateDefault
	}
	return defaultPinPowerLevel
}

// pinCapableUsers returns the local members who may pin, strongest first, empty when nobody
// qualifies. The caller walks the list: power levels remember users who have left the room, so
// the first name is a good guess rather than an answer.
//
// Only local users are considered: the Application Service can only act as users on its own
// homeserver. Ties break on the user ID so a re-run picks the same sender and the room's state
// does not churn between senders.
//
// The creator is the last resort rather than the first choice. From room version 12 a creator
// holds implicit infinite power and is forbidden from appearing in content.users, so such a
// room can present an empty users map and still have exactly one member who may pin — but in
// older rooms the creator is an ordinary entry, and an explicitly powered member is the more
// faithful sender when one exists.
func pinCapableUsers(pl *PowerLevelsContent, homeserver string, required int, creator string) []string {
	suffix := ":" + homeserver

	type candidate struct {
		userID string
		level  int
	}
	var powered []candidate
	if pl != nil {
		for user, level := range pl.Users {
			if level < required || !strings.HasSuffix(user, suffix) {
				continue
			}
			powered = append(powered, candidate{userID: user, level: level})
		}
	}
	sort.Slice(powered, func(a, b int) bool {
		if powered[a].level != powered[b].level {
			return powered[a].level > powered[b].level
		}
		return powered[a].userID < powered[b].userID
	})

	ordered := make([]string, 0, len(powered)+1)
	for _, c := range powered {
		ordered = append(ordered, c.userID)
	}
	if creator != "" && strings.HasSuffix(creator, suffix) {
		for _, user := range ordered {
			if user == creator {
				return ordered
			}
		}
		ordered = append(ordered, creator)
	}
	return ordered
}

// unionPinned merges the migrated pin list into what the room already has pinned.
//
// Existing pins keep their order and their place at the front: a pin somebody made on the
// Matrix side after an earlier run must survive the next one. changed is false when the merge
// would write back exactly what is already there, which is what makes a replay free.
func unionPinned(current, migrated []string) ([]string, bool) {
	seen := make(map[string]struct{}, len(current)+len(migrated))
	merged := make([]string, 0, len(current)+len(migrated))

	for _, id := range current {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		merged = append(merged, id)
	}
	// A current list carrying blanks or duplicates is worth rewriting on its own.
	changed := len(merged) != len(current)

	for _, id := range migrated {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		merged = append(merged, id)
		changed = true
	}

	return merged, changed
}

// newPinCount reports how many distinct migrated event IDs the room did not already have
// pinned. It cannot be read off the list lengths: a rewrite also drops blanks and duplicates
// from the current list, which shrinks it by as much as the migrated IDs grow it.
func newPinCount(current, migrated []string) int {
	seen := make(map[string]struct{}, len(current)+len(migrated))
	for _, id := range current {
		seen[id] = struct{}{}
	}
	added := 0
	for _, id := range migrated {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		added++
	}
	return added
}

// PinProgressStage is passed in the channel slot of MessageImportCallback while the pin pass
// runs, so a front end can label the progress instead of reporting rooms as messages.
const PinProgressStage = "pins"

// PinImport turns the pin pass on; a nil value turns it off. It carries no data of its own —
// the pin flag rides on the posts the importer was already handed — and exists so the call
// site reads like the reaction one rather than taking a bare boolean.
type PinImport struct{}

// PinSkip records a pinned post that could not be carried across, and why.
type PinSkip struct {
	PostID string
	Reason string
}

// pinnedByRoom turns the pinned posts of an export into the ordered event-ID list each room
// should carry. Order is post creation time ascending, with the post ID breaking ties so a
// re-run produces the same list and the room's state does not churn.
//
// A deleted post is not reported as a skip: it was never meant to reach Matrix.
func pinnedByRoom(posts []mattermost.Post, eventByPost, roomByPost map[string]string) (map[string][]string, []PinSkip) {
	pinned := make([]mattermost.Post, 0)
	for _, p := range posts {
		if p.IsPinned && !p.IsDeleted() {
			pinned = append(pinned, p)
		}
	}
	sort.SliceStable(pinned, func(a, b int) bool {
		if pinned[a].CreateAt != pinned[b].CreateAt {
			return pinned[a].CreateAt < pinned[b].CreateAt
		}
		return pinned[a].ID < pinned[b].ID
	})

	byRoom := make(map[string][]string)
	var skips []PinSkip
	for _, p := range pinned {
		roomID := roomByPost[p.ID]
		eventID := eventByPost[p.ID]
		switch {
		case roomID == "":
			skips = append(skips, PinSkip{PostID: p.ID, Reason: "no room mapping"})
		case eventID == "":
			skips = append(skips, PinSkip{PostID: p.ID, Reason: "message not imported"})
		default:
			byRoom[roomID] = append(byRoom[roomID], eventID)
		}
	}
	return byRoom, skips
}

// importPins writes each room's pinned message list, once the messages themselves have event
// IDs. It runs after the reaction pass for the same reason that pass runs last: a pin can only
// point at an event that already exists.
//
// A room is the unit of work here, not a post: Matrix holds the whole pin list in one state
// event, so a room with forty pinned posts costs one read and one write. The read goes through
// the Synapse admin API, which needs no membership, so a room the admin has left is not joined
// just to be looked at.
func (i *Importer) importPins(
	result *ImportMessagesResult,
	posts []mattermost.Post,
	roomByPost map[string]string,
	progress MessageImportCallback,
) {
	byRoom, skips := pinnedByRoom(posts, result.Mapping, roomByPost)

	tally := &skipTally{}
	for _, skip := range skips {
		tally.add(skip.Reason)
	}
	result.Stats.PinsSkipped += len(skips)

	rooms := make([]string, 0, len(byRoom))
	for roomID := range byRoom {
		rooms = append(rooms, roomID)
	}
	sort.Strings(rooms)

	total := len(rooms)
	logger.Info("Starting pinned message import: %d room(s) with pins, %d pinned post(s) unusable", total, len(skips))

	var adminID string
	if total > 0 {
		if me, err := i.client.WhoAmI(); err == nil {
			adminID = me.UserID
		} else {
			logger.Debug("importPins: cannot tell who the admin is (%v); it will not write without joining first", err)
		}
	}

	for idx, roomID := range rooms {
		current, view, err := i.client.readRoomPins(roomID)
		if err != nil {
			result.Stats.PinsFailed++
			result.Errors = append(result.Errors,
				fmt.Sprintf("Failed to read pinned messages of room %s: %v", roomID, err))
			if progress != nil {
				progress(idx+1, total, PinProgressStage, "failed")
			}
			continue
		}

		merged, changed := unionPinned(current, byRoom[roomID])
		if !changed {
			result.Stats.PinnedRoomsUnchanged++
			if progress != nil {
				progress(idx+1, total, PinProgressStage, "skipped")
			}
			continue
		}

		if err := i.client.writeRoomPins(roomID, merged, view, adminID); err != nil {
			result.Stats.PinsFailed++
			result.Errors = append(result.Errors,
				fmt.Sprintf("Failed to pin messages in room %s: %v", roomID, err))
			if progress != nil {
				progress(idx+1, total, PinProgressStage, "failed")
			}
			continue
		}

		result.Stats.PinnedRoomsUpdated++
		result.Stats.PinnedEventsAdded += newPinCount(current, byRoom[roomID])
		if progress != nil {
			progress(idx+1, total, PinProgressStage, "imported")
		}
	}

	logger.Info("Pinned message import completed: rooms_updated=%d, rooms_unchanged=%d, events_added=%d, skipped=%d, failed=%d",
		result.Stats.PinnedRoomsUpdated, result.Stats.PinnedRoomsUnchanged,
		result.Stats.PinnedEventsAdded, result.Stats.PinsSkipped, result.Stats.PinsFailed)
	if summary := tally.String(); summary != "" {
		logger.Info("Pinned posts skipped by reason: %s", summary)
	}
}
