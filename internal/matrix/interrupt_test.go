package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aligundogdu/matrixmigrate/internal/mattermost"
)

// requestCounter is an httptest server that answers every Matrix call it sees and counts them.
// onSend runs after each message send is counted, with the running total of sends.
type requestCounter struct {
	mu     sync.Mutex
	total  int
	sends  int
	onSend func(sends int)
}

func (rc *requestCounter) counts() (total, sends int) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.total, rc.sends
}

func newRequestCounter(t *testing.T, rc *requestCounter) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc.mu.Lock()
		rc.total++
		isSend := strings.Contains(r.URL.Path, "/send/m.room.message/")
		if isSend {
			rc.sends++
		}
		sends := rc.sends
		rc.mu.Unlock()

		switch {
		case isSend:
			if rc.onSend != nil {
				rc.onSend(sends)
			}
			_, _ = fmt.Fprintf(w, `{"event_id":"$e%d"}`, sends)
		case strings.HasSuffix(r.URL.Path, "/members"):
			_ = json.NewEncoder(w).Encode(map[string][]string{"members": {"@alice:example.com", "@bob_dev:example.com"}})
		case strings.Contains(r.URL.Path, "/account/whoami"):
			_ = json.NewEncoder(w).Encode(map[string]string{"user_id": "@admin:example.com"})
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func interruptTestPosts(n int) []mattermost.Post {
	posts := make([]mattermost.Post, n)
	for idx := range posts {
		author := "u-alice"
		if idx%2 == 1 {
			author = "u-bob"
		}
		posts[idx] = mattermost.Post{
			ID:        fmt.Sprintf("p%d", idx),
			ChannelID: "c1",
			UserID:    author,
			Message:   fmt.Sprintf("message %d", idx),
			CreateAt:  int64(1000 + idx),
		}
	}
	return posts
}

var interruptTestUsers = map[string]string{
	"u-alice": "@alice:example.com",
	"u-bob":   "@bob_dev:example.com",
}

func TestImportMessagesWithCancelledContextSendsNothing(t *testing.T) {
	rc := &requestCounter{}
	srv := newRequestCounter(t, rc)
	c := NewClientWithRateLimit(srv.URL, "admin-token", "example.com", RateLimitConfig{})
	c.SetASToken("as-token")
	i := NewImporter(c)
	i.SetContext(cancelledContext())

	posts := interruptTestPosts(4)
	reactions := &ReactionImport{Reactions: []mattermost.Reaction{{UserID: "u-alice", PostID: "p0", EmojiName: "smile"}}}
	result, err := i.ImportMessagesWithFiles(posts, map[string]string{"c1": "!r:example.com"}, interruptTestUsers,
		map[string]string{"p-old": "$old"}, nil, nil, reactions, &PinImport{}, nil)
	if err != nil {
		t.Fatalf("ImportMessagesWithFiles: %v", err)
	}

	if total, _ := rc.counts(); total != 0 {
		t.Errorf("an interrupted import made %d requests, want 0", total)
	}
	if len(result.Mapping) != 1 || result.Mapping["p-old"] != "$old" {
		t.Errorf("mapping = %v, want only the existing entry", result.Mapping)
	}
}

func TestImportMessagesStopsAfterCurrentItemWhenCancelled(t *testing.T) {
	const sentBeforeInterrupt = 3
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rc := &requestCounter{onSend: func(sends int) {
		if sends == sentBeforeInterrupt {
			cancel()
		}
	}}
	srv := newRequestCounter(t, rc)
	c := NewClientWithRateLimit(srv.URL, "admin-token", "example.com", RateLimitConfig{})
	c.SetASToken("as-token")
	i := NewImporter(c)
	i.SetContext(ctx)

	result, err := i.ImportMessagesWithFiles(interruptTestPosts(10), map[string]string{"c1": "!r:example.com"}, interruptTestUsers,
		nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("ImportMessagesWithFiles: %v", err)
	}

	if _, sends := rc.counts(); sends != sentBeforeInterrupt {
		t.Errorf("server saw %d sends, want %d", sends, sentBeforeInterrupt)
	}
	if len(result.Mapping) != sentBeforeInterrupt {
		t.Errorf("mapping holds %d entries, want %d: %v", len(result.Mapping), sentBeforeInterrupt, result.Mapping)
	}
	for idx := 0; idx < sentBeforeInterrupt; idx++ {
		if _, ok := result.Mapping[fmt.Sprintf("p%d", idx)]; !ok {
			t.Errorf("post p%d was sent but is missing from the mapping", idx)
		}
	}
}

func TestImportUsersWithCancelledContextCreatesNothing(t *testing.T) {
	rc := &requestCounter{}
	srv := newRequestCounter(t, rc)
	c := NewClientWithRateLimit(srv.URL, "admin-token", "example.com", RateLimitConfig{})
	i := NewImporter(c)
	i.SetContext(cancelledContext())

	users := []mattermost.User{
		{ID: "u-alice", Username: "alice", Email: "alice@example.com"},
		{ID: "u-bob", Username: "bob_dev", Email: "bob@example.com"},
	}
	mapping, stats, err := i.ImportUsers(users, map[string]string{"u-old": "@old:example.com"}, nil)
	if err != nil {
		t.Fatalf("ImportUsers: %v", err)
	}
	if total, _ := rc.counts(); total != 0 {
		t.Errorf("an interrupted user import made %d requests, want 0", total)
	}
	if len(mapping) != 1 || mapping["u-old"] != "@old:example.com" {
		t.Errorf("mapping = %v, want only the existing entry", mapping)
	}
	if stats.UsersCreated != 0 {
		t.Errorf("UsersCreated = %d, want 0", stats.UsersCreated)
	}
}

func TestImportTeamsWithCancelledContextCreatesNothing(t *testing.T) {
	rc := &requestCounter{}
	srv := newRequestCounter(t, rc)
	c := NewClientWithRateLimit(srv.URL, "admin-token", "example.com", RateLimitConfig{})
	i := NewImporter(c)
	i.SetContext(cancelledContext())

	teams := []mattermost.Team{{ID: "t1", Name: "eng", DisplayName: "Engineering"}}
	mapping, _, err := i.ImportTeamsAsSpaces(teams, nil, interruptTestUsers, nil, nil)
	if err != nil {
		t.Fatalf("ImportTeamsAsSpaces: %v", err)
	}
	if total, _ := rc.counts(); total != 0 {
		t.Errorf("an interrupted space import made %d requests, want 0", total)
	}
	if len(mapping) != 0 {
		t.Errorf("mapping = %v, want empty", mapping)
	}
}

// A request waiting to retry after a 429 must give up as soon as the run is interrupted,
// rather than sleeping out a Retry-After that can be a minute long.
func TestRetryWaitEndsWhenContextCancelled(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	firstSeen := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		if requests == 1 {
			close(firstSeen)
		}
		mu.Unlock()
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"errcode":"M_LIMIT_EXCEEDED"}`))
	}))
	t.Cleanup(srv.Close)

	for _, tc := range []struct {
		name string
		call func(c *Client) error
	}{
		{"admin token", func(c *Client) error { _, _, err := c.doRequest("GET", "/x", nil); return err }},
		{"explicit token", func(c *Client) error { _, _, err := c.doRequestWithToken("GET", "/x", nil, "as-token"); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			requests = 0
			firstSeen = make(chan struct{})
			seen := firstSeen
			mu.Unlock()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := NewClientWithRateLimit(srv.URL, "admin-token", "example.com", RateLimitConfig{MaxRetries: 5, RetryBaseDelay: 10 * time.Second})
			c.SetContext(ctx)
			go func() {
				<-seen
				cancel()
			}()

			start := time.Now()
			err := tc.call(c)
			elapsed := time.Since(start)

			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want one wrapping context.Canceled", err)
			}
			if elapsed > 5*time.Second {
				t.Errorf("request took %v to give up; the retry wait ignored the cancellation", elapsed)
			}
			mu.Lock()
			defer mu.Unlock()
			if requests != 1 {
				t.Errorf("server saw %d requests, want 1 (no retry after the interrupt)", requests)
			}
		})
	}
}

// After an interrupt the cleanup calls (leaving rooms, admin withdrawal) must still go out,
// and still be spaced by the rate limiter so they do not provoke 429s that can no longer be
// retried.
func TestRateLimitSpacingKeptWhenContextCancelled(t *testing.T) {
	const interval = 100 * time.Millisecond
	rc := &requestCounter{}
	srv := newRequestCounter(t, rc)
	c := NewClientWithRateLimit(srv.URL, "admin-token", "example.com", RateLimitConfig{RequestsPerSecond: 10})
	c.SetContext(cancelledContext())

	start := time.Now()
	for n := 0; n < 3; n++ {
		if _, _, err := c.doRequest("GET", "/x", nil); err != nil {
			t.Fatalf("request %d: %v", n, err)
		}
	}
	// The counter answers {} with no content_uri, so the upload reports an error; what matters
	// here is only that it was sent, after its slot.
	_, _ = c.UploadMedia([]byte("x"), "a.txt", "text/plain")
	elapsed := time.Since(start)

	// The first request goes at once; the other three each wait for a slot.
	if want := 3 * interval; elapsed < want-10*time.Millisecond {
		t.Errorf("4 requests took %v, want at least %v: the rate-limit spacing was skipped", elapsed, want)
	}
	if total, _ := rc.counts(); total != 4 {
		t.Errorf("server saw %d requests, want 4", total)
	}
}
