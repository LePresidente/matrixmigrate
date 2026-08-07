package matrix

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newFakeMASWithEmails is newFakeMAS plus the user-emails route, answering with emailStatus
// and recording the request bodies it received.
func newFakeMASWithEmails(t *testing.T, emailStatus int) (*httptest.Server, *[]map[string]string) {
	t.Helper()
	var addEmailCalls []map[string]string

	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "test-token", "token_type": "Bearer", "expires_in": 300,
		})
	})
	mux.HandleFunc("/api/admin/v1/users", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.api+json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"type": "user", "id": "01ABCDEF",
				"attributes": map[string]any{"username": "alice"},
			},
		})
	})
	mux.HandleFunc("/api/admin/v1/user-emails", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var call map[string]string
		json.Unmarshal(body, &call)
		addEmailCalls = append(addEmailCalls, call)

		if emailStatus == http.StatusNotFound {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(emailStatus)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &addEmailCalls
}

func TestMASCreateUserAttachesEmail(t *testing.T) {
	srv, calls := newFakeMASWithEmails(t, http.StatusCreated)
	c := NewMASClient(srv.URL, "id", "secret", "example.com")

	if _, err := c.CreateUser("alice", &CreateUserRequest{Email: "alice@example.com"}); err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}

	if len(*calls) != 1 {
		t.Fatalf("expected exactly one add-email call, got %d", len(*calls))
	}
	// MAS identifies users by its own ULID, not by localpart.
	if got := (*calls)[0]["user_id"]; got != "01ABCDEF" {
		t.Errorf("user_id = %q, want the MAS ULID 01ABCDEF", got)
	}
	if got := (*calls)[0]["email"]; got != "alice@example.com" {
		t.Errorf("email = %q, want alice@example.com", got)
	}
}

func TestMASCreateUserSkipsEmailWhenNoneGiven(t *testing.T) {
	srv, calls := newFakeMASWithEmails(t, http.StatusCreated)
	c := NewMASClient(srv.URL, "id", "secret", "example.com")

	if _, err := c.CreateUser("alice", &CreateUserRequest{}); err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("expected no add-email call for a user without an address, got %d", len(*calls))
	}
}

func TestMASCreateUserSurvivesMissingEmailRoute(t *testing.T) {
	// The route arrived in MAS 0.15.0. On an older MAS the account must still be created and
	// mapped: the address notifications depend on lives in Synapse, not here.
	srv, calls := newFakeMASWithEmails(t, http.StatusNotFound)
	c := NewMASClient(srv.URL, "id", "secret", "example.com")

	resp, err := c.CreateUser("alice", &CreateUserRequest{Email: "alice@example.com"})
	if err != nil {
		t.Fatalf("a missing user-emails route must not fail the import: %v", err)
	}
	if resp.UserID != "@alice:example.com" {
		t.Fatalf("UserID = %q", resp.UserID)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected one attempt, got %d", len(*calls))
	}
}

func TestMASAddEmailTreatsConflictAsDone(t *testing.T) {
	// Re-running the import over an account that already carries the address.
	srv, _ := newFakeMASWithEmails(t, http.StatusConflict)
	c := NewMASClient(srv.URL, "id", "secret", "example.com")

	if err := c.AddEmail("01ABCDEF", "alice@example.com"); err != nil {
		t.Fatalf("an address that is already attached is not an error: %v", err)
	}
}

func TestMASAddEmailReportsRealFailures(t *testing.T) {
	srv, _ := newFakeMASWithEmails(t, http.StatusInternalServerError)
	c := NewMASClient(srv.URL, "id", "secret", "example.com")

	if err := c.AddEmail("01ABCDEF", "alice@example.com"); err == nil {
		t.Fatal("a 500 must be reported, not swallowed")
	}
}
