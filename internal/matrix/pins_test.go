package matrix

import (
	"reflect"
	"testing"
)

func TestUnionPinnedAppendsNewIDsAfterExistingOnes(t *testing.T) {
	merged, changed := unionPinned([]string{"$a", "$b"}, []string{"$c"})
	if !changed {
		t.Fatal("adding a new pin should report a change")
	}
	if want := []string{"$a", "$b", "$c"}; !reflect.DeepEqual(merged, want) {
		t.Fatalf("merged = %v, want %v", merged, want)
	}
}

func TestUnionPinnedReportsNoChangeWhenEverythingIsAlreadyPinned(t *testing.T) {
	merged, changed := unionPinned([]string{"$a", "$b"}, []string{"$b", "$a"})
	if changed {
		t.Fatal("re-pinning what is already pinned must not report a change")
	}
	if want := []string{"$a", "$b"}; !reflect.DeepEqual(merged, want) {
		t.Fatalf("merged = %v, want %v (existing order must survive)", merged, want)
	}
}

func TestUnionPinnedDropsDuplicatesWithinTheMigratedList(t *testing.T) {
	merged, changed := unionPinned(nil, []string{"$a", "$a", "$b"})
	if !changed {
		t.Fatal("pinning into an empty room is a change")
	}
	if want := []string{"$a", "$b"}; !reflect.DeepEqual(merged, want) {
		t.Fatalf("merged = %v, want %v", merged, want)
	}
}

func TestRequiredPinPowerLevelPrefersTheExplicitEventEntry(t *testing.T) {
	pl := &PowerLevelsContent{StateDefault: 100, Events: map[string]int{EventTypePinnedEvents: 25}}
	if got := requiredPinPowerLevel(pl); got != 25 {
		t.Fatalf("requiredPinPowerLevel = %d, want 25", got)
	}
}

func TestRequiredPinPowerLevelFallsBackToStateDefaultThenFifty(t *testing.T) {
	if got := requiredPinPowerLevel(&PowerLevelsContent{StateDefault: 75}); got != 75 {
		t.Fatalf("with state_default 75, got %d", got)
	}
	// StateDefault is unmarshalled with omitempty, so absent and 0 are indistinguishable;
	// 50 is the spec default and the safe assumption.
	if got := requiredPinPowerLevel(&PowerLevelsContent{}); got != 50 {
		t.Fatalf("with no state_default, got %d, want 50", got)
	}
	if got := requiredPinPowerLevel(nil); got != 50 {
		t.Fatalf("with no power levels at all, got %d, want 50", got)
	}
}

func TestPickPinCapableUserPicksTheStrongestLocalUser(t *testing.T) {
	pl := &PowerLevelsContent{Users: map[string]int{
		"@alice:example.com":   50,
		"@bob_dev:example.com": 100,
		"@carol:example.com":   0,
	}}
	if got := pickPinCapableUser(pl, "example.com", 50); got != "@bob_dev:example.com" {
		t.Fatalf("pickPinCapableUser = %q, want @bob_dev:example.com", got)
	}
}

func TestPickPinCapableUserIgnoresRemoteUsers(t *testing.T) {
	// The AS token can only act as users in its own namespace on this homeserver.
	pl := &PowerLevelsContent{Users: map[string]int{
		"@remote:other.example": 100,
		"@alice:example.com":    50,
	}}
	if got := pickPinCapableUser(pl, "example.com", 50); got != "@alice:example.com" {
		t.Fatalf("pickPinCapableUser = %q, want @alice:example.com", got)
	}
}

func TestPickPinCapableUserReturnsEmptyWhenNobodyQualifies(t *testing.T) {
	pl := &PowerLevelsContent{Users: map[string]int{"@alice:example.com": 25}}
	if got := pickPinCapableUser(pl, "example.com", 50); got != "" {
		t.Fatalf("pickPinCapableUser = %q, want empty", got)
	}
}

func TestPickPinCapableUserIsDeterministicOnTies(t *testing.T) {
	pl := &PowerLevelsContent{Users: map[string]int{
		"@bob_dev:example.com": 100,
		"@alice:example.com":   100,
	}}
	for i := 0; i < 20; i++ {
		if got := pickPinCapableUser(pl, "example.com", 50); got != "@alice:example.com" {
			t.Fatalf("pickPinCapableUser = %q, want @alice:example.com on every call", got)
		}
	}
}
