package mattermost

import "testing"

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
