package mattermost

import "testing"

func TestDMParticipantIDs(t *testing.T) {
	tests := []struct {
		name         string
		channelName  string
		wantSender   string
		wantReceiver string
		wantErr      bool
	}{
		{"double underscore", "alice__bob", "alice", "bob", false},
		{"single underscore", "alice_bob", "alice", "bob", false},
		// Mattermost names a note-to-self channel with the same ID on both sides.
		{"self-DM", "alice__alice", "alice", "alice", false},
		{"empty name", "", "", "", true},
		{"no separator", "alice", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Channel{ID: "c1", Type: "D", Name: tt.channelName}
			sender, receiver, err := c.DMParticipantIDs()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("DMParticipantIDs(%q) = (%q, %q, nil), want error", tt.channelName, sender, receiver)
				}
				return
			}
			if err != nil {
				t.Fatalf("DMParticipantIDs(%q) returned error: %v", tt.channelName, err)
			}
			if sender != tt.wantSender || receiver != tt.wantReceiver {
				t.Fatalf("DMParticipantIDs(%q) = (%q, %q), want (%q, %q)",
					tt.channelName, sender, receiver, tt.wantSender, tt.wantReceiver)
			}
		})
	}
}

func TestIsSystemMessage(t *testing.T) {
	tests := []struct {
		name string
		typ  string
		want bool
	}{
		{"normal post", "", false},
		{"join channel", "system_join_channel", true},
		{"header change", "system_header_change", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Post{Type: tt.typ}
			if got := p.IsSystemMessage(); got != tt.want {
				t.Fatalf("IsSystemMessage(type=%q) = %v, want %v", tt.typ, got, tt.want)
			}
		})
	}
}
