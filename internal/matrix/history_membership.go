package matrix

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"

	"github.com/aligundogdu/matrixmigrate/internal/logger"
	"github.com/aligundogdu/matrixmigrate/internal/mattermost"
)

// HistoryMembership is a room membership the import created only so that a past author
// could be impersonated while their messages were replayed. It is recorded so the caller
// can undo it afterwards - see LeaveHistoryMemberships.
type HistoryMembership struct {
	RoomID string
	UserID string
}

// roomMemberIDs lists a room's joined members through the Synapse admin API.
//
// The client already has joinedMemberIDs, but that asks as a user who must themselves be in
// the room - which is exactly what is unknown here. The admin route needs no membership.
func (c *Client) roomMemberIDs(roomID string) ([]string, error) {
	endpoint := fmt.Sprintf("/_synapse/admin/v1/rooms/%s/members", url.PathEscape(roomID))
	body, statusCode, err := c.doRequest("GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Members []string `json:"members"`
		Errcode string   `json:"errcode"`
		Error   string   `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}
	return resp.Members, nil
}

// ensureHistoryAuthorsJoined joins every past author of a room's messages who is not
// currently a member of it.
//
// Membership import works from the *current* channel membership, but history is written by
// whoever was there at the time. Anyone who has since left the channel is therefore absent
// from the room, and the application service cannot send as a non-member: the homeserver
// answers M_FORBIDDEN and the message is lost. On this deployment that accounted for 4937 of
// 5703 failed sends, spread over 98 channels.
//
// The joins are deliberately recorded rather than left implicit, because being re-added to a
// conversation you left is a visible change to the account - see LeaveHistoryMemberships for
// the other half.
func (i *Importer) ensureHistoryAuthorsJoined(
	posts []mattermost.Post,
	channelToRoom map[string]string,
	userMapping map[string]string,
) []HistoryMembership {
	if !i.client.HasAdminToken() {
		logger.Warn("No admin token: cannot join past authors to their rooms; messages from anyone who left a channel will fail with M_FORBIDDEN")
		return nil
	}

	// Distinct authors per room. System messages are skipped for the same reason the import
	// skips them: they are never replayed, so their sender needs no membership.
	authorsByRoom := make(map[string]map[string]struct{})
	for idx := range posts {
		post := &posts[idx]
		if post.IsSystemMessage() {
			continue
		}
		roomID, ok := channelToRoom[post.ChannelID]
		if !ok {
			continue
		}
		mxid, mapped := userMapping[post.UserID]
		if !mapped || mxid == "" {
			continue
		}
		if _, seen := authorsByRoom[roomID]; !seen {
			authorsByRoom[roomID] = make(map[string]struct{})
		}
		authorsByRoom[roomID][mxid] = struct{}{}
	}

	rooms := make([]string, 0, len(authorsByRoom))
	for roomID := range authorsByRoom {
		rooms = append(rooms, roomID)
	}
	sort.Strings(rooms)

	var joined []HistoryMembership
	failed := 0
	for _, roomID := range rooms {
		current, err := i.client.roomMemberIDs(roomID)
		if err != nil {
			// Better to attempt the joins than to skip the room: ForceJoinUser is idempotent,
			// so the cost of a wrong guess is one redundant call per author.
			logger.Warn("Could not list members of room %s (%v); joining all past authors instead", roomID, err)
			current = nil
		}
		member := make(map[string]struct{}, len(current))
		for _, id := range current {
			member[id] = struct{}{}
		}

		missing := make([]string, 0)
		for mxid := range authorsByRoom[roomID] {
			if _, ok := member[mxid]; !ok {
				missing = append(missing, mxid)
			}
		}
		if len(missing) == 0 {
			continue
		}
		sort.Strings(missing)

		if err := i.client.ensureAdminInRoom(roomID); err != nil {
			logger.Warn("Could not put the admin in room %s (%v); %d past author(s) will fail to send", roomID, err, len(missing))
			failed += len(missing)
			continue
		}
		for _, mxid := range missing {
			if err := i.client.ForceJoinUser(roomID, mxid); err != nil {
				logger.Warn("Could not join past author %s to room %s: %v", mxid, roomID, err)
				failed++
				continue
			}
			joined = append(joined, HistoryMembership{RoomID: roomID, UserID: mxid})
		}
	}

	if len(joined) > 0 || failed > 0 {
		logger.Info("Past-author membership: joined %d (user,room) pair(s), %d could not be joined", len(joined), failed)
	}
	return joined
}
