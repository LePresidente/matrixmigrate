package matrix

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newFakeMAS stands in for the MAS admin API, answering with the status codes the real
// service uses: 201 for creating a user, and setPasswordStatus for set-password.
func newFakeMAS(t *testing.T, setPasswordStatus int) (*httptest.Server, *int) {
	t.Helper()
	setPasswordCalls := 0

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
	mux.HandleFunc("/api/admin/v1/users/01ABCDEF/set-password", func(w http.ResponseWriter, r *http.Request) {
		setPasswordCalls++
		if setPasswordStatus == http.StatusForbidden {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"errors":[{"title":"Password auth is disabled"}]}`))
			return
		}
		w.WriteHeader(setPasswordStatus)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &setPasswordCalls
}

func TestMASCreateUserAcceptsNoContentFromSetPassword(t *testing.T) {
	// The regression: MAS answers 204 for set-password, the client compared against 200 only,
	// and every user with a password produced "MAS set-password failed ... status 204" even
	// though the password had been set.
	for _, status := range []int{http.StatusOK, http.StatusNoContent, http.StatusCreated} {
		srv, calls := newFakeMAS(t, status)
		c := NewMASClient(srv.URL, "id", "secret", "example.com")

		resp, err := c.CreateUser("alice", &CreateUserRequest{Password: "a-test-password-24ch"})
		if err != nil {
			t.Fatalf("set-password status %d: CreateUser returned error: %v", status, err)
		}
		if resp.UserID != "@alice:example.com" {
			t.Fatalf("set-password status %d: UserID = %q", status, resp.UserID)
		}
		if *calls != 1 {
			t.Fatalf("set-password status %d: expected exactly one set-password call, got %d", status, *calls)
		}
	}
}

func TestMASCreateUserSurvivesPasswordAuthDisabled(t *testing.T) {
	// A 403 means the account cannot use a password, but the user itself was created and the
	// import must carry on and map them.
	srv, calls := newFakeMAS(t, http.StatusForbidden)
	c := NewMASClient(srv.URL, "id", "secret", "example.com")

	resp, err := c.CreateUser("alice", &CreateUserRequest{Password: "a-test-password-24ch"})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if resp.UserID != "@alice:example.com" {
		t.Fatalf("UserID = %q", resp.UserID)
	}
	if *calls != 1 {
		t.Fatalf("expected one set-password call, got %d", *calls)
	}
}

func TestMASCreateUserSkipsSetPasswordWhenNoPassword(t *testing.T) {
	// user_password.mode none/local_only leaves Password empty for SSO accounts; there is
	// nothing to set and MAS must not be called.
	srv, calls := newFakeMAS(t, http.StatusNoContent)
	c := NewMASClient(srv.URL, "id", "secret", "example.com")

	if _, err := c.CreateUser("alice", &CreateUserRequest{}); err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("expected no set-password call, got %d", *calls)
	}
}
