package matrix

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// forbiddenOnceServer refuses sends from a given user until they have been joined.
func forbiddenOnceServer(t *testing.T, room string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	joined := map[string]bool{}
	var sends []string

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/account/whoami"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"user_id": "@bot:example.com"})

		case strings.Contains(r.URL.Path, "/_synapse/admin/v1/join/"):
			var body struct {
				UserID string `json:"user_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			joined[body.UserID] = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))

		case strings.Contains(r.URL.Path, "/send/m.room.message/"):
			user := r.URL.Query().Get("user_id")
			if user == "" {
				user = "@bot:example.com"
			}
			mu.Lock()
			isMember := joined[user]
			if isMember {
				sends = append(sends, user)
			}
			mu.Unlock()
			if !isMember {
				w.WriteHeader(http.StatusForbidden)
				_, _ = fmt.Fprintf(w, `{"errcode":"M_FORBIDDEN","error":"User %s not in room %s"}`, user, room)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"event_id":"$ok"}`))

		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"room_id":"!r:example.com"}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), sends...) }
}

func TestSendWithMembershipRecoveryJoinsSenderAndRetries(t *testing.T) {
	srv, sends := forbiddenOnceServer(t, "!room")
	c := NewClient(srv.URL, "admin-token", "example.com")
	c.SetASToken("as-token")
	i := NewImporter(c)

	resp, note, err := i.sendWithMembershipRecovery("!room", "hello", 1, "@late:example.com")
	if err != nil {
		t.Fatalf("expected recovery to succeed, got %v", err)
	}
	if resp == nil || resp.EventID != "$ok" {
		t.Fatalf("expected an event id, got %#v", resp)
	}
	if !strings.Contains(note, "joined sender") {
		t.Fatalf("expected a join-recovery note, got %q", note)
	}
	got := sends()
	if len(got) != 1 || got[0] != "@late:example.com" {
		t.Fatalf("message should have landed as its real author, got %v", got)
	}
	// Recorded so the withdrawal pass removes what recovery added.
	if len(i.HistoryJoins()) != 1 || i.HistoryJoins()[0].UserID != "@late:example.com" {
		t.Fatalf("recovery join should be recorded, got %#v", i.HistoryJoins())
	}
}

func TestSendWithMembershipRecoveryLeavesOtherErrorsAlone(t *testing.T) {
	// A refusal that joining cannot fix must surface, not trigger a pointless join.
	mux := http.NewServeMux()
	joins := 0
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/_synapse/admin/v1/join/") {
			joins++
		}
		if strings.Contains(r.URL.Path, "/send/m.room.message/") {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errcode":"M_FORBIDDEN","error":"You don't have permission to post"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL, "admin-token", "example.com")
	c.SetASToken("as-token")
	i := NewImporter(c)

	if _, _, err := i.sendWithMembershipRecovery("!room", "x", 1, "@u:example.com"); err == nil {
		t.Fatal("expected the error to surface")
	}
	if joins != 0 {
		t.Fatalf("no join should be attempted for a non-membership refusal, got %d", joins)
	}
}
