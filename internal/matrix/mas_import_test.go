package matrix

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aligundogdu/matrixmigrate/internal/mattermost"
)

// masImportFixture is an importer whose client uses MAS for accounts. The MAS fake answers the
// by-username lookup with lookupStatus and user creation with createStatus/createBody; the
// Synapse fake reports alice with a complete profile and counts every PUT to the admin user
// endpoint (a PUT there replaces the display name and the whole threepid list).
type masImportFixture struct {
	mu          sync.Mutex
	masCreates  int
	synapsePuts int
}

func newMASImportFixture(t *testing.T, lookupStatus, createStatus int, createBody string) (*Importer, *masImportFixture) {
	t.Helper()
	f := &masImportFixture{}

	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "test-token", "token_type": "Bearer", "expires_in": 300})
	})
	mux.HandleFunc("/api/admin/v1/users/by-username/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(lookupStatus)
		_, _ = w.Write([]byte(`{"errors":[{"title":"lookup"}]}`))
	})
	mux.HandleFunc("/api/admin/v1/users", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.masCreates++
		f.mu.Unlock()
		w.WriteHeader(createStatus)
		_, _ = w.Write([]byte(createBody))
	})
	mas := httptest.NewServer(mux)
	t.Cleanup(mas.Close)

	synapse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/_synapse/admin/v2/users/"):
			f.mu.Lock()
			f.synapsePuts++
			f.mu.Unlock()
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/_synapse/admin/v2/users/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name":        "@alice:example.com",
				"displayname": "Alice Example",
				"threepids":   []map[string]string{{"medium": "email", "address": "alice@example.com"}},
			})
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(synapse.Close)

	c := NewClientWithRateLimit(synapse.URL, "admin-token", "example.com", RateLimitConfig{})
	c.SetMASClient(NewMASClient(mas.URL, "id", "secret", "example.com"))
	return NewImporter(c), f
}

var masAlice = mattermost.User{ID: "u1", Username: "alice", FirstName: "Alice", LastName: "Example", Email: "alice@example.com"}

func TestImportUsersWithMASUnconfirmedExistenceDoesNotCreate(t *testing.T) {
	imp, f := newMASImportFixture(t, http.StatusInternalServerError, http.StatusCreated, `{}`)

	mapping, stats, err := imp.ImportUsers([]mattermost.User{masAlice}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.masCreates != 0 {
		t.Errorf("%d MAS create requests, want none for an account whose existence is unknown", f.masCreates)
	}
	if stats.UsersFailed != 1 || stats.UsersCreated != 0 || stats.UsersUnconfirmed != 1 {
		t.Errorf("stats = %+v, want 1 failed, 1 unconfirmed", stats)
	}
	if _, ok := mapping["u1"]; ok {
		t.Error("an unconfirmed user must not be mapped")
	}
}

func TestImportUsersWithMASConflictAdoptsExistingAccount(t *testing.T) {
	imp, f := newMASImportFixture(t, http.StatusNotFound, http.StatusConflict, `{"errors":[{"title":"User already exists"}]}`)

	mapping, stats, err := imp.ImportUsers([]mattermost.User{masAlice}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.masCreates != 1 {
		t.Fatalf("%d MAS create requests, want 1", f.masCreates)
	}
	if f.synapsePuts != 0 {
		t.Errorf("%d PUTs to the Synapse user endpoint, want none: the account already has its profile", f.synapsePuts)
	}
	if creds := imp.GeneratedCredentials(); len(creds) != 0 {
		t.Errorf("credentials = %v, want none: no password was set on an existing account", creds)
	}
	if mapping["u1"] != "@alice:example.com" {
		t.Errorf("mapping = %v, want u1 -> @alice:example.com", mapping)
	}
	if stats.UsersSkipped != 1 || stats.UsersCreated != 0 || stats.UsersFailed != 0 {
		t.Errorf("stats = %+v, want 1 skipped", stats)
	}
}

func TestMASCreateUserReportsExistingAccount(t *testing.T) {
	for _, tc := range []struct {
		status int
		title  string
	}{
		{http.StatusConflict, "User already exists"},
		{http.StatusUnprocessableEntity, "Username is reserved"},
	} {
		mux := http.NewServeMux()
		mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 300})
		})
		mux.HandleFunc("/api/admin/v1/users", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"title": tc.title}}})
		})
		srv := httptest.NewServer(mux)
		resp, err := NewMASClient(srv.URL, "id", "secret", "example.com").CreateUser("alice", &CreateUserRequest{Password: "pw"})
		srv.Close()
		if !errors.Is(err, ErrUserAlreadyExists) || resp != nil {
			t.Errorf("status %d %q: resp = %v err = %v, want an already-exists error", tc.status, tc.title, resp, err)
		}
	}
}
