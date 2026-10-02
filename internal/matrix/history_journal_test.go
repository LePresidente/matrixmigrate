package matrix

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aligundogdu/matrixmigrate/internal/mattermost"
)

// orderedLog is a shared, ordered record of what happened, written to by both the fake
// server and the test's join recorder so their relative order can be asserted.
type orderedLog struct {
	mu      sync.Mutex
	entries []string
}

func (l *orderedLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, s)
}

func (l *orderedLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.entries...)
}

func (l *orderedLog) count(prefix string) int {
	n := 0
	for _, e := range l.all() {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

// historyServer is a fake homeserver for the history-join paths. The member list either
// fails (membersFail) or reports members; sends are refused with "not in room" for anyone
// who is neither a listed member nor force-joined since. Force-joins are logged as
// "join:<user>", sends as "send:<user>".
func historyServer(t *testing.T, members []string, membersFail bool, log *orderedLog) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	inRoom := map[string]bool{}
	for _, m := range members {
		inRoom[m] = true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/members") && r.Method == http.MethodGet:
			if membersFail {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN","error":"boom"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"members": members})

		case strings.Contains(path, "/_synapse/admin/v1/join/"):
			var body struct {
				UserID string `json:"user_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			inRoom[body.UserID] = true
			mu.Unlock()
			log.add("join:" + body.UserID)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))

		case strings.Contains(path, "/account/whoami"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"user_id":"@bot:example.com"}`))

		case strings.Contains(path, "/send/m.room.message/"):
			user := r.URL.Query().Get("user_id")
			if user == "" {
				user = "@bot:example.com"
			}
			mu.Lock()
			ok := inRoom[user]
			mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusForbidden)
				_, _ = fmt.Fprintf(w, `{"errcode":"M_FORBIDDEN","error":"User %s not in room !room"}`, user)
				return
			}
			log.add("send:" + user)
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"event_id":"$%d"}`, len(log.all()))

		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"room_id":"!room"}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestEnsureHistoryAuthorsJoinedSkipsRoomWhenMemberListFails(t *testing.T) {
	// Without the member list there is no telling a past author from a current member.
	// Joining "everyone" would record real members as joined-by-us, and the cleanup would
	// then remove them. The room must be left alone.
	log := &orderedLog{}
	srv := historyServer(t, nil, true, log)
	c := NewClient(srv.URL, "admin-token", "example.com")
	i := NewImporter(c)
	var recorded []HistoryMembership
	i.SetHistoryJoinRecorder(func(m HistoryMembership) { recorded = append(recorded, m) })

	got := i.ensureHistoryAuthorsJoined(
		[]mattermost.Post{{ID: "p1", ChannelID: "c1", UserID: "u_alice", Message: "hi"}},
		map[string]string{"c1": "!room"},
		map[string]string{"u_alice": "@alice:example.com"})

	if len(got) != 0 {
		t.Fatalf("expected no joins when the member list fails, got %#v", got)
	}
	if n := log.count("join:"); n != 0 {
		t.Fatalf("expected no force-join requests, got %v", log.all())
	}
	if len(recorded) != 0 {
		t.Fatalf("expected nothing recorded, got %#v", recorded)
	}
}

func TestHistoryJoinRecorderRunsBeforeForceJoin(t *testing.T) {
	log := &orderedLog{}
	srv := historyServer(t, []string{"@alice:example.com"}, false, log)
	c := NewClient(srv.URL, "admin-token", "example.com")
	i := NewImporter(c)
	i.SetHistoryJoinRecorder(func(m HistoryMembership) { log.add("record:" + m.UserID + "@" + m.RoomID) })

	i.ensureHistoryAuthorsJoined(
		[]mattermost.Post{
			{ID: "p1", ChannelID: "c1", UserID: "u_alice", Message: "hi"},
			{ID: "p2", ChannelID: "c1", UserID: "u_bob", Message: "bye"},
		},
		map[string]string{"c1": "!room"},
		map[string]string{"u_alice": "@alice:example.com", "u_bob": "@bob_dev:example.com"})

	got := log.all()
	want := []string{"record:@bob_dev:example.com@!room", "join:@bob_dev:example.com"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("expected the record to precede the join: want %v, got %v", want, got)
	}
}

func TestSendRecoveryRecordsBeforeJoin(t *testing.T) {
	log := &orderedLog{}
	srv := historyServer(t, nil, false, log)
	c := NewClient(srv.URL, "admin-token", "example.com")
	c.SetASToken("as-token")
	i := NewImporter(c)
	i.SetHistoryJoinRecorder(func(m HistoryMembership) { log.add("record:" + m.UserID) })

	_, _, err := i.sendWithMembershipRecovery("!room", "@bob_dev:example.com", func(sender string) (*SendMessageResponse, error) {
		return c.SendMessageWithTimestamp("!room", "hello", 1, sender)
	})
	if err != nil {
		t.Fatalf("expected recovery to succeed, got %v", err)
	}
	got := strings.Join(log.all(), ",")
	want := "record:@bob_dev:example.com,join:@bob_dev:example.com,send:@bob_dev:example.com"
	if got != want {
		t.Fatalf("want %s, got %s", want, got)
	}
}

func TestFallbackSenderRecordsBeforeJoin(t *testing.T) {
	log := &orderedLog{}
	srv := historyServer(t, nil, false, log)
	c := NewClient(srv.URL, "admin-token", "example.com")
	c.SetASToken("as-token")
	i := NewImporter(c)
	i.SetHistoryJoinRecorder(func(m HistoryMembership) { log.add("record:" + m.UserID) })

	if err := i.ensureFallbackSenderInRoom("!room"); err != nil {
		t.Fatalf("expected the fallback sender to be joined, got %v", err)
	}
	got := log.all()
	if len(got) < 2 || !strings.HasPrefix(got[0], "record:") || !strings.HasPrefix(got[1], "join:") {
		t.Fatalf("expected the record to precede the join, got %v", got)
	}
}

func TestImportRecoversReplyFromAbsentAuthor(t *testing.T) {
	// The member list is unreadable, so the pre-pass joins nobody. alice's root message and
	// bob's reply must both be recovered one by one when the homeserver says "not in room".
	log := &orderedLog{}
	srv := historyServer(t, nil, true, log)
	c := NewClient(srv.URL, "admin-token", "example.com")
	c.SetASToken("as-token")
	i := NewImporter(c)

	posts := []mattermost.Post{
		{ID: "root", ChannelID: "c1", UserID: "u_alice", Message: "question", CreateAt: 1},
		{ID: "reply", ChannelID: "c1", UserID: "u_bob", RootID: "root", Message: "answer", CreateAt: 2},
	}
	res, err := i.ImportMessagesWithFiles(posts,
		map[string]string{"c1": "!room"},
		map[string]string{"u_alice": "@alice:example.com", "u_bob": "@bob_dev:example.com"},
		nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}
	if res.Stats.RepliesImported != 1 || res.Stats.RepliesFailed != 0 {
		t.Fatalf("expected the reply to be recovered, got %+v (errors %v)", res.Stats, res.Errors)
	}
	if log.count("send:@bob_dev:example.com") != 1 {
		t.Fatalf("expected the reply to land as bob, got %v", log.all())
	}
	found := false
	for _, hm := range i.HistoryJoins() {
		if hm.UserID == "@bob_dev:example.com" && hm.RoomID == "!room" {
			found = true
		}
	}
	if !found {
		t.Fatalf("recovery join for bob should be recorded, got %#v", i.HistoryJoins())
	}
}

func TestLeaveHistoryMembershipsReportsFailedLeaveInRemaining(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/state/m.room.power_levels"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"users":{}}`))
		case strings.HasSuffix(path, "/leave"):
			if r.URL.Query().Get("user_id") == "@stuck:example.com" {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN","error":"boom"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case strings.Contains(path, "/account/whoami"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"user_id":"@admin:example.com"}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL, "admin-token", "example.com")
	c.SetASToken("as-token")
	i := NewImporter(c)
	i.AddHistoryJoins(
		HistoryMembership{RoomID: "!r1", UserID: "@alice:example.com"},
		HistoryMembership{RoomID: "!r1", UserID: "@stuck:example.com"},
	)

	res := i.LeaveHistoryMemberships()
	if res.Left != 1 || res.Failed != 1 {
		t.Fatalf("expected 1 left and 1 failed, got %+v", res)
	}
	if len(res.Remaining) != 1 || res.Remaining[0] != (HistoryMembership{RoomID: "!r1", UserID: "@stuck:example.com"}) {
		t.Fatalf("only the failed leave should remain, got %#v", res.Remaining)
	}
}

func TestLeaveHistoryMembershipsWithoutASTokenKeepsAllRemaining(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "admin-token", "example.com")
	i := NewImporter(c)
	i.AddHistoryJoins(
		HistoryMembership{RoomID: "!r1", UserID: "@alice:example.com"},
		HistoryMembership{RoomID: "!r1", UserID: "@alice:example.com"},
	)
	res := i.LeaveHistoryMemberships()
	if len(res.Remaining) != 1 {
		t.Fatalf("a skipped membership must stay remaining (once), got %#v", res.Remaining)
	}
}
