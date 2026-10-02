package matrix

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aligundogdu/matrixmigrate/internal/mattermost"
)

type assetSnapshot struct {
	users, spaces, rooms map[string]string
}

// An interrupted run must be able to resume from what was already created, so the importer
// reports its mappings after the users and after each space and room.
func TestImportAssetsCheckpoints(t *testing.T) {
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/createRoom"):
			mu.Lock()
			n++
			id := n
			mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"room_id":"!r%d:example.com"}`, id)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/_synapse/admin/v2/users/"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errcode":"M_NOT_FOUND","error":"User not found"}`))
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/_synapse/admin/v2/users/"):
			_, _ = w.Write([]byte(`{"name":"@alice:example.com"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)

	imp := NewImporter(NewClient(srv.URL, "admin-token", "example.com"))
	var snaps []assetSnapshot
	imp.SetAssetCheckpoint(func(users, spaces, rooms map[string]string) {
		snaps = append(snaps, assetSnapshot{users, spaces, rooms})
	})

	assets := &mattermost.Assets{
		Users:    []mattermost.User{{ID: "u1", Username: "alice"}},
		Teams:    []mattermost.Team{{ID: "t1", Name: "team", DisplayName: "Team"}},
		Channels: []mattermost.Channel{{ID: "c1", Name: "one", DisplayName: "One", Type: "O"}, {ID: "c2", Name: "two", DisplayName: "Two", Type: "O"}},
	}
	if _, err := imp.ImportAssets(assets, nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	// users, 1 space, 2 rooms
	if len(snaps) != 4 {
		t.Fatalf("checkpoint called %d times, want 4", len(snaps))
	}
	if snaps[0].users["u1"] != "@alice:example.com" || len(snaps[0].spaces) != 0 {
		t.Errorf("after users: %+v", snaps[0])
	}
	if len(snaps[1].spaces) != 1 || len(snaps[1].users) != 1 {
		t.Errorf("after space: %+v", snaps[1])
	}
	if len(snaps[2].rooms) != 1 || len(snaps[3].rooms) != 2 {
		t.Errorf("after rooms: %+v / %+v", snaps[2], snaps[3])
	}
	// Snapshots are copies: later progress must not leak into an earlier one.
	if len(snaps[2].rooms) != 1 {
		t.Errorf("snapshot mutated after the fact: %+v", snaps[2].rooms)
	}
}

func TestImportDirectChannelsCheckpointIncludesRegularRooms(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/createRoom") {
			_, _ = w.Write([]byte(`{"room_id":"!dm:example.com"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	imp := NewImporter(NewClient(srv.URL, "admin-token", "example.com"))
	var last assetSnapshot
	calls := 0
	imp.SetAssetCheckpoint(func(users, spaces, rooms map[string]string) {
		calls++
		last = assetSnapshot{users, spaces, rooms}
	})
	imp.assetRooms = map[string]string{"c1": "!room:example.com"}

	users := map[string]string{"ua": "@alice:example.com", "ub": "@bob_dev:example.com"}
	dm := mattermost.Channel{ID: "d1", Type: "D", Name: "ua__ub", CreatorID: "ua"}
	if _, _, err := imp.ImportDirectChannelsAsDMs([]mattermost.Channel{dm}, nil, users, map[string]string{"c1": "!room:example.com"}, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("checkpoint called %d times, want 1", calls)
	}
	if last.rooms["d1"] != "!dm:example.com" || last.rooms["c1"] != "!room:example.com" {
		t.Errorf("DM checkpoint rooms = %v, want the DM and the regular room", last.rooms)
	}
}

func TestImportUsersUnconfirmedExistenceDoesNotCreate(t *testing.T) {
	puts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/_synapse/admin/v2/users/"):
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errcode":"M_FORBIDDEN","error":"nope"}`))
		case r.Method == http.MethodPut:
			puts++
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)

	imp := NewImporter(NewClient(srv.URL, "admin-token", "example.com"))
	mapping, stats, err := imp.ImportUsers([]mattermost.User{{ID: "u1", Username: "alice"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if puts != 0 {
		t.Errorf("%d PUTs to the user endpoint, want none: an upsert would overwrite a real account", puts)
	}
	if stats.UsersFailed != 1 || stats.UsersCreated != 0 {
		t.Errorf("stats = %+v, want 1 failed", stats)
	}
	if _, ok := mapping["u1"]; ok {
		t.Error("an unconfirmed user must not be mapped")
	}
}

func TestInviteUserForbidden(t *testing.T) {
	for _, tc := range []struct {
		name    string
		msg     string
		wantErr bool
	}{
		{"already in room", "@bob_dev:example.com is already in the room.", false},
		{"not in room", "You don't have permission to invite users", true},
		{"other", "User is banned from this room", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = fmt.Fprintf(w, `{"errcode":"M_FORBIDDEN","error":%q}`, tc.msg)
			}))
			t.Cleanup(srv.Close)
			err := NewClient(srv.URL, "admin-token", "example.com").InviteUser("!r:example.com", "@bob_dev:example.com")
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestSetDirectRoomForUserReadFailureSendsNoPut(t *testing.T) {
	puts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts++
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN","error":"boom"}`))
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "admin-token", "example.com")
	c.SetASToken("as-token")
	if err := c.setDirectRoomForUser("@alice:example.com", "@bob_dev:example.com", "!dm:example.com"); err == nil {
		t.Error("expected the failed read to be returned")
	}
	if puts != 0 {
		t.Errorf("%d PUTs, want none: writing would wipe the existing m.direct", puts)
	}
}

func TestSetDirectRoomForUserMissingAccountDataIsEmpty(t *testing.T) {
	puts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts++
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errcode":"M_NOT_FOUND","error":"no"}`))
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "admin-token", "example.com")
	c.SetASToken("as-token")
	if err := c.setDirectRoomForUser("@alice:example.com", "@bob_dev:example.com", "!dm:example.com"); err != nil {
		t.Fatal(err)
	}
	if puts != 1 {
		t.Errorf("%d PUTs, want 1", puts)
	}
}

// A re-run's checkpoints replace the newest mapping file, so none of them may drop anything the
// previous run recorded, including a room (such as a DM) that is not among the channels imported.
func TestImportAssetsCheckpointsKeepExistingMappings(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/createRoom"):
			n++
			_, _ = fmt.Fprintf(w, `{"room_id":"!new%d:example.com"}`, n)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/_synapse/admin/v2/users/"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errcode":"M_NOT_FOUND","error":"User not found"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)

	imp := NewImporter(NewClient(srv.URL, "admin-token", "example.com"))
	var snaps []assetSnapshot
	imp.SetAssetCheckpoint(func(users, spaces, rooms map[string]string) {
		snaps = append(snaps, assetSnapshot{users, spaces, rooms})
	})
	existing := &ExistingMappings{
		Users:  map[string]string{"u0": "@bob_dev:example.com"},
		Spaces: map[string]string{"t0": "!space:example.com"},
		Rooms:  map[string]string{"c0": "!room:example.com", "dm0": "!dm:example.com"},
	}
	assets := &mattermost.Assets{
		Users:    []mattermost.User{{ID: "u1", Username: "alice"}},
		Teams:    []mattermost.Team{{ID: "t0"}, {ID: "t1", Name: "team", DisplayName: "Team"}},
		Channels: []mattermost.Channel{{ID: "c0", DisplayName: "Zero", Type: "O"}, {ID: "c1", Name: "one", DisplayName: "One", Type: "O"}},
	}
	if _, err := imp.ImportAssets(assets, existing, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(snaps) < 3 {
		t.Fatalf("only %d checkpoints", len(snaps))
	}
	for idx, s := range snaps {
		if s.users["u0"] == "" || s.spaces["t0"] != "!space:example.com" || s.rooms["c0"] != "!room:example.com" || s.rooms["dm0"] != "!dm:example.com" {
			t.Errorf("checkpoint %d lost existing mappings: %+v", idx, s)
		}
	}
}
