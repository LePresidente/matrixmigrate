package matrix

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newFakeSendServer records the content of whatever event it is asked to send.
func newFakeSendServer(t *testing.T) (*httptest.Server, *map[string]any) {
	t.Helper()
	content := map[string]any{}

	mux := http.NewServeMux()
	mux.HandleFunc("/_matrix/client/v3/rooms/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		for k := range content {
			delete(content, k)
		}
		json.Unmarshal(body, &content)
		w.Write([]byte(`{"event_id":"$evt:example.com"}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &content
}

// requireEmptyMentions asserts that m.mentions is present and mentions nobody.
//
// Presence is the whole point: per MSC3952 the legacy push rules — which match on the message
// text, not on any declared intent — apply only while m.mentions is missing. Drop it and every
// migrated message containing someone's display name notifies them years after the fact.
func requireEmptyMentions(t *testing.T, content map[string]any) {
	t.Helper()

	raw, present := content["m.mentions"]
	if !present {
		t.Fatalf("m.mentions is absent; legacy push rules would apply: %#v", content)
	}
	mentions, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("m.mentions is not an object: %#v", raw)
	}
	if len(mentions) != 0 {
		t.Fatalf("m.mentions should be empty, got %#v", mentions)
	}
}

func TestSendMessageAlwaysDeclaresMentions(t *testing.T) {
	srv, content := newFakeSendServer(t)
	c := NewClient(srv.URL, "admin-token", "example.com")

	if _, err := c.SendMessage("!room:example.com", "just a plain message, no mentions"); err != nil {
		t.Fatalf("SendMessage returned error: %v", err)
	}
	requireEmptyMentions(t, *content)
}

func TestSendMessageCarriesRealMentions(t *testing.T) {
	srv, content := newFakeSendServer(t)
	c := NewClient(srv.URL, "admin-token", "example.com")

	if _, err := c.SendMessage("!room:example.com", "ping @alice:example.com"); err != nil {
		t.Fatalf("SendMessage returned error: %v", err)
	}

	mentions, ok := (*content)["m.mentions"].(map[string]any)
	if !ok {
		t.Fatalf("m.mentions missing: %#v", *content)
	}
	ids, ok := mentions["user_ids"].([]any)
	if !ok || len(ids) != 1 || ids[0] != "@alice:example.com" {
		t.Fatalf("user_ids = %#v, want [@alice:example.com]", mentions["user_ids"])
	}
}

func TestSendReplyAlwaysDeclaresMentions(t *testing.T) {
	srv, content := newFakeSendServer(t)
	c := NewClient(srv.URL, "admin-token", "example.com")

	if _, err := c.SendReplyWithTimestamp("!room:example.com", "no mentions here",
		"$parent:example.com", "", 0, ""); err != nil {
		t.Fatalf("SendReplyWithTimestamp returned error: %v", err)
	}
	requireEmptyMentions(t, *content)
}

func TestSendFileAlwaysDeclaresMentions(t *testing.T) {
	// The body of a file event is the filename, which the legacy display-name rule matches
	// just as happily as prose.
	srv, content := newFakeSendServer(t)
	c := NewClient(srv.URL, "admin-token", "example.com")

	if _, err := c.SendUploadedFile("!room:example.com", "mxc://example.com/abc",
		"Angebot_Anna.pdf", "application/pdf", 1024, 0, 0, 0, ""); err != nil {
		t.Fatalf("SendUploadedFile returned error: %v", err)
	}
	requireEmptyMentions(t, *content)
}

func TestSendFileAsReplyAlwaysDeclaresMentions(t *testing.T) {
	srv, content := newFakeSendServer(t)
	c := NewClient(srv.URL, "admin-token", "example.com")

	if _, err := c.SendUploadedFileAsReply("!room:example.com", "mxc://example.com/abc",
		"Angebot_Anna.pdf", "application/pdf", 1024, 0, 0, "$parent:example.com", "", 0, ""); err != nil {
		t.Fatalf("SendUploadedFileAsReply returned error: %v", err)
	}
	requireEmptyMentions(t, *content)
}

func TestMigratedMessagesNeverClaimARoomMention(t *testing.T) {
	// @all becomes the text @room so the archive reads correctly, but the migration must never
	// declare it as an actual room mention: with preserve_owner_and_alias the channel creator
	// holds PL 100, which is above the notifications.room threshold, so .m.rule.is_room_mention
	// would fire and ping everyone about a years-old announcement.
	srv, content := newFakeSendServer(t)
	c := NewClient(srv.URL, "admin-token", "example.com")

	if _, err := c.SendMessage("!room:example.com", "@room please fill in the form"); err != nil {
		t.Fatalf("SendMessage returned error: %v", err)
	}

	mentions, ok := (*content)["m.mentions"].(map[string]any)
	if !ok {
		t.Fatalf("m.mentions missing: %#v", *content)
	}
	if _, claimed := mentions["room"]; claimed {
		t.Fatalf("m.mentions.room must never be set by the migration: %#v", mentions)
	}
}
