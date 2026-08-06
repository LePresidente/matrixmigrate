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
	return newFakeSynapseWithLevels(t, creator, version, map[string]int{creator: 100})
}

// newFakeSynapseWithLevels is newFakeSynapse with explicit starting power levels.
func newFakeSynapseWithLevels(t *testing.T, creator, version string, levels map[string]int) (*httptest.Server, *PowerLevelsContent) {
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
		json.NewEncoder(w).Encode(PowerLevelsContent{Users: levels})
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

func TestCanChangeUserLevel(t *testing.T) {
	current := &PowerLevelsContent{Users: map[string]int{
		"@me:example.com":      100,
		"@creator:example.com": 100,
		"@mod:example.com":     50,
	}}
	const me = "@me:example.com"

	tests := []struct {
		name    string
		target  string
		newLvl  int
		myLevel int
		want    bool
	}{
		// The case from the field: the room creator sits at 100 in room versions before 12,
		// exactly where the migration admin sits, and the server refuses the whole event.
		{"equal level cannot be demoted", "@creator:example.com", 50, 100, false},
		{"lower level can be changed", "@mod:example.com", 50, 100, true},
		{"unlisted user can be raised below own level", "@new:example.com", 50, 100, true},
		{"nobody can be raised above own level", "@new:example.com", 100, 50, false},
		{"own entry is always allowed", me, 100, 100, true},
		// An entry already at the wanted level is not a change, so there is nothing to
		// authorise and it must not be filtered out.
		{"no-op on an equal-level user is fine", "@creator:example.com", 100, 100, true},
	}
	for _, tt := range tests {
		if got := canChangeUserLevel(current, me, tt.myLevel, tt.target, tt.newLvl); got != tt.want {
			t.Errorf("%s: canChangeUserLevel(%s -> %d, myLevel=%d) = %v, want %v",
				tt.name, tt.target, tt.newLvl, tt.myLevel, got, tt.want)
		}
	}
}

func TestSetPowerLevelsBulkKeepsGoingAroundAnUntouchableEntry(t *testing.T) {
	// One entry the account may not change must not cost every other member their level.
	srv, sent := newFakeSynapseWithLevels(t, "@creator:example.com", "11", map[string]int{
		"@creator:example.com":      100,
		"@matrix-admin:example.com": 100,
	})
	c := NewClient(srv.URL, "token", "example.com")

	err := c.SetPowerLevelsBulk("!room:example.com", map[string]int{
		"@creator:example.com": 50,
		"@alice:example.com":   50,
		"@bob:example.com":     50,
	})
	if err != nil {
		t.Fatalf("SetPowerLevelsBulk returned error: %v", err)
	}
	if got := sent.Users["@creator:example.com"]; got != 100 {
		t.Fatalf("creator level = %d, want 100 (left alone)", got)
	}
	for _, u := range []string{"@alice:example.com", "@bob:example.com"} {
		if got := sent.Users[u]; got != 50 {
			t.Fatalf("%s level = %d, want 50", u, got)
		}
	}
}
