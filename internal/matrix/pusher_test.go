package matrix

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/aligundogdu/matrixmigrate/internal/mattermost"
)

// newFakePusherAPI records what /pushers/set is asked to create, and answers with the given
// status and body.
func newFakePusherAPI(t *testing.T, status int, errBody string) (*httptest.Server, *map[string]any, *url.Values) {
	t.Helper()
	var content map[string]any
	var query url.Values

	mux := http.NewServeMux()
	mux.HandleFunc("/_matrix/client/v3/pushers/set", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &content)
		query = r.URL.Query()
		if status != http.StatusOK {
			w.WriteHeader(status)
			w.Write([]byte(errBody))
			return
		}
		w.Write([]byte(`{}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &content, &query
}

func TestSetEmailPusherRegistersAsTheUser(t *testing.T) {
	srv, content, query := newFakePusherAPI(t, http.StatusOK, "")
	c := NewClient(srv.URL, "admin-token", "example.com")
	c.SetASToken("as-token")

	if err := c.SetEmailPusher("@bob_dev:example.com", "bob@example.com"); err != nil {
		t.Fatalf("SetEmailPusher returned error: %v", err)
	}

	// The pusher has to belong to the user, not to the admin doing the migration.
	if got := query.Get("user_id"); got != "@bob_dev:example.com" {
		t.Errorf("user_id = %q, want @bob_dev:example.com", got)
	}
	if got := (*content)["kind"]; got != "email" {
		t.Errorf("kind = %v, want email", got)
	}
	if got := (*content)["app_id"]; got != "m.email" {
		t.Errorf("app_id = %v, want m.email", got)
	}
	// Synapse matches the pushkey against the user's own third-party IDs, so it has to be
	// the address itself.
	if got := (*content)["pushkey"]; got != "bob@example.com" {
		t.Errorf("pushkey = %v, want bob@example.com", got)
	}
}

func TestSetEmailPusherRequiresAppService(t *testing.T) {
	// Without the AS token the pusher would be registered for the admin account instead of
	// the user, which is worse than failing.
	srv, _, _ := newFakePusherAPI(t, http.StatusOK, "")
	c := NewClient(srv.URL, "admin-token", "example.com")

	if err := c.SetEmailPusher("@bob_dev:example.com", "bob@example.com"); err == nil {
		t.Fatal("expected an error when no Application Service token is set")
	}
}

func TestEnableEmailNotificationsClassifiesEveryOutcome(t *testing.T) {
	srv, _, _ := newFakePusherAPI(t, http.StatusOK, "")
	c := NewClient(srv.URL, "admin-token", "example.com")
	c.SetASToken("as-token")
	importer := NewImporter(c)

	users := []mattermost.User{
		{ID: "mm-alice", Username: "alice", Email: "alice@example.com"},
		{ID: "mm-bob", Username: "bob_dev", Email: ""},                            // no address
		{ID: "mm-carol", Username: "carol", Email: "carol@example.com"},           // not mapped
		{ID: "mm-dave", Username: "dave", Email: "dave@example.com", DeleteAt: 1}, // deactivated
	}
	userMapping := map[string]string{
		"mm-alice": "@alice:example.com",
		"mm-bob":   "@bob_dev:example.com",
		"mm-dave":  "@dave:example.com",
	}

	stats, err := importer.EnableEmailNotifications(users, userMapping, nil)
	if err != nil {
		t.Fatalf("EnableEmailNotifications returned error: %v", err)
	}

	if stats.UsersCreated != 1 {
		t.Errorf("enabled = %d, want 1 (only alice qualifies)", stats.UsersCreated)
	}
	if stats.UsersSkipped != 3 {
		t.Errorf("skipped = %d, want 3 (no address, not mapped, deactivated)", stats.UsersSkipped)
	}
	if stats.UsersFailed != 0 {
		t.Errorf("failed = %d, want 0", stats.UsersFailed)
	}
}

func TestEnableEmailNotificationsNeedsAppService(t *testing.T) {
	srv, _, _ := newFakePusherAPI(t, http.StatusOK, "")
	c := NewClient(srv.URL, "admin-token", "example.com")
	importer := NewImporter(c)

	_, err := importer.EnableEmailNotifications(
		[]mattermost.User{{ID: "mm-alice", Username: "alice", Email: "alice@example.com"}},
		map[string]string{"mm-alice": "@alice:example.com"}, nil)
	if err == nil {
		t.Fatal("expected the run to refuse to start without an Application Service token")
	}
}

func TestEnableEmailNotificationsReportsMissingThreepid(t *testing.T) {
	// Synapse refuses a pusher whose pushkey is not one of the user's own addresses. That is
	// the account's profile being incomplete, not a broken pusher call, and the count has to
	// say so or the operator has nothing to act on.
	srv, _, _ := newFakePusherAPI(t, http.StatusBadRequest,
		`{"errcode":"M_THREEPID_NOT_FOUND","error":"Email not found"}`)
	c := NewClient(srv.URL, "admin-token", "example.com")
	c.SetASToken("as-token")
	importer := NewImporter(c)

	stats, err := importer.EnableEmailNotifications(
		[]mattermost.User{{ID: "mm-alice", Username: "alice", Email: "alice@example.com"}},
		map[string]string{"mm-alice": "@alice:example.com"}, nil)
	if err != nil {
		t.Fatalf("a per-user failure must not abort the run: %v", err)
	}
	if stats.UsersFailed != 1 {
		t.Errorf("failed = %d, want 1", stats.UsersFailed)
	}
	if stats.UsersCreated != 0 {
		t.Errorf("enabled = %d, want 0", stats.UsersCreated)
	}
}

func TestEmailNotificationsDisabledHint(t *testing.T) {
	// Synapse only registers the "email" pusher type when email.enable_notifs is set, so
	// without a server-side email config every call fails identically. That is one omission,
	// not hundreds of broken accounts, and the run should say so.
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"unknown pusher type", errorString("API error (400): M_UNKNOWN - Unknown pusher type"), true},
		{"threepid missing is not this", errorString("API error (400): M_THREEPID_NOT_FOUND - Email not found"), false},
		{"nil", nil, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := emailNotificationsDisabledHint(tt.err); got != tt.want {
				t.Errorf("emailNotificationsDisabledHint() = %v, want %v", got, tt.want)
			}
		})
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }
