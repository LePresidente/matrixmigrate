package matrix

import (
	"encoding/json"
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

// defaultPinPowerLevel is what the Matrix spec gives state_default. PowerLevelsContent is
// unmarshalled with omitempty, so an absent state_default and a genuine 0 are indistinguishable;
// assuming 50 picks a user who can pin either way.
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

	pl, creator := c.pinAuthority(roomID)
	if pl == nil {
		return fmt.Errorf("admin lacks power to pin in %s and its power levels are unreadable: %w", roomID, err)
	}

	required := requiredPinPowerLevel(pl)
	sender := pickPinCapableUser(pl, c.homeserver, required, creator)
	if sender == "" {
		return fmt.Errorf("admin lacks power to pin in %s and no local member has power level %d: %w", roomID, required, err)
	}

	logger.Debug("PinEvents: pinning in room=%s as %s (needs power %d)", roomID, sender, required)
	return c.setPinnedEventsAsUser(roomID, eventIDs, sender)
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
	return pinnedFromState(state), nil
}

// pinAuthority reports what decides who may pin in a room: its power levels, and its creator,
// who from room version 12 holds implicit power without appearing in content.users.
//
// Power levels are read as the admin where that works and from the admin API where it does
// not, for the same reason GetPinnedEvents has that fallback: after `import leave-rooms` the
// admin is not in the room and its own read comes back forbidden.
func (c *Client) pinAuthority(roomID string) (*PowerLevelsContent, string) {
	creator := c.lookupRoomInfo(roomID).creator

	pl, err := c.getPowerLevels(roomID)
	if err == nil {
		return pl, creator
	}
	logger.Debug("pinAuthority: power levels of room=%s unreadable as admin (%v); trying the admin API", roomID, err)

	state, stateErr := c.adminRoomState(roomID)
	if stateErr != nil {
		logger.Debug("pinAuthority: admin API refused room=%s too: %v", roomID, stateErr)
		return nil, creator
	}
	if creator == "" {
		creator = creatorFromState(state)
	}
	return powerLevelsFromState(state), creator
}

// pinnedFromState picks the pinned event IDs out of an admin API state dump. A room with no
// m.room.pinned_events has never been pinned to, which is an empty list rather than an error.
func pinnedFromState(state []adminStateEvent) []string {
	for _, event := range state {
		if event.Type != EventTypePinnedEvents || event.StateKey != "" {
			continue
		}
		var content PinnedEventsContent
		if json.Unmarshal(event.Content, &content) != nil {
			return nil
		}
		return content.Pinned
	}
	return nil
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
		return fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}
	return nil
}

// requiredPinPowerLevel reports the power level needed to send m.room.pinned_events.
func requiredPinPowerLevel(pl *PowerLevelsContent) int {
	if pl == nil {
		return defaultPinPowerLevel
	}
	if level, ok := pl.Events[EventTypePinnedEvents]; ok {
		return level
	}
	if pl.StateDefault > 0 {
		return pl.StateDefault
	}
	return defaultPinPowerLevel
}

// pickPinCapableUser returns the local member best placed to pin, or "" when nobody qualifies.
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
func pickPinCapableUser(pl *PowerLevelsContent, homeserver string, required int, creator string) string {
	suffix := ":" + homeserver

	best, bestLevel := "", -1
	if pl != nil {
		for user, level := range pl.Users {
			if level < required || !strings.HasSuffix(user, suffix) {
				continue
			}
			if level > bestLevel || (level == bestLevel && user < best) {
				best, bestLevel = user, level
			}
		}
	}
	if best != "" {
		return best
	}
	if creator != "" && strings.HasSuffix(creator, suffix) {
		return creator
	}
	return ""
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
// event, so a room with forty pinned posts costs one read and one write.
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

	for idx, roomID := range rooms {
		current, err := i.client.GetPinnedEvents(roomID)
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

		if err := i.client.PinEvents(roomID, merged); err != nil {
			result.Stats.PinsFailed++
			result.Errors = append(result.Errors,
				fmt.Sprintf("Failed to pin messages in room %s: %v", roomID, err))
			if progress != nil {
				progress(idx+1, total, PinProgressStage, "failed")
			}
			continue
		}

		result.Stats.PinnedRoomsUpdated++
		if added := len(merged) - len(current); added > 0 {
			result.Stats.PinnedEventsAdded += added
		}
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
