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

// RoomOwnerPowerLevel is the power level preserve_owner_and_alias gives a room's owner.
// It is also how a forced owner is recognised after the fact: nothing else in a migrated
// room is granted 100.
const RoomOwnerPowerLevel = 100

// LeaveRoomsResult reports what the post-import withdrawal did.
type LeaveRoomsResult struct {
	Left    int // memberships withdrawn
	Kept    int // memberships deliberately preserved because the account owns the room
	Failed  int
	Skipped int // nothing to do (no AS token, no recorded joins)
}

// LeaveHistoryMemberships withdraws the memberships this migration created for its own
// convenience, and only those.
//
// Two kinds are undone: past authors joined so their messages could be replayed, and the
// migration's own admin account, which joins each room in order to operate on it. Matrix
// keeps events after their sender leaves, so the archive is unaffected by either.
//
// The exception is ownership. Where a channel's real creator was locked or no longer exists,
// room creation installs an admin account as the owner instead; that account holds
// RoomOwnerPowerLevel and is the only thing standing between the room and having nobody who
// can administer it. Those memberships are kept, whichever account they belong to.
func (i *Importer) LeaveHistoryMemberships() *LeaveRoomsResult {
	result := &LeaveRoomsResult{}

	if !i.client.HasASToken() {
		logger.Warn("No Application Service token: cannot withdraw memberships created for the import")
		result.Skipped = len(i.historyJoins)
		return result
	}

	// The admin's own memberships are withdrawn too, so resolve who that is.
	adminID := ""
	if me, err := i.client.WhoAmI(); err == nil && me != nil {
		adminID = me.UserID
	}

	type membership struct{ roomID, userID string }
	pending := make([]membership, 0, len(i.historyJoins))
	seen := make(map[membership]struct{})
	for _, hm := range i.historyJoins {
		m := membership{hm.RoomID, hm.UserID}
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		pending = append(pending, m)
	}
	if adminID != "" {
		for _, roomID := range i.client.AdminJoinedRooms() {
			m := membership{roomID, adminID}
			if _, dup := seen[m]; dup {
				continue
			}
			seen[m] = struct{}{}
			pending = append(pending, m)
		}
	}

	sort.Slice(pending, func(a, b int) bool {
		if pending[a].roomID != pending[b].roomID {
			return pending[a].roomID < pending[b].roomID
		}
		return pending[a].userID < pending[b].userID
	})

	// Power levels are per room and every membership in a room asks the same question, so
	// resolve each room once.
	ownerCache := make(map[string]map[string]int)
	unknownOwnership := make(map[string]bool)
	for _, m := range pending {
		levels, cached := ownerCache[m.roomID]
		if !cached {
			levels = make(map[string]int)
			content, err := i.client.getPowerLevels(m.roomID)
			if err != nil || content == nil {
				// Unknown ownership: keep every membership in this room. Leaving a room that
				// turns out to have no other administrator is not recoverable without a
				// server admin, so an unnecessary membership is the cheaper mistake.
				logger.Warn("Could not read power levels for room %s (%v); keeping memberships in place", m.roomID, err)
				unknownOwnership[m.roomID] = true
			} else {
				for user, level := range content.Users {
					levels[user] = level
				}
			}
			ownerCache[m.roomID] = levels
		}

		if unknownOwnership[m.roomID] {
			result.Kept++
			continue
		}

		if levels[m.userID] >= RoomOwnerPowerLevel {
			logger.Info("Keeping %s in room %s: holds power level %d, so it is the room's owner", m.userID, m.roomID, levels[m.userID])
			result.Kept++
			continue
		}

		var err error
		if m.userID == adminID {
			err = i.client.LeaveRoom(m.roomID)
		} else {
			err = i.client.LeaveRoomAsUser(m.roomID, m.userID)
		}
		if err != nil {
			logger.Warn("Could not remove %s from room %s: %v", m.userID, m.roomID, err)
			result.Failed++
			continue
		}
		result.Left++
	}

	logger.Info("Post-import membership cleanup: left %d, kept %d owner membership(s), %d failed",
		result.Left, result.Kept, result.Failed)
	return result
}

// ensureFallbackSenderInRoom joins the application service's own user to roomID.
//
// When a post's author has no Matrix user -- a Mattermost account deleted outright, so there
// was nobody to create -- the import sends the message as the AS bot instead. That account is
// subject to the same rule as any other sender: it cannot post to a room it is not in.
// Nothing else in the migration ever joins it, so on this deployment every such message
// failed, 766 of them.
//
// Rooms are remembered so a channel full of orphaned posts costs one join, not one per post.
func (i *Importer) ensureFallbackSenderInRoom(roomID string) error {
	if i.fallbackSenderRooms == nil {
		i.fallbackSenderRooms = make(map[string]struct{})
	}
	if _, done := i.fallbackSenderRooms[roomID]; done {
		return nil
	}

	botID, err := i.client.ASBotUserID()
	if err != nil {
		return err
	}
	if !i.client.HasAdminToken() {
		return fmt.Errorf("no admin token: cannot join the fallback sender %s to %s", botID, roomID)
	}
	if err := i.client.ensureAdminInRoom(roomID); err != nil {
		return fmt.Errorf("admin could not enter %s: %w", roomID, err)
	}
	if err := i.client.ForceJoinUser(roomID, botID); err != nil {
		return fmt.Errorf("could not join fallback sender %s to %s: %w", botID, roomID, err)
	}

	i.fallbackSenderRooms[roomID] = struct{}{}
	i.historyJoins = append(i.historyJoins, HistoryMembership{RoomID: roomID, UserID: botID})
	return nil
}
