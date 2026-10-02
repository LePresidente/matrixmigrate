package matrix

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
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
	// Content that never carried state_default gets the spec default of 50.
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
	got, err := pinnedFromState(state)
	if err != nil {
		t.Fatalf("pinnedFromState returned %v", err)
	}
	if want := []string{"$a", "$b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pinnedFromState = %v, want %v", got, want)
	}
}

func TestPinnedFromStateRejectsUnparseableContent(t *testing.T) {
	state := []adminStateEvent{{Type: EventTypePinnedEvents, Content: []byte(`{"pinned":"not-a-list"}`)}}
	if got, err := pinnedFromState(state); err == nil {
		t.Fatalf("pinnedFromState = %v with no error, want an error for unparseable content", got)
	}
}

func TestPinnedFromStateReturnsNothingForAnUnpinnedRoom(t *testing.T) {
	// No m.room.pinned_events means nobody has ever pinned here - an empty list, not a failure.
	state := []adminStateEvent{{Type: "m.room.create", Content: []byte(`{"room_version":"12"}`)}}
	if got, err := pinnedFromState(state); err != nil || len(got) != 0 {
		t.Fatalf("pinnedFromState = %v, %v; want empty and no error", got, err)
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

const pinTestRoom = "!room:example.com"

// pinWrite is one m.room.pinned_events PUT the fake homeserver received.
type pinWrite struct {
	asUser string // "" when the admin token wrote it
	pinned []string
}

// fakePinServer is a homeserver that knows one room. It serves that room's state through the
// Synapse admin API, accepts joins, and records pin writes — refusing them for the users
// listed in refuse, and cutting the connection for the users listed in hangUp.
type fakePinServer struct {
	mu         sync.Mutex
	state      []map[string]any
	stateReads int
	joins      int
	writes     []pinWrite
	attempts   []string // every pin write attempted, by sender ("" for the admin)
	refuse     map[string]refusal
	hangUp     map[string]bool
}

type refusal struct {
	status  int
	errcode string
}

const pinTestAdmin = "@matrix-admin:example.com"

func newFakePinServer(t *testing.T, state []map[string]any) (*fakePinServer, *Client) {
	t.Helper()
	f := &fakePinServer{state: state, refuse: map[string]refusal{}, hangUp: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "admin-token", "example.com")
	c.rateLimit = 0
	c.maxRetries = 0
	return f, c
}

func (f *fakePinServer) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path

	switch {
	case path == "/_matrix/client/v3/account/whoami":
		json.NewEncoder(w).Encode(map[string]any{"user_id": pinTestAdmin})

	case strings.HasPrefix(path, "/_synapse/admin/v1/rooms/") && strings.HasSuffix(path, "/state"):
		f.stateReads++
		json.NewEncoder(w).Encode(map[string]any{"state": f.state})

	case strings.HasPrefix(path, "/_synapse/admin/v1/rooms/"):
		json.NewEncoder(w).Encode(map[string]any{})

	case strings.HasPrefix(path, "/_synapse/admin/v1/join/") || strings.HasSuffix(path, "/join"):
		f.joins++
		json.NewEncoder(w).Encode(map[string]any{"room_id": pinTestRoom})

	case strings.HasSuffix(path, "/state/"+EventTypePinnedEvents) && r.Method == http.MethodPut:
		sender := r.URL.Query().Get("user_id")
		f.attempts = append(f.attempts, sender)
		if f.hangUp[sender] {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		if ref, ok := f.refuse[sender]; ok {
			w.WriteHeader(ref.status)
			json.NewEncoder(w).Encode(map[string]any{"errcode": ref.errcode, "error": "refused"})
			return
		}
		var content PinnedEventsContent
		json.NewDecoder(r.Body).Decode(&content)
		f.writes = append(f.writes, pinWrite{asUser: sender, pinned: content.Pinned})
		w.Write([]byte(`{"event_id":"$pin"}`))

	case strings.Contains(path, "/state/"):
		// A client-side state read: answer from the same state the admin API serves.
		eventType := path[strings.LastIndex(path, "/state/")+len("/state/"):]
		for _, event := range f.state {
			if event["type"] == eventType {
				json.NewEncoder(w).Encode(event["content"])
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"errcode": "M_NOT_FOUND", "error": "no such state"})

	default:
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"errcode": "M_UNRECOGNIZED", "error": path})
	}
}

func TestPinAsCandidatesMovesPastRefusedCandidates(t *testing.T) {
	// A locked account answers 401 M_USER_LOCKED, a member without power 403 M_FORBIDDEN.
	// Both are the homeserver saying "not this one", and the next candidate may well succeed.
	f, c := newFakePinServer(t, nil)
	c.SetASToken("as-token")
	f.refuse["@alice:example.com"] = refusal{http.StatusUnauthorized, "M_USER_LOCKED"}
	f.refuse["@bob_dev:example.com"] = refusal{http.StatusForbidden, "M_FORBIDDEN"}

	err := c.pinAsCandidates(pinTestRoom, []string{"$a"},
		[]string{"@alice:example.com", "@bob_dev:example.com", "@carol:example.com"})
	if err != nil {
		t.Fatalf("pinAsCandidates returned %v, want success as carol", err)
	}
	if len(f.writes) != 1 || f.writes[0].asUser != "@carol:example.com" {
		t.Fatalf("writes = %+v, want one write as carol", f.writes)
	}
}

func TestPinAsCandidatesReturnsTheLastRefusalWhenEveryoneIsRefused(t *testing.T) {
	f, c := newFakePinServer(t, nil)
	c.SetASToken("as-token")
	f.refuse["@alice:example.com"] = refusal{http.StatusForbidden, "M_FORBIDDEN"}
	f.refuse["@bob_dev:example.com"] = refusal{http.StatusUnauthorized, "M_USER_LOCKED"}

	err := c.pinAsCandidates(pinTestRoom, []string{"$a"}, []string{"@alice:example.com", "@bob_dev:example.com"})
	if err == nil || !strings.Contains(err.Error(), "M_USER_LOCKED") {
		t.Fatalf("pinAsCandidates returned %v, want the last refusal (M_USER_LOCKED)", err)
	}
}

func TestPinAsCandidatesStopsOnATransportFailure(t *testing.T) {
	// A connection that dies is not an answer about alice; it says the homeserver is not
	// reachable, and trying everyone else against it would only repeat the failure.
	f, c := newFakePinServer(t, nil)
	c.SetASToken("as-token")
	f.hangUp["@alice:example.com"] = true

	err := c.pinAsCandidates(pinTestRoom, []string{"$a"}, []string{"@alice:example.com", "@bob_dev:example.com"})
	if err == nil {
		t.Fatal("pinAsCandidates succeeded, want the transport error")
	}
	if want := []string{"@alice:example.com"}; !reflect.DeepEqual(f.attempts, want) {
		t.Fatalf("attempts = %v, want %v (bob_dev must not be tried)", f.attempts, want)
	}
}

// pinRoomState builds an admin API state dump for pinTestRoom: created by creator, with the
// given power levels content, joined members and pinned content (nil for no pin event).
func pinRoomState(creator string, levels map[string]any, joined []string, pinned any) []map[string]any {
	state := []map[string]any{
		{"type": EventTypeRoomCreate, "state_key": "", "sender": creator, "content": map[string]any{"room_version": "11"}},
		{"type": EventTypePowerLevels, "state_key": "", "sender": creator, "content": levels},
	}
	for _, user := range joined {
		state = append(state, map[string]any{
			"type": EventTypeRoomMember, "state_key": user, "sender": user,
			"content": map[string]any{"membership": "join"},
		})
	}
	if pinned != nil {
		state = append(state, map[string]any{"type": EventTypePinnedEvents, "state_key": "", "sender": creator, "content": pinned})
	}
	return state
}

// runPinPass runs the pin pass for one pinned post, mapped to event $new in pinTestRoom.
func runPinPass(c *Client) *ImportMessagesResult {
	result := &ImportMessagesResult{
		Stats:   &MessageImportStats{},
		Mapping: map[string]string{"p1": "$new"},
	}
	posts := []mattermost.Post{{ID: "p1", ChannelID: "c1", CreateAt: 100, IsPinned: true}}
	NewImporter(c).importPins(result, posts, map[string]string{"p1": pinTestRoom}, nil)
	return result
}

func TestImportPinsWritesAsAMemberWithoutJoiningTheAdmin(t *testing.T) {
	// After leave-rooms the admin is in none of the migrated rooms. Reading the room and
	// writing as a member through the Application Service needs no join at all.
	state := pinRoomState("@alice:example.com",
		map[string]any{"users": map[string]any{"@alice:example.com": 100}},
		[]string{"@alice:example.com"}, map[string]any{"pinned": []string{"$old"}})
	f, c := newFakePinServer(t, state)
	c.SetASToken("as-token")

	result := runPinPass(c)

	if result.Stats.PinnedRoomsUpdated != 1 || result.Stats.PinsFailed != 0 {
		t.Fatalf("stats = %+v, errors = %v; want one room updated", result.Stats, result.Errors)
	}
	if f.joins != 0 {
		t.Fatalf("the admin was joined %d time(s), want none", f.joins)
	}
	if f.stateReads != 1 {
		t.Fatalf("room state was read %d time(s), want once", f.stateReads)
	}
	want := []pinWrite{{asUser: "@alice:example.com", pinned: []string{"$old", "$new"}}}
	if !reflect.DeepEqual(f.writes, want) {
		t.Fatalf("writes = %+v, want %+v", f.writes, want)
	}
}

func TestImportPinsWritesAsTheAdminWhenItIsJoinedAndPowerful(t *testing.T) {
	state := pinRoomState(pinTestAdmin,
		map[string]any{"users": map[string]any{pinTestAdmin: 100, "@alice:example.com": 100}},
		[]string{pinTestAdmin, "@alice:example.com"}, nil)
	f, c := newFakePinServer(t, state)
	c.SetASToken("as-token")

	result := runPinPass(c)

	if result.Stats.PinnedRoomsUpdated != 1 {
		t.Fatalf("stats = %+v, errors = %v; want one room updated", result.Stats, result.Errors)
	}
	if f.joins != 0 || f.stateReads != 1 {
		t.Fatalf("joins = %d, state reads = %d; want 0 and 1", f.joins, f.stateReads)
	}
	if want := []pinWrite{{asUser: "", pinned: []string{"$new"}}}; !reflect.DeepEqual(f.writes, want) {
		t.Fatalf("writes = %+v, want one write as the admin", f.writes)
	}
}

func TestImportPinsFallsBackToJoiningWithoutAnASToken(t *testing.T) {
	// No AS token is the deployment that worked before: join the admin, write as the admin.
	state := pinRoomState(pinTestAdmin,
		map[string]any{"users": map[string]any{pinTestAdmin: 100}},
		[]string{"@alice:example.com"}, nil)
	f, c := newFakePinServer(t, state)

	result := runPinPass(c)

	if result.Stats.PinnedRoomsUpdated != 1 {
		t.Fatalf("stats = %+v, errors = %v; want one room updated", result.Stats, result.Errors)
	}
	if f.joins == 0 {
		t.Fatal("the admin was never joined, but without an AS token joining is the only way in")
	}
	if want := []pinWrite{{asUser: "", pinned: []string{"$new"}}}; !reflect.DeepEqual(f.writes, want) {
		t.Fatalf("writes = %+v, want one write as the admin", f.writes)
	}
}

func TestRequiredPinPowerLevelHonoursAnExplicitZeroStateDefault(t *testing.T) {
	// state_default 0 means anyone may send state; reading it as "absent" would demand 50.
	state := []adminStateEvent{{Type: EventTypePowerLevels, Content: []byte(`{"state_default":0}`)}}
	if got := requiredPinPowerLevel(powerLevelsFromState(state)); got != 0 {
		t.Fatalf("requiredPinPowerLevel with explicit state_default 0 = %d, want 0", got)
	}

	var decoded PowerLevelsContent
	if err := json.Unmarshal([]byte(`{"state_default":0}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if got := requiredPinPowerLevel(&decoded); got != 0 {
		t.Fatalf("requiredPinPowerLevel of decoded content = %d, want 0", got)
	}

	var absent PowerLevelsContent
	if err := json.Unmarshal([]byte(`{"users_default":0}`), &absent); err != nil {
		t.Fatal(err)
	}
	if got := requiredPinPowerLevel(&absent); got != 50 {
		t.Fatalf("requiredPinPowerLevel with no state_default = %d, want the spec default 50", got)
	}
}

func TestPinCandidatesAddJoinedMembersWhenUsersDefaultIsEnough(t *testing.T) {
	// users_default at or above the bar lets every member pin. An explicit entry still wins:
	// carol is listed at 0 and so cannot, and a remote user is out of the AS's reach.
	pl := &PowerLevelsContent{
		UsersDefault: 50,
		Users:        map[string]int{"@bob_dev:example.com": 100, "@carol:example.com": 0},
	}
	joined := map[string]struct{}{
		"@dave:example.com": {}, "@alice:example.com": {}, "@bob_dev:example.com": {},
		"@carol:example.com": {}, "@remote:other.example": {},
	}
	got := pinCandidates(pl, "example.com", 50, "", joined)
	want := []string{"@bob_dev:example.com", "@alice:example.com", "@dave:example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pinCandidates = %v, want %v", got, want)
	}
}

func TestImportPinsWritesAsAnUnlistedMemberWhenUsersDefaultIsEnough(t *testing.T) {
	state := pinRoomState("@carol:example.com",
		map[string]any{"users_default": 50, "users": map[string]any{"@bob_dev:example.com": 0}},
		[]string{"@alice:example.com", "@bob_dev:example.com"}, nil)
	f, c := newFakePinServer(t, state)
	c.SetASToken("as-token")

	result := runPinPass(c)

	if result.Stats.PinnedRoomsUpdated != 1 {
		t.Fatalf("stats = %+v, errors = %v; want one room updated", result.Stats, result.Errors)
	}
	if want := []string{"@alice:example.com"}; !reflect.DeepEqual(f.attempts, want) {
		t.Fatalf("attempts = %v, want %v", f.attempts, want)
	}
}

func TestImportPinsFailsARoomWhosePinnedContentIsUnreadable(t *testing.T) {
	// Content that cannot be parsed is not "no pins": writing the migrated list over it could
	// throw away pins nobody can see any more.
	state := pinRoomState(pinTestAdmin,
		map[string]any{"users": map[string]any{pinTestAdmin: 100}},
		[]string{pinTestAdmin}, map[string]any{"pinned": "not-a-list"})
	f, c := newFakePinServer(t, state)
	c.SetASToken("as-token")

	result := runPinPass(c)

	if result.Stats.PinsFailed != 1 || result.Stats.PinnedRoomsUpdated != 0 {
		t.Fatalf("stats = %+v, want the room counted as failed", result.Stats)
	}
	if len(f.attempts) != 0 {
		t.Fatalf("pin writes attempted = %v, want none", f.attempts)
	}
}

func TestImportPinsCountsOnlyTheEventsItAdded(t *testing.T) {
	// The room carries a duplicate. Rewriting it collapses the duplicate, so the list does
	// not grow by one even though one event was added.
	state := pinRoomState(pinTestAdmin,
		map[string]any{"users": map[string]any{pinTestAdmin: 100}},
		[]string{pinTestAdmin}, map[string]any{"pinned": []string{"$old", "$old"}})
	f, c := newFakePinServer(t, state)

	result := runPinPass(c)

	if want := []pinWrite{{asUser: "", pinned: []string{"$old", "$new"}}}; !reflect.DeepEqual(f.writes, want) {
		t.Fatalf("writes = %+v, want %+v", f.writes, want)
	}
	if result.Stats.PinnedEventsAdded != 1 {
		t.Fatalf("PinnedEventsAdded = %d, want 1", result.Stats.PinnedEventsAdded)
	}
}

func TestNewPinCountIgnoresDuplicatesAndExistingPins(t *testing.T) {
	got := newPinCount([]string{"$a", "$a", "$b"}, []string{"$b", "$c", "$c", "", "$d"})
	if got != 2 {
		t.Fatalf("newPinCount = %d, want 2 ($c and $d)", got)
	}
}
