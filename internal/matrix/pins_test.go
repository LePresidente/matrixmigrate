package matrix

import (
	"reflect"
	"testing"

	"github.com/aligundogdu/matrixmigrate/internal/mattermost"
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

func TestPinCapableUsersRanksTheStrongestLocalUserFirst(t *testing.T) {
	pl := &PowerLevelsContent{Users: map[string]int{
		"@alice:example.com":   50,
		"@bob_dev:example.com": 100,
		"@carol:example.com":   0,
	}}
	got := pinCapableUsers(pl, "example.com", 50, "")
	if want := []string{"@bob_dev:example.com", "@alice:example.com"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pinCapableUsers = %v, want %v (carol is below the bar)", got, want)
	}
}

func TestPickPinCapableUserIgnoresRemoteUsers(t *testing.T) {
	// The AS token can only act as users in its own namespace on this homeserver.
	pl := &PowerLevelsContent{Users: map[string]int{
		"@remote:other.example": 100,
		"@alice:example.com":    50,
	}}
	if got := firstPinCandidate(pl, "example.com", 50, ""); got != "@alice:example.com" {
		t.Fatalf("first pin candidate = %q, want @alice:example.com", got)
	}
}

func TestPickPinCapableUserReturnsEmptyWhenNobodyQualifies(t *testing.T) {
	pl := &PowerLevelsContent{Users: map[string]int{"@alice:example.com": 25}}
	if got := firstPinCandidate(pl, "example.com", 50, ""); got != "" {
		t.Fatalf("first pin candidate = %q, want empty", got)
	}
}

func TestPinCapableUsersAreDeterministicOnTies(t *testing.T) {
	pl := &PowerLevelsContent{Users: map[string]int{
		"@bob_dev:example.com": 100,
		"@alice:example.com":   100,
	}}
	for i := 0; i < 20; i++ {
		if got := firstPinCandidate(pl, "example.com", 50, ""); got != "@alice:example.com" {
			t.Fatalf("first pin candidate = %q, want @alice:example.com on every call", got)
		}
	}
}

func TestPinnedByRoomOrdersByCreationTime(t *testing.T) {
	posts := []mattermost.Post{
		{ID: "p2", ChannelID: "c1", CreateAt: 200, IsPinned: true},
		{ID: "p1", ChannelID: "c1", CreateAt: 100, IsPinned: true},
		{ID: "p3", ChannelID: "c1", CreateAt: 300},
	}
	events := map[string]string{"p1": "$e1", "p2": "$e2", "p3": "$e3"}
	rooms := map[string]string{"p1": "!room:example.com", "p2": "!room:example.com", "p3": "!room:example.com"}

	byRoom, skips := pinnedByRoom(posts, events, rooms)
	if len(skips) != 0 {
		t.Fatalf("no post should be skipped, got %v", skips)
	}
	if want := []string{"$e1", "$e2"}; !reflect.DeepEqual(byRoom["!room:example.com"], want) {
		t.Fatalf("pins = %v, want %v (oldest first, unpinned post excluded)", byRoom["!room:example.com"], want)
	}
}

func TestPinnedByRoomBreaksTiesOnPostID(t *testing.T) {
	posts := []mattermost.Post{
		{ID: "pb", ChannelID: "c1", CreateAt: 100, IsPinned: true},
		{ID: "pa", ChannelID: "c1", CreateAt: 100, IsPinned: true},
	}
	events := map[string]string{"pa": "$ea", "pb": "$eb"}
	rooms := map[string]string{"pa": "!room:example.com", "pb": "!room:example.com"}

	byRoom, _ := pinnedByRoom(posts, events, rooms)
	if want := []string{"$ea", "$eb"}; !reflect.DeepEqual(byRoom["!room:example.com"], want) {
		t.Fatalf("pins = %v, want %v (post ID breaks the tie)", byRoom["!room:example.com"], want)
	}
}

func TestPinnedByRoomSkipsUnmappedPostsWithAReason(t *testing.T) {
	posts := []mattermost.Post{
		{ID: "p1", ChannelID: "c1", CreateAt: 100, IsPinned: true}, // never imported
		{ID: "p2", ChannelID: "c2", CreateAt: 200, IsPinned: true}, // channel not migrated
		{ID: "p3", ChannelID: "c1", CreateAt: 300, IsPinned: true, DeleteAt: 400},
	}
	events := map[string]string{"p2": "$e2", "p3": "$e3"}
	rooms := map[string]string{"p1": "!room:example.com", "p3": "!room:example.com"}

	byRoom, skips := pinnedByRoom(posts, events, rooms)
	if len(byRoom) != 0 {
		t.Fatalf("nothing should be pinnable, got %v", byRoom)
	}
	if len(skips) != 2 {
		t.Fatalf("want 2 skips (deleted posts are not reported), got %v", skips)
	}
	reasons := map[string]string{skips[0].PostID: skips[0].Reason, skips[1].PostID: skips[1].Reason}
	if reasons["p1"] != "message not imported" {
		t.Fatalf("p1 reason = %q", reasons["p1"])
	}
	if reasons["p2"] != "no room mapping" {
		t.Fatalf("p2 reason = %q", reasons["p2"])
	}
}

func TestPickPinCapableUserFallsBackToTheCreator(t *testing.T) {
	// Room version 12 gives the creator implicit infinite power and forbids listing them in
	// content.users, so a room that can be pinned in still shows an empty users map.
	pl := &PowerLevelsContent{StateDefault: 50, Users: map[string]int{}}
	if got := firstPinCandidate(pl, "example.com", 50, "@alice:example.com"); got != "@alice:example.com" {
		t.Fatalf("first pin candidate = %q, want the creator @alice:example.com", got)
	}
}

func TestPickPinCapableUserPrefersAnExplicitlyPoweredMemberOverTheCreator(t *testing.T) {
	pl := &PowerLevelsContent{Users: map[string]int{"@bob_dev:example.com": 100}}
	if got := firstPinCandidate(pl, "example.com", 50, "@alice:example.com"); got != "@bob_dev:example.com" {
		t.Fatalf("first pin candidate = %q, want @bob_dev:example.com", got)
	}
}

func TestPickPinCapableUserIgnoresARemoteCreator(t *testing.T) {
	if got := firstPinCandidate(&PowerLevelsContent{}, "example.com", 50, "@alice:other.example"); got != "" {
		t.Fatalf("first pin candidate = %q, want empty for a creator on another homeserver", got)
	}
}

func TestPinnedFromStateReadsTheAdminAPIDump(t *testing.T) {
	state := []adminStateEvent{
		{Type: "m.room.create", Content: []byte(`{"room_version":"12"}`)},
		{Type: EventTypePinnedEvents, Content: []byte(`{"pinned":["$a","$b"]}`)},
	}
	if want := []string{"$a", "$b"}; !reflect.DeepEqual(pinnedFromState(state), want) {
		t.Fatalf("pinnedFromState = %v, want %v", pinnedFromState(state), want)
	}
}

func TestPinnedFromStateReturnsNothingForAnUnpinnedRoom(t *testing.T) {
	// No m.room.pinned_events means nobody has ever pinned here - an empty list, not a failure.
	state := []adminStateEvent{{Type: "m.room.create", Content: []byte(`{"room_version":"12"}`)}}
	if got := pinnedFromState(state); len(got) != 0 {
		t.Fatalf("pinnedFromState = %v, want empty", got)
	}
}

func TestPowerLevelsFromStateReadsTheAdminAPIDump(t *testing.T) {
	state := []adminStateEvent{
		{Type: EventTypePowerLevels, Content: []byte(`{"state_default":50,"users":{"@alice:example.com":100}}`)},
	}
	pl := powerLevelsFromState(state)
	if pl == nil {
		t.Fatal("powerLevelsFromState returned nil for a room that has power levels")
	}
	if pl.StateDefault != 50 || pl.Users["@alice:example.com"] != 100 {
		t.Fatalf("powerLevelsFromState = %+v, want state_default 50 and alice at 100", pl)
	}
}

func TestCreatorFromStateUsesTheCreateEventSender(t *testing.T) {
	// From room version 12 the create event content carries no creator field, so the sender is
	// the only place the creator is recorded.
	state := []adminStateEvent{
		{Type: EventTypeRoomCreate, Sender: "@alice:example.com", Content: []byte(`{"room_version":"12"}`)},
	}
	if got := creatorFromState(state); got != "@alice:example.com" {
		t.Fatalf("creatorFromState = %q, want @alice:example.com", got)
	}
}

// firstPinCandidate is the strongest candidate pinCapableUsers offers, or "" when it offers
// none. The production caller walks the whole list; these tests care about the head of it.
func firstPinCandidate(pl *PowerLevelsContent, homeserver string, required int, creator string) string {
	users := pinCapableUsers(pl, homeserver, required, creator)
	if len(users) == 0 {
		return ""
	}
	return users[0]
}

func TestPinCapableUsersKeepsTheCreatorAsALastResort(t *testing.T) {
	// The bot outranks everyone on paper; the creator is the one who is still in the room.
	pl := &PowerLevelsContent{Users: map[string]int{
		"@bot:example.com":   100,
		"@alice:example.com": 50,
	}}
	got := pinCapableUsers(pl, "example.com", 50, "@carol:example.com")
	want := []string{"@bot:example.com", "@alice:example.com", "@carol:example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pinCapableUsers = %v, want %v", got, want)
	}
}

func TestPinCapableUsersDoesNotRepeatACreatorAlreadyListed(t *testing.T) {
	pl := &PowerLevelsContent{Users: map[string]int{"@alice:example.com": 100}}
	got := pinCapableUsers(pl, "example.com", 50, "@alice:example.com")
	if want := []string{"@alice:example.com"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pinCapableUsers = %v, want %v", got, want)
	}
}

func TestOnlyJoinedDropsCandidatesWhoHaveLeft(t *testing.T) {
	// Power levels keep an entry for someone who has left, and the migration bot is usually
	// the highest-powered name in a migrated room long after leave-rooms withdrew it.
	joined := map[string]struct{}{"@alice:example.com": {}, "@carol:example.com": {}}
	got := onlyJoined([]string{"@bot:example.com", "@alice:example.com", "@carol:example.com"}, joined)
	if want := []string{"@alice:example.com", "@carol:example.com"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("onlyJoined = %v, want %v", got, want)
	}
}

func TestJoinedFromStateReadsMembership(t *testing.T) {
	state := []adminStateEvent{
		{Type: EventTypeRoomMember, StateKey: "@alice:example.com", Content: []byte(`{"membership":"join"}`)},
		{Type: EventTypeRoomMember, StateKey: "@bot:example.com", Content: []byte(`{"membership":"leave"}`)},
		{Type: EventTypeRoomMember, StateKey: "@bob_dev:example.com", Content: []byte(`{"membership":"invite"}`)},
	}
	joined := joinedFromState(state)
	if _, ok := joined["@alice:example.com"]; !ok {
		t.Fatal("a joined member should be in the set")
	}
	if len(joined) != 1 {
		t.Fatalf("joinedFromState = %v, want only alice", joined)
	}
}
