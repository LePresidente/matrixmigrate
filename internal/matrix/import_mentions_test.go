package matrix

import "testing"

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

