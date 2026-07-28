package matrix

import (
	"strings"
	"testing"
)

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

