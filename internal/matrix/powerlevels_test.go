package matrix

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreatorMustBeAbsentFromPowerLevels(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{"12", true},
		{"13", true},
		{"11", false},
		{"10", false},
		{"1", false},
		{" 12 ", true},
		// Unstable or unknown versions are left alone: leaving an entry in place is
		// recoverable, removing one the server refuses to let us remove is not.
		{"org.matrix.msc3787", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := creatorMustBeAbsentFromPowerLevels(tt.version); got != tt.want {
			t.Errorf("creatorMustBeAbsentFromPowerLevels(%q) = %v, want %v", tt.version, got, tt.want)
		}
	}
}

// newFakeSynapse serves the admin room-details endpoint with the given creator and room
// version, and records the m.room.power_levels content it is asked to send.
func newFakeSynapse(t *testing.T, creator, version string) (*httptest.Server, *PowerLevelsContent) {
	t.Helper()
	var sent PowerLevelsContent

	mux := http.NewServeMux()
	mux.HandleFunc("/_matrix/client/v3/account/whoami", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"user_id": "@matrix-admin:example.com"})
	})
	mux.HandleFunc("/_synapse/admin/v1/rooms/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"creator": creator, "version": version})
	})
	mux.HandleFunc("/_matrix/client/v3/rooms/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/state/m.room.power_levels") && r.Method == http.MethodPut {
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &sent)
			w.Write([]byte(`{"event_id":"$evt"}`))
			return
		}
		// getPowerLevels
		json.NewEncoder(w).Encode(PowerLevelsContent{Users: map[string]int{creator: 100}})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &sent
}

func TestSetPowerLevelsKeepsCreatorBeforeRoomVersion12(t *testing.T) {
	// The regression this guards: stripping the creator unconditionally turned a working
	// update into "M_FORBIDDEN - You don't have permission to remove ops level equal to your
	// own", because in these room versions the creator is an ordinary entry at 100 and
	// removing it is a privileged change.
	srv, sent := newFakeSynapse(t, "@daniel:example.com", "11")
	c := NewClient(srv.URL, "token", "example.com")

	if err := c.SetPowerLevels("!room:example.com", "@matrix-admin:example.com", 100); err != nil {
		t.Fatalf("SetPowerLevels returned error: %v", err)
	}
	if _, listed := sent.Users["@daniel:example.com"]; !listed {
		t.Fatalf("creator was removed from content.users in a room version that requires it: %#v", sent.Users)
	}
	if got := sent.Users["@matrix-admin:example.com"]; got != 100 {
		t.Fatalf("target level = %d, want 100", got)
	}
}

func TestSetPowerLevelsDropsCreatorFromRoomVersion12(t *testing.T) {
	srv, sent := newFakeSynapse(t, "@daniel:example.com", "12")
	c := NewClient(srv.URL, "token", "example.com")

	if err := c.SetPowerLevels("!room:example.com", "@matrix-admin:example.com", 100); err != nil {
		t.Fatalf("SetPowerLevels returned error: %v", err)
	}
	if _, listed := sent.Users["@daniel:example.com"]; listed {
		t.Fatalf("creator must not appear in content.users from room version 12: %#v", sent.Users)
	}
	if got := sent.Users["@matrix-admin:example.com"]; got != 100 {
		t.Fatalf("target level = %d, want 100", got)
	}
}
