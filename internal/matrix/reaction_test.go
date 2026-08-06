package matrix

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/aligundogdu/matrixmigrate/internal/mattermost"
)

func TestReactionSkipReason(t *testing.T) {
	const (
		event  = "$evt:example.com"
		room   = "!room:example.com"
		sender = "@alice:example.com"
		emoji  = "👍"
	)

	tests := []struct {
		name                          string
		event, room, sender, emojiKey string
		want                          string
	}{
		{"everything resolved", event, room, sender, emoji, ""},
		{"target missing", "", room, sender, emoji, "target message not imported"},
		{"room missing", event, "", sender, emoji, "no room mapping"},
		{"sender missing", event, room, "", emoji, "user not mapped"},
		{"emoji missing", event, room, sender, "", "empty emoji name"},
		// A reaction on a post that was never imported is the common case (system messages);
		// it must report the target, not the first other thing that also happens to be empty.
		{"target missing wins over room", "", "", sender, emoji, "target message not imported"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reactionSkipReason(tt.event, tt.room, tt.sender, tt.emojiKey); got != tt.want {
				t.Errorf("reactionSkipReason() = %q, want %q", got, tt.want)
			}
		})
	}
}

// sentReaction records what a fake homeserver was asked to send.
type sentReaction struct {
	path    string
	query   url.Values
	content map[string]any
}

// newFakeReactionServer accepts any m.reaction send and records the request.
func newFakeReactionServer(t *testing.T) (*httptest.Server, *sentReaction) {
	t.Helper()
	var sent sentReaction

	mux := http.NewServeMux()
	mux.HandleFunc("/_matrix/client/v3/rooms/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sent.path = r.URL.Path
		sent.query = r.URL.Query()
		json.Unmarshal(body, &sent.content)
		w.Write([]byte(`{"event_id":"$reaction:example.com"}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &sent
}

func TestSendReactionWithTimestamp(t *testing.T) {
	srv, sent := newFakeReactionServer(t)
	c := NewClient(srv.URL, "admin-token", "example.com")
	c.SetASToken("as-token")

	resp, err := c.SendReactionWithTimestamp("!room:example.com", "$target:example.com", "👍",
		1700000000000, "@alice:example.com")
	if err != nil {
		t.Fatalf("SendReactionWithTimestamp returned error: %v", err)
	}
	if resp.EventID != "$reaction:example.com" {
		t.Errorf("event ID = %q, want $reaction:example.com", resp.EventID)
	}

	if !strings.Contains(sent.path, "/send/m.reaction/") {
		t.Errorf("path = %q, want an m.reaction send endpoint", sent.path)
	}
	if got := sent.query.Get("user_id"); got != "@alice:example.com" {
		t.Errorf("user_id = %q, want @alice:example.com", got)
	}
	if got := sent.query.Get("ts"); got != "1700000000000" {
		t.Errorf("ts = %q, want 1700000000000", got)
	}

	// An m.reaction carries the relation and nothing else. A body or msgtype here would mean
	// the event was built as a message and clients would show it as one.
	if len(sent.content) != 1 {
		t.Fatalf("content has %d keys, want only m.relates_to: %#v", len(sent.content), sent.content)
	}
	relates, ok := sent.content["m.relates_to"].(map[string]any)
	if !ok {
		t.Fatalf("m.relates_to missing or not an object: %#v", sent.content)
	}
	if relates["rel_type"] != "m.annotation" {
		t.Errorf("rel_type = %v, want m.annotation", relates["rel_type"])
	}
	if relates["event_id"] != "$target:example.com" {
		t.Errorf("event_id = %v, want $target:example.com", relates["event_id"])
	}
	if relates["key"] != "👍" {
		t.Errorf("key = %v, want 👍", relates["key"])
	}
}

// TestImportReactionsClassifiesEveryOutcome drives the reaction pass through all of its
// branches at once, because each one silently changes what ends up in the room.
func TestImportReactionsClassifiesEveryOutcome(t *testing.T) {
	var sentKeys []string

	mux := http.NewServeMux()
	mux.HandleFunc("/_matrix/client/v3/rooms/", func(w http.ResponseWriter, r *http.Request) {
		// Anyone but @carol is a member; @carol left the channel after reacting.
		if r.URL.Query().Get("user_id") == "@carol:example.com" {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"errcode":"M_FORBIDDEN","error":"User @carol:example.com not in room"}`))
			return
		}
		var content map[string]any
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &content)
		if relates, ok := content["m.relates_to"].(map[string]any); ok {
			sentKeys = append(sentKeys, relates["key"].(string))
		}
		w.Write([]byte(`{"event_id":"$reaction:example.com"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "admin-token", "example.com")
	c.SetASToken("as-token")
	importer := NewImporter(c)

	result := &ImportMessagesResult{
		Stats:           &MessageImportStats{},
		Mapping:         map[string]string{"post1": "$evt1:example.com"},
		ReactionMapping: make(map[string]string),
	}
	roomByPost := map[string]string{"post1": "!room:example.com"}
	userMapping := map[string]string{
		"mm-alice": "@alice:example.com",
		"mm-bob":   "@bob_dev:example.com",
		"mm-carol": "@carol:example.com",
	}
	alreadyImported := map[string]string{
		(&mattermost.Reaction{PostID: "post1", UserID: "mm-bob", EmojiName: "tada"}).Key(): "$old:example.com",
	}

	reactions := []mattermost.Reaction{
		{PostID: "post1", UserID: "mm-alice", EmojiName: "+1", CreateAt: 1700000000000},
		{PostID: "post1", UserID: "mm-alice", EmojiName: "party_parrot", CreateAt: 1700000000001}, // custom
		{PostID: "post1", UserID: "mm-bob", EmojiName: "tada", CreateAt: 1700000000002},           // already sent
		{PostID: "post1", UserID: "mm-carol", EmojiName: "+1", CreateAt: 1700000000003},           // left the room
		{PostID: "post1", UserID: "mm-dave", EmojiName: "+1", CreateAt: 1700000000004},            // not mapped
		{PostID: "post9", UserID: "mm-alice", EmojiName: "+1", CreateAt: 1700000000005},           // post not imported
	}

	importer.importReactions(result, reactions, roomByPost, userMapping, alreadyImported, nil)

	if got := result.Stats.ReactionsImported; got != 2 {
		t.Errorf("imported = %d, want 2 (thumbs up and the custom emoji)", got)
	}
	if got := result.Stats.ReactionsSkipped; got != 4 {
		t.Errorf("skipped = %d, want 4 (already sent, left room, unmapped user, missing post)", got)
	}
	if got := result.Stats.ReactionsFailed; got != 0 {
		t.Errorf("failed = %d, want 0: a departed member is a skip, not a failure", got)
	}
	if got := result.Stats.ReactionsCustomEmoji; got != 1 {
		t.Errorf("custom_emoji = %d, want 1", got)
	}

	if len(sentKeys) != 2 || sentKeys[0] != "👍" || sentKeys[1] != ":party_parrot:" {
		t.Errorf("sent keys = %#v, want [👍 :party_parrot:]", sentKeys)
	}

	// Only the newly sent reactions are recorded; the one carried in from a previous run is
	// already in the caller's mapping and must not be duplicated there.
	if got := len(result.ReactionMapping); got != 2 {
		t.Errorf("recorded %d reactions, want 2: %#v", got, result.ReactionMapping)
	}
}

func TestSendReactionWithoutASTokenOmitsImpersonation(t *testing.T) {
	// Without an AS token Synapse would reject user_id and ts outright, so they must not be
	// sent: the reaction lands as the admin, now, rather than failing.
	srv, sent := newFakeReactionServer(t)
	c := NewClient(srv.URL, "admin-token", "example.com")

	if _, err := c.SendReactionWithTimestamp("!room:example.com", "$target:example.com", "👍",
		1700000000000, "@alice:example.com"); err != nil {
		t.Fatalf("SendReactionWithTimestamp returned error: %v", err)
	}

	if got := sent.query.Get("user_id"); got != "" {
		t.Errorf("user_id = %q, want it absent without an AS token", got)
	}
	if got := sent.query.Get("ts"); got != "" {
		t.Errorf("ts = %q, want it absent without an AS token", got)
	}
}
