package matrix

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aligundogdu/matrixmigrate/internal/mattermost"
)

// joinOutcome is how the fake homeserver answers a force-join: an HTTP status, or 0 to drop
// the connection without answering (a transport error).
type joinOutcome struct {
	name   string
	status int
	kept   bool // whether the pair must stay tracked for cleanup
}

var joinOutcomes = []joinOutcome{
	{"502 from a proxy", http.StatusBadGateway, true},
	{"transport error", 0, true},
	{"403 refusal", http.StatusForbidden, false},
}

// failingJoinServer is a homeserver whose room has no members and whose force-join answers
// with outcome. Sends are refused as "not in room".
func failingJoinServer(t *testing.T, outcome joinOutcome) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/members"):
			_ = json.NewEncoder(w).Encode(map[string][]string{"members": {}})
		case strings.Contains(r.URL.Path, "/_synapse/admin/v1/join/"):
			if outcome.status == 0 {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
				return
			}
			w.WriteHeader(outcome.status)
			_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN","error":"join failed"}`))
		case strings.Contains(r.URL.Path, "/account/whoami"):
			_, _ = w.Write([]byte(`{"user_id":"@bot:example.com"}`))
		case strings.Contains(r.URL.Path, "/send/m.room.message/"):
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprintf(w, `{"errcode":"M_FORBIDDEN","error":"User %s not in room !room"}`, r.URL.Query().Get("user_id"))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func failingJoinImporter(t *testing.T, outcome joinOutcome, asToken bool) *Importer {
	t.Helper()
	srv := failingJoinServer(t, outcome)
	c := NewClientWithRateLimit(srv.URL, "admin-token", "example.com", RateLimitConfig{MaxRetries: 1, RetryBaseDelay: time.Millisecond})
	if asToken {
		c.SetASToken("as-token")
	}
	return NewImporter(c)
}

func tracks(memberships []HistoryMembership, want HistoryMembership) bool {
	for _, m := range memberships {
		if m == want {
			return true
		}
	}
	return false
}

// A force-join that fails without a definite refusal may still have been applied, so the pair
// must stay tracked: withdrawn by the cleanup or, failing that, left in Remaining.
func TestPastAuthorJoinWithAmbiguousFailureStaysTracked(t *testing.T) {
	for _, outcome := range joinOutcomes {
		t.Run(outcome.name, func(t *testing.T) {
			// No AS token: the cleanup cannot leave, so whatever is tracked ends in Remaining.
			i := failingJoinImporter(t, outcome, false)
			joined := i.ensureHistoryAuthorsJoined(
				[]mattermost.Post{{ID: "p1", ChannelID: "c1", UserID: "u_bob", Message: "hi"}},
				map[string]string{"c1": "!room"},
				map[string]string{"u_bob": "@bob_dev:example.com"})
			i.AddHistoryJoins(joined...)

			pair := HistoryMembership{RoomID: "!room", UserID: "@bob_dev:example.com"}
			if got := tracks(i.LeaveHistoryMemberships().Remaining, pair); got != outcome.kept {
				t.Errorf("pair in Remaining = %v, want %v", got, outcome.kept)
			}
		})
	}
}

func TestSendRecoveryJoinWithAmbiguousFailureStaysTracked(t *testing.T) {
	for _, outcome := range joinOutcomes {
		t.Run(outcome.name, func(t *testing.T) {
			i := failingJoinImporter(t, outcome, true)
			_, _, _ = i.sendWithMembershipRecovery("!room", "@bob_dev:example.com", func(sender string) (*SendMessageResponse, error) {
				return i.client.SendMessageWithTimestamp("!room", "hello", 1, sender)
			})
			pair := HistoryMembership{RoomID: "!room", UserID: "@bob_dev:example.com"}
			if got := tracks(i.HistoryJoins(), pair); got != outcome.kept {
				t.Errorf("pair tracked = %v, want %v (history joins %v)", got, outcome.kept, i.HistoryJoins())
			}
		})
	}
}

func TestFallbackSenderJoinWithAmbiguousFailureStaysTracked(t *testing.T) {
	for _, outcome := range joinOutcomes {
		t.Run(outcome.name, func(t *testing.T) {
			i := failingJoinImporter(t, outcome, true)
			if err := i.ensureFallbackSenderInRoom("!room"); err == nil {
				t.Fatal("expected the failed join to be reported")
			}
			pair := HistoryMembership{RoomID: "!room", UserID: "@bot:example.com"}
			if got := tracks(i.HistoryJoins(), pair); got != outcome.kept {
				t.Errorf("pair tracked = %v, want %v (history joins %v)", got, outcome.kept, i.HistoryJoins())
			}
		})
	}
}
