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

// fakeMembershipServer serves the admin member list and records force-joins.
func fakeMembershipServer(t *testing.T, members map[string][]string) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	joins := []string{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/members") && r.Method == http.MethodGet:
			roomID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/_synapse/admin/v1/rooms/"), "/members")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"members": members[roomID]})
		case strings.Contains(r.URL.Path, "/_synapse/admin/v1/join/"):
			var body struct {
				UserID string `json:"user_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			roomID := strings.TrimPrefix(r.URL.Path, "/_synapse/admin/v1/join/")
			mu.Lock()
			joins = append(joins, body.UserID+"@"+roomID)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			// Room joins by the admin itself, and anything else, succeed quietly.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"room_id":"!r:example.com"}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &joins
}

func TestEnsureHistoryAuthorsJoinedOnlyJoinsNonMembers(t *testing.T) {
	// !room already has alice; bob posted there before leaving, so only bob needs joining.
	srv, joins := fakeMembershipServer(t, map[string][]string{
		"!room": {"@alice:example.com"},
	})
	c := NewClient(srv.URL, "admin-token", "example.com")
	i := NewImporter(c)

	posts := []mattermost.Post{
		{ID: "p1", ChannelID: "c1", UserID: "u_alice", Message: "hi"},
		{ID: "p2", ChannelID: "c1", UserID: "u_bob", Message: "bye"},
	}
	got := i.ensureHistoryAuthorsJoined(posts,
		map[string]string{"c1": "!room"},
		map[string]string{"u_alice": "@alice:example.com", "u_bob": "@bob:example.com"})

	if len(got) != 1 || got[0].UserID != "@bob:example.com" || got[0].RoomID != "!room" {
		t.Fatalf("expected only bob joined to !room, got %#v", got)
	}
	for _, j := range *joins {
		if strings.HasPrefix(j, "@alice:example.com@") {
			t.Fatalf("alice was already a member and must not be re-joined: %v", *joins)
		}
	}
	if i.HistoryJoins() != nil && len(i.HistoryJoins()) != 0 {
		t.Fatalf("HistoryJoins should only be populated by the import entrypoints, got %#v", i.HistoryJoins())
	}
}

func TestEnsureHistoryAuthorsJoinedSkipsSystemMessagesAndUnmappedAuthors(t *testing.T) {
	srv, joins := fakeMembershipServer(t, map[string][]string{"!room": {}})
	c := NewClient(srv.URL, "admin-token", "example.com")
	i := NewImporter(c)

	posts := []mattermost.Post{
		// System messages are never replayed, so their sender needs no membership.
		{ID: "p1", ChannelID: "c1", UserID: "u_alice", Type: "system_join_channel"},
		// A deleted account has no Matrix user to join.
		{ID: "p2", ChannelID: "c1", UserID: "u_ghost", Message: "hello"},
	}
	got := i.ensureHistoryAuthorsJoined(posts,
		map[string]string{"c1": "!room"},
		map[string]string{"u_alice": "@alice:example.com"})

	if len(got) != 0 {
		t.Fatalf("expected no joins, got %#v", got)
	}
	if len(*joins) != 0 {
		t.Fatalf("expected no force-joins, got %v", *joins)
	}
}

func TestEnsureHistoryAuthorsJoinedWithoutAdminTokenIsANoop(t *testing.T) {
	// Without an admin token there is no force-join route; the pass must degrade rather
	// than spray failing requests.
	c := NewClient("http://127.0.0.1:1", "", "example.com")
	i := NewImporter(c)
	got := i.ensureHistoryAuthorsJoined(
		[]mattermost.Post{{ID: "p1", ChannelID: "c1", UserID: "u", Message: "x"}},
		map[string]string{"c1": "!room"},
		map[string]string{"u": "@u:example.com"})
	if got != nil {
		t.Fatalf("expected nil without an admin token, got %#v", got)
	}
}
