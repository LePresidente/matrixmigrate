package matrix

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aligundogdu/matrixmigrate/internal/mattermost"
)

// deletedUserFixture is one active user and one whose Mattermost account is deleted, both
// members of the same public channel.
func deletedUserFixture() ([]mattermost.Channel, []mattermost.User, []mattermost.ChannelMember, map[string]string, map[string]string) {
	channels := []mattermost.Channel{{ID: "c1", Type: "O", DisplayName: "town-square"}}
	users := []mattermost.User{
		{ID: "u_alice", Username: "alice"},
		{ID: "u_dave", Username: "dave", DeleteAt: 1},
	}
	members := []mattermost.ChannelMember{
		{ChannelID: "c1", UserID: "u_alice"},
		{ChannelID: "c1", UserID: "u_dave"},
	}
	userMapping := map[string]string{
		"u_alice": "@alice:example.com",
		"u_dave":  "@dave:example.com",
	}
	roomMapping := map[string]string{"c1": "!room:example.com"}
	return channels, users, members, userMapping, roomMapping
}

func TestApplyChannelMembershipsSkipsDeletedUsersWhenDeactivated(t *testing.T) {
	srv, joins := fakeMembershipServer(t, map[string][]string{})
	c := NewClient(srv.URL, "admin-token", "example.com")
	c.SetForceJoin(true)
	i := NewImporter(c)

	channels, users, members, userMapping, roomMapping := deletedUserFixture()
	stats, err := i.ApplyChannelMemberships(channels, users, members, userMapping, roomMapping, "@admin:example.com", nil)
	if err != nil {
		t.Fatalf("ApplyChannelMemberships: %v", err)
	}

	for _, j := range *joins {
		if strings.HasPrefix(j, "@dave:example.com@") {
			t.Fatalf("a deactivated account must not be joined to a room: %v", *joins)
		}
	}
	if stats.MembersAdded != 1 {
		t.Errorf("MembersAdded = %d, want 1 (alice only)", stats.MembersAdded)
	}
	if stats.MembersSkipped != 1 {
		t.Errorf("MembersSkipped = %d, want 1 (dave)", stats.MembersSkipped)
	}
}

func TestApplyChannelMembershipsKeepsDeletedUsersWhenLocked(t *testing.T) {
	srv, joins := fakeMembershipServer(t, map[string][]string{})
	c := NewClient(srv.URL, "admin-token", "example.com")
	c.SetForceJoin(true)
	i := NewImporter(c)
	i.SetDeletedUserMode(DeletedUserModeLocked)

	channels, users, members, userMapping, roomMapping := deletedUserFixture()
	stats, err := i.ApplyChannelMemberships(channels, users, members, userMapping, roomMapping, "@admin:example.com", nil)
	if err != nil {
		t.Fatalf("ApplyChannelMemberships: %v", err)
	}

	// Locking keeps the account out of rooms it never left, which is the whole point of the mode.
	found := false
	for _, j := range *joins {
		if strings.HasPrefix(j, "@dave:example.com@") {
			found = true
		}
	}
	if !found {
		t.Fatalf("locked mode must keep deleted users in their rooms, joins were %v", *joins)
	}
	if stats.MembersAdded != 2 {
		t.Errorf("MembersAdded = %d, want 2", stats.MembersAdded)
	}
}

func TestApplyTeamMembershipsSkipsDeletedUsers(t *testing.T) {
	srv, joins := fakeMembershipServer(t, map[string][]string{})
	c := NewClient(srv.URL, "admin-token", "example.com")
	c.SetForceJoin(true)
	i := NewImporter(c)

	_, users, _, userMapping, _ := deletedUserFixture()
	teamMembers := []mattermost.TeamMember{
		{TeamID: "t1", UserID: "u_alice"},
		{TeamID: "t1", UserID: "u_dave"},
	}
	stats, _, err := i.ApplyTeamMemberships(teamMembers, users, userMapping,
		map[string]string{"t1": "!space:example.com"}, "@admin:example.com", nil)
	if err != nil {
		t.Fatalf("ApplyTeamMemberships: %v", err)
	}

	for _, j := range *joins {
		if strings.HasPrefix(j, "@dave:example.com@") {
			t.Fatalf("a deactivated account must not be joined to a space: %v", *joins)
		}
	}
	if stats.MembersAdded != 1 {
		t.Errorf("MembersAdded = %d, want 1 (alice only)", stats.MembersAdded)
	}
}

// fakeRemovalServer answers the admin joined-rooms and power-level lookups the removal sweep
// makes, and records every kick and leave-as-user it receives.
func fakeRemovalServer(t *testing.T, joinedRooms map[string][]string, powerLevels map[string]map[string]int) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	removals := []string{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasPrefix(path, "/_synapse/admin/v1/users/") && strings.HasSuffix(path, "/joined_rooms"):
			userID := strings.TrimSuffix(strings.TrimPrefix(path, "/_synapse/admin/v1/users/"), "/joined_rooms")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"joined_rooms": joinedRooms[userID]})
		case strings.Contains(path, "/state/m.room.power_levels"):
			roomID := strings.TrimSuffix(strings.TrimPrefix(path, "/_matrix/client/v3/rooms/"), "/state/m.room.power_levels/")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"users": powerLevels[roomID]})
		case strings.HasSuffix(path, "/kick"):
			var body struct {
				UserID string `json:"user_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			roomID := strings.TrimSuffix(strings.TrimPrefix(path, "/_matrix/client/v3/rooms/"), "/kick")
			mu.Lock()
			removals = append(removals, "kick "+body.UserID+"@"+roomID)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(path, "/leave"):
			roomID := strings.TrimSuffix(strings.TrimPrefix(path, "/_matrix/client/v3/rooms/"), "/leave")
			mu.Lock()
			removals = append(removals, "leave "+r.URL.Query().Get("user_id")+"@"+roomID)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(path, "/account/whoami"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"user_id":"@migratebot:example.com"}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &removals
}

func TestRemoveDeletedUsersFromRoomsRemovesOnlyDeletedAccounts(t *testing.T) {
	srv, removals := fakeRemovalServer(t,
		map[string][]string{
			"@dave:example.com":  {"!room:example.com"},
			"@alice:example.com": {"!room:example.com"},
		},
		map[string]map[string]int{"!room:example.com": {"@owner:example.com": 100}})
	c := NewClient(srv.URL, "admin-token", "example.com")
	i := NewImporter(c)

	_, users, _, userMapping, _ := deletedUserFixture()
	result := i.RemoveDeletedUsersFromRooms(users, userMapping, nil)

	if result.Accounts != 1 {
		t.Errorf("Accounts = %d, want 1 (dave only)", result.Accounts)
	}
	if result.Left != 1 {
		t.Errorf("Left = %d, want 1", result.Left)
	}
	if len(*removals) != 1 || !strings.Contains((*removals)[0], "@dave:example.com") {
		t.Fatalf("expected dave alone to be removed, got %v", *removals)
	}
	// Deactivated accounts cannot act for themselves, so the admin kick is the route used.
	if !strings.HasPrefix((*removals)[0], "kick ") {
		t.Errorf("expected an admin kick, got %q", (*removals)[0])
	}
}

func TestRemoveDeletedUsersFromRoomsKeepsRoomOwner(t *testing.T) {
	// dave is deleted in Mattermost but holds PL 100: removing him would leave the room
	// with nobody who can administer it.
	srv, removals := fakeRemovalServer(t,
		map[string][]string{"@dave:example.com": {"!room:example.com"}},
		map[string]map[string]int{"!room:example.com": {"@dave:example.com": 100}})
	c := NewClient(srv.URL, "admin-token", "example.com")
	i := NewImporter(c)

	_, users, _, userMapping, _ := deletedUserFixture()
	result := i.RemoveDeletedUsersFromRooms(users, userMapping, nil)

	if result.Kept != 1 || result.Left != 0 {
		t.Errorf("Kept=%d Left=%d, want Kept=1 Left=0", result.Kept, result.Left)
	}
	if len(*removals) != 0 {
		t.Fatalf("a room owner must not be removed, got %v", *removals)
	}
}

func TestRemoveDeletedUsersFromRoomsDoesNothingWhenLocked(t *testing.T) {
	srv, removals := fakeRemovalServer(t,
		map[string][]string{"@dave:example.com": {"!room:example.com"}},
		map[string]map[string]int{"!room:example.com": {}})
	c := NewClient(srv.URL, "admin-token", "example.com")
	i := NewImporter(c)
	i.SetDeletedUserMode(DeletedUserModeLocked)

	_, users, _, userMapping, _ := deletedUserFixture()
	result := i.RemoveDeletedUsersFromRooms(users, userMapping, nil)

	if result.Accounts != 0 || result.Left != 0 {
		t.Errorf("locked mode must not touch memberships, got %+v", result)
	}
	if len(*removals) != 0 {
		t.Fatalf("locked mode must not remove anyone, got %v", *removals)
	}
}

func TestRemoveASBotFromRoomsLeavesEveryRoomItJoined(t *testing.T) {
	srv, removals := fakeRemovalServer(t,
		map[string][]string{"@migratebot:example.com": {"!a:example.com", "!b:example.com"}},
		map[string]map[string]int{
			"!a:example.com": {"@owner:example.com": 100},
			"!b:example.com": {"@owner:example.com": 100},
		})
	c := NewClient(srv.URL, "admin-token", "example.com")
	c.SetASToken("as-token")
	i := NewImporter(c)

	result := i.RemoveASBotFromRooms(nil)

	if result.Left != 2 {
		t.Errorf("Left = %d, want 2", result.Left)
	}
	// The bot is a live account, so it leaves under its own name rather than being kicked.
	for _, r := range *removals {
		if !strings.HasPrefix(r, "leave @migratebot:example.com@") {
			t.Errorf("expected the bot to leave as itself, got %q", r)
		}
	}
}

func TestRemoveASBotFromRoomsKeepsRoomsItOwns(t *testing.T) {
	srv, removals := fakeRemovalServer(t,
		map[string][]string{"@migratebot:example.com": {"!a:example.com"}},
		map[string]map[string]int{"!a:example.com": {"@migratebot:example.com": 100}})
	c := NewClient(srv.URL, "admin-token", "example.com")
	c.SetASToken("as-token")
	i := NewImporter(c)

	result := i.RemoveASBotFromRooms(nil)

	if result.Kept != 1 || result.Left != 0 {
		t.Errorf("Kept=%d Left=%d, want Kept=1 Left=0", result.Kept, result.Left)
	}
	if len(*removals) != 0 {
		t.Fatalf("the bot owns the room and must stay, got %v", *removals)
	}
}
