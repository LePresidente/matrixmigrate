package matrix

import (
	"strings"
	"testing"

	"github.com/aligundogdu/matrixmigrate/internal/mattermost"
)

func TestUnmappedChannelReason(t *testing.T) {
	users := []mattermost.User{
		{ID: "u1", Username: "alice", DeleteAt: 0},
		{ID: "u2", Username: "bob", DeleteAt: 111},
		{ID: "u3", Username: "carol", DeleteAt: 222},
	}

	tests := []struct {
		name     string
		ch       mattermost.Channel
		known    bool
		contains string
	}{
		{
			name:     "channel missing from the export",
			ch:       mattermost.Channel{},
			known:    false,
			contains: "not present in the exported assets",
		},
		{
			name:     "deleted channel",
			ch:       mattermost.Channel{ID: "c1", Type: "O", DeleteAt: 999},
			known:    true,
			contains: "deleted in Mattermost",
		},
		{
			name:     "direct channel",
			ch:       mattermost.Channel{ID: "c2", Type: "D"},
			known:    true,
			contains: "direct channel",
		},
		{
			name:     "group with only locked participants",
			ch:       mattermost.Channel{ID: "c3", Type: "G", DisplayName: "bob, carol"},
			known:    true,
			contains: "all locked or deleted",
		},
		{
			// The case that made the counts disagree: a channel that should have become a
			// room and did not. Nothing about it is deliberate, so it must not read like
			// the others.
			name:     "everything else points at a failed creation",
			ch:       mattermost.Channel{ID: "c4", Type: "O", DisplayName: "General"},
			known:    true,
			contains: "room creation did not succeed",
		},
		{
			name:     "group with a live participant is not locked-only",
			ch:       mattermost.Channel{ID: "c5", Type: "G", DisplayName: "alice, bob"},
			known:    true,
			contains: "room creation did not succeed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unmappedChannelReason(tt.ch, tt.known, users)
			if !strings.Contains(got, tt.contains) {
				t.Fatalf("unmappedChannelReason() = %q, want it to mention %q", got, tt.contains)
			}
		})
	}
}
