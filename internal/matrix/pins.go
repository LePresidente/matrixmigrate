package matrix

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/aligundogdu/matrixmigrate/internal/logger"
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
