package matrix

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeLeaveServer serves power levels and records leaves as "user@room".
func fakeLeaveServer(t *testing.T, powerUsers map[string]map[string]int) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	left := []string{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/state/m.room.power_levels"):
			roomID := strings.TrimPrefix(path, "/_matrix/client/v3/rooms/")
			roomID = strings.Split(roomID, "/")[0]
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"users": powerUsers[roomID]})
		case strings.HasSuffix(path, "/leave"):
			roomID := strings.TrimSuffix(strings.TrimPrefix(path, "/_matrix/client/v3/rooms/"), "/leave")
			user := r.URL.Query().Get("user_id")
			if user == "" {
				user = "@admin:example.com"
			}
			mu.Lock()
			left = append(left, user+"@"+roomID)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case strings.Contains(path, "/account/whoami"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"user_id":"@admin:example.com"}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &left
}

func TestLeaveHistoryMembershipsKeepsForcedOwner(t *testing.T) {
	// !r1 has an admin installed as owner because the real creator was locked: PL 100.
	// That membership must survive; the ordinary past author's must not.
	srv, left := fakeLeaveServer(t, map[string]map[string]int{
		"!r1": {"@standin:example.com": RoomOwnerPowerLevel, "@past:example.com": 0},
	})
	c := NewClient(srv.URL, "admin-token", "example.com")
	c.SetASToken("as-token")
	i := NewImporter(c)
	i.historyJoins = []HistoryMembership{
		{RoomID: "!r1", UserID: "@past:example.com"},
		{RoomID: "!r1", UserID: "@standin:example.com"},
	}

	res := i.LeaveHistoryMemberships()
	if res.Left != 1 || res.Kept != 1 {
		t.Fatalf("expected 1 left and 1 kept, got left=%d kept=%d failed=%d", res.Left, res.Kept, res.Failed)
	}
	joinedLeaves := strings.Join(*left, ",")
	if !strings.Contains(joinedLeaves, "@past:example.com@!r1") {
		t.Fatalf("past author should have left: %v", *left)
	}
	if strings.Contains(joinedLeaves, "@standin:example.com") {
		t.Fatalf("forced owner must never leave: %v", *left)
	}
}

func TestLeaveHistoryMembershipsKeepsMembershipWhenOwnershipIsUnknown(t *testing.T) {
	// Power levels unreadable: leaving could strand a room with no administrator, so the
	// safe move is to keep the membership rather than assume it is disposable.
	mux := http.NewServeMux()
	var left []string
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/state/m.room.power_levels") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/leave") {
			left = append(left, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"user_id":"@admin:example.com"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL, "admin-token", "example.com")
	c.SetASToken("as-token")
	i := NewImporter(c)
	i.historyJoins = []HistoryMembership{{RoomID: "!r9", UserID: "@past:example.com"}}

	res := i.LeaveHistoryMemberships()
	if res.Kept != 1 || res.Left != 0 {
		t.Fatalf("expected the membership to be kept when ownership is unknown, got %+v", res)
	}
}

func TestLeaveHistoryMembershipsWithoutASTokenDoesNothing(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "admin-token", "example.com")
	i := NewImporter(c)
	i.historyJoins = []HistoryMembership{{RoomID: "!r1", UserID: "@past:example.com"}}
	res := i.LeaveHistoryMemberships()
	if res.Left != 0 || res.Skipped != 1 {
		t.Fatalf("expected a no-op without an AS token, got %+v", res)
	}
}
