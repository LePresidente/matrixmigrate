package matrix

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newFakeUserAdminAPI serves the Synapse admin user endpoint with the given current profile
// and records the body of any PUT it receives. sawPut stays false when no write happened.
func newFakeUserAdminAPI(t *testing.T, current UserResponse) (*httptest.Server, *map[string]any, *bool) {
	t.Helper()
	var sent map[string]any
	sawPut := false

	mux := http.NewServeMux()
	mux.HandleFunc("/_synapse/admin/v2/users/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			sawPut = true
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &sent)
			w.Write([]byte(`{}`))
			return
		}
		json.NewEncoder(w).Encode(current)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &sent, &sawPut
}

// threepidAddresses pulls the addresses out of a recorded threepids payload.
func threepidAddresses(t *testing.T, sent map[string]any) []string {
	t.Helper()

	raw, ok := sent["threepids"].([]any)
	if !ok {
		t.Fatalf("threepids missing or not a list: %#v", sent)
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		tp, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("threepid entry is not an object: %#v", item)
		}
		out = append(out, tp["address"].(string))
	}
	return out
}

func TestEnsureUserProfileAddsMissingEmail(t *testing.T) {
	// The case this exists for: an account that signed in through SSO before the migration.
	// MAS gave it a display name from the GitLab claims, but it has no email address, and so
	// no email notifications.
	srv, sent, sawPut := newFakeUserAdminAPI(t, UserResponse{
		Name:        "@bob_dev:example.com",
		DisplayName: "Bob Beispiel",
	})
	c := NewClient(srv.URL, "admin-token", "example.com")

	if err := c.EnsureUserProfile("@bob_dev:example.com", "Bob Beispiel", "bob@example.com"); err != nil {
		t.Fatalf("EnsureUserProfile returned error: %v", err)
	}
	if !*sawPut {
		t.Fatal("expected a PUT: the account has no email address")
	}

	if got := threepidAddresses(t, *sent); len(got) != 1 || got[0] != "bob@example.com" {
		t.Errorf("threepids = %v, want [bob@example.com]", got)
	}
	if _, overwritten := (*sent)["displayname"]; overwritten {
		t.Errorf("display name was already set and must not be rewritten: %#v", *sent)
	}
}

func TestEnsureUserProfileKeepsExistingThreepids(t *testing.T) {
	// The admin API's threepids parameter replaces the whole list, so a blind write would
	// delete the address this person added themselves.
	srv, sent, _ := newFakeUserAdminAPI(t, UserResponse{
		Name:        "@bob_dev:example.com",
		DisplayName: "Bob Beispiel",
		Threepids:   []Threepid{{Medium: "email", Address: "privat@example.com"}},
	})
	c := NewClient(srv.URL, "admin-token", "example.com")

	if err := c.EnsureUserProfile("@bob_dev:example.com", "Bob Beispiel", "bob@example.com"); err != nil {
		t.Fatalf("EnsureUserProfile returned error: %v", err)
	}

	got := threepidAddresses(t, *sent)
	if len(got) != 2 || got[0] != "privat@example.com" || got[1] != "bob@example.com" {
		t.Errorf("threepids = %v, want [privat@example.com bob@example.com]", got)
	}
}

func TestEnsureUserProfileSetsDisplayNameOnlyWhenEmpty(t *testing.T) {
	srv, sent, _ := newFakeUserAdminAPI(t, UserResponse{Name: "@bob_dev:example.com"})
	c := NewClient(srv.URL, "admin-token", "example.com")

	if err := c.EnsureUserProfile("@bob_dev:example.com", "Bob Beispiel", "bob@example.com"); err != nil {
		t.Fatalf("EnsureUserProfile returned error: %v", err)
	}
	if (*sent)["displayname"] != "Bob Beispiel" {
		t.Errorf("displayname = %v, want it filled in on an account that had none", (*sent)["displayname"])
	}
}

func TestEnsureUserProfileWritesNothingWhenComplete(t *testing.T) {
	srv, _, sawPut := newFakeUserAdminAPI(t, UserResponse{
		Name:        "@bob_dev:example.com",
		DisplayName: "Bob Beispiel",
		Threepids:   []Threepid{{Medium: "email", Address: "bob@example.com"}},
	})
	c := NewClient(srv.URL, "admin-token", "example.com")

	if err := c.EnsureUserProfile("@bob_dev:example.com", "Bob Beispiel", "bob@example.com"); err != nil {
		t.Fatalf("EnsureUserProfile returned error: %v", err)
	}
	if *sawPut {
		t.Fatal("nothing was missing, so the account must not be written to at all")
	}
}

func TestEnsureUserProfileMatchesAddressCaseInsensitively(t *testing.T) {
	// Synapse canonicalises a pusher's pushkey but stores the threepid as given, so the two
	// can differ only in case. Treating them as different addresses would append a duplicate.
	srv, _, sawPut := newFakeUserAdminAPI(t, UserResponse{
		Name:        "@bob_dev:example.com",
		DisplayName: "Bob Beispiel",
		Threepids:   []Threepid{{Medium: "email", Address: "bob@example.com"}},
	})
	c := NewClient(srv.URL, "admin-token", "example.com")

	if err := c.EnsureUserProfile("@bob_dev:example.com", "Bob Beispiel", "Bob@Example.com"); err != nil {
		t.Fatalf("EnsureUserProfile returned error: %v", err)
	}
	if *sawPut {
		t.Fatal("the address is already present, only spelled differently")
	}
}

func TestEnsureUserProfileIgnoresNonEmailThreepids(t *testing.T) {
	srv, sent, _ := newFakeUserAdminAPI(t, UserResponse{
		Name:        "@bob_dev:example.com",
		DisplayName: "Bob Beispiel",
		Threepids:   []Threepid{{Medium: "msisdn", Address: "bob@example.com"}},
	})
	c := NewClient(srv.URL, "admin-token", "example.com")

	if err := c.EnsureUserProfile("@bob_dev:example.com", "Bob Beispiel", "bob@example.com"); err != nil {
		t.Fatalf("EnsureUserProfile returned error: %v", err)
	}

	// The phone number must survive, and the email must be added rather than considered present.
	got := threepidAddresses(t, *sent)
	if len(got) != 2 {
		t.Fatalf("threepids = %v, want the msisdn kept and the email added", got)
	}
}
