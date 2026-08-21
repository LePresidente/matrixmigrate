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
func (c *Client) GetPinnedEvents(roomID string) ([]string, error) {
	endpoint := fmt.Sprintf("/_matrix/client/v3/rooms/%s/state/%s",
		url.PathEscape(roomID), EventTypePinnedEvents)

	body, statusCode, err := c.doRequest("GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	if statusCode == http.StatusNotFound {
		return nil, nil
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

	pl, plErr := c.getPowerLevels(roomID)
	if plErr != nil {
		return fmt.Errorf("admin lacks power to pin in %s and its power levels are unreadable (%v): %w", roomID, plErr, err)
	}

	required := requiredPinPowerLevel(pl)
	sender := pickPinCapableUser(pl, c.homeserver, required)
	if sender == "" {
		return fmt.Errorf("admin lacks power to pin in %s and no local member has power level %d: %w", roomID, required, err)
	}

	logger.Debug("PinEvents: pinning in room=%s as %s (needs power %d)", roomID, sender, required)
	return c.setPinnedEventsAsUser(roomID, eventIDs, sender)
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
func pickPinCapableUser(pl *PowerLevelsContent, homeserver string, required int) string {
	if pl == nil {
		return ""
	}
	suffix := ":" + homeserver

	best, bestLevel := "", -1
	for user, level := range pl.Users {
		if level < required || !strings.HasSuffix(user, suffix) {
			continue
		}
		if level > bestLevel || (level == bestLevel && user < best) {
			best, bestLevel = user, level
		}
	}
	return best
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
