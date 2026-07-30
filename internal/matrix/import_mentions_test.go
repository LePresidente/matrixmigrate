package matrix

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mentionTestImporter returns an importer whose homeserver reports only the given localparts as
// existing users, plus a counter of how many existence lookups reached the server.
func mentionTestImporter(t *testing.T, existing ...string) (*Importer, *int) {
	t.Helper()

	onServer := make(map[string]struct{}, len(existing))
	for _, localpart := range existing {
		onServer[localpart] = struct{}{}
	}

	lookups := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookups++
		// Path is /_synapse/admin/v2/users/@localpart:example.com
		userID := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
		if _, ok := onServer[mentionLocalpart(userID)]; ok {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"name":"` + userID + `"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"errcode":"M_NOT_FOUND"}`))
	}))
	t.Cleanup(srv.Close)

	client := NewClientWithRateLimit(srv.URL, "token", "example.com", RateLimitConfig{
		RequestsPerSecond: 1000,
		MaxRetries:        1,
		RetryBaseDelay:    time.Millisecond,
	})
	return &Importer{client: client}, &lookups
}

func TestNormalizeMatrixMentionsVerifiesUsers(t *testing.T) {
	tests := []struct {
		name     string
		mapping  map[string]string
		existing []string
		in       string
		want     string
	}{
		{
			name:    "domain in prose is not a mention",
			mapping: map[string]string{"mm1": "@alice:example.com"},
			in:      "Username: same as email without @example.com\npassword: 000000",
			want:    "Username: same as email without @example.com\npassword: 000000",
		},
		{
			name:    "mapped user is still mentioned",
			mapping: map[string]string{"mm1": "@alice:example.com"},
			in:      "ping @alice please",
			want:    "ping @alice:example.com please",
		},
		{
			name:    "unmapped user absent from the homeserver stays plain",
			mapping: map[string]string{"mm1": "@alice:example.com"},
			in:      "ping @ghost please",
			want:    "ping @ghost please",
		},
		{
			name:     "unmapped user present on the homeserver is mentioned",
			mapping:  map[string]string{"mm1": "@alice:example.com"},
			existing: []string{"bob_dev"},
			in:       "ping @bob_dev please",
			want:     "ping @bob_dev:example.com please",
		},
		{
			name:    "mapping lookup ignores case",
			mapping: map[string]string{"mm1": "@alice:example.com"},
			in:      "ping @Alice please",
			want:    "ping @Alice:example.com please",
		},
		{
			name:     "broadcast mentions are untouched without a lookup",
			mapping:  map[string]string{"mm1": "@alice:example.com"},
			existing: []string{"here"},
			in:       "@here now",
			want:     "@here now",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			importer, _ := mentionTestImporter(t, tt.existing...)
			importer.SetKnownMentionUsers(tt.mapping)

			got := importer.normalizeMatrixMentions(tt.in)
			if got != tt.want {
				t.Fatalf("normalizeMatrixMentions() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMentionExistenceIsCachedPerName(t *testing.T) {
	importer, lookups := mentionTestImporter(t)
	importer.SetKnownMentionUsers(map[string]string{"mm1": "@alice:example.com"})

	// @alice comes from the mapping and must never be looked up; @ghost is unknown and must be
	// looked up exactly once however many times it appears.
	importer.normalizeMatrixMentions("@alice @ghost @ghost @alice @ghost")

	if *lookups != 1 {
		t.Fatalf("want 1 homeserver lookup, got %d", *lookups)
	}
}

func TestMentionUnverifiableStaysPlainText(t *testing.T) {
	// No server: the existence check errors, and an unverifiable name must not become a pill.
	client := NewClientWithRateLimit("http://127.0.0.1:1", "token", "example.com", RateLimitConfig{
		RequestsPerSecond: 1000,
		MaxRetries:        1,
		RetryBaseDelay:    time.Millisecond,
	})
	importer := &Importer{client: client}
	importer.SetKnownMentionUsers(map[string]string{"mm1": "@alice:example.com"})

	got := importer.normalizeMatrixMentions("@alice and @example.com")
	if got != "@alice:example.com and @example.com" {
		t.Fatalf("normalizeMatrixMentions() = %q", got)
	}
}

func TestMentionsUngatedWithoutRoster(t *testing.T) {
	// Callers that never set a roster keep the old unconditional behaviour, with no lookups.
	importer, lookups := mentionTestImporter(t)

	got := importer.normalizeMatrixMentions("ping @ghost")
	if got != "ping @ghost:example.com" {
		t.Fatalf("normalizeMatrixMentions() = %q", got)
	}
	if *lookups != 0 {
		t.Fatalf("want no homeserver lookups, got %d", *lookups)
	}
}

func TestMentionsUnderscorePreserved(t *testing.T) {
	i := &Importer{client: &Client{homeserver: "example.com"}}

	body, html, ids := i.renderMentions("@alice_dev @bob_dev hi")

	if body != "@alice_dev:example.com @bob_dev:example.com hi" {
		t.Fatalf("body lost underscore: %q", body)
	}
	if !strings.Contains(html, "matrix.to/#/@alice_dev:example.com") ||
		!strings.Contains(html, "matrix.to/#/@bob_dev:example.com") {
		t.Fatalf("missing pills: %q", html)
	}
	if strings.Contains(html, "<em>") {
		t.Fatalf("underscore was italicised: %q", html)
	}
	if len(ids) != 2 {
		t.Fatalf("want 2 mention ids, got %v: %v", len(ids), ids)
	}
}

func TestNormalizeMatrixMentions(t *testing.T) {
	importer := &Importer{
		client: &Client{homeserver: "example.com"},
	}

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "single mention",
			in:   "hello @alice",
			want: "hello @alice:example.com",
		},
		{
			name: "multiple mentions",
			in:   "@alice please check with @bob-dev",
			want: "@alice:example.com please check with @bob-dev:example.com",
		},
		{
			name: "already matrix id stays unchanged",
			in:   "ping @alice:example.com",
			want: "ping @alice:example.com",
		},
		{
			name: "email is not changed",
			in:   "mail me at alice@example.com",
			want: "mail me at alice@example.com",
		},
		{
			name: "broadcast mentions are not changed",
			in:   "@here @all @channel",
			want: "@here @all @channel",
		},
		{
			name: "trailing period is preserved outside mention",
			in:   "thanks @alice.",
			want: "thanks @alice:example.com.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := importer.normalizeMatrixMentions(tt.in)
			if got != tt.want {
				t.Fatalf("normalizeMatrixMentions() = %q, want %q", got, tt.want)
			}
		})
	}
}
