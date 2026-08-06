package matrix

import "testing"

func TestReactionKey(t *testing.T) {
	tests := []struct {
		name       string
		emojiName  string
		wantKey    string
		wantCustom bool
	}{
		{"known shortcode", "+1", "👍", false},
		{"known shortcode with colons", ":smile:", "😄", false},
		{"surrounding whitespace is ignored", "  heart  ", "❤️", false},
		{"custom emoji falls back to literal", "party_parrot", ":party_parrot:", true},
		{"skin tone variant is not in gemoji", "+1_dark_skin_tone", ":+1_dark_skin_tone:", true},
		{"empty name", "", "", true},
		{"colons only", "::", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, custom := ReactionKey(tt.emojiName)
			if key != tt.wantKey {
				t.Errorf("ReactionKey(%q) key = %q, want %q", tt.emojiName, key, tt.wantKey)
			}
			if custom != tt.wantCustom {
				t.Errorf("ReactionKey(%q) custom = %v, want %v", tt.emojiName, custom, tt.wantCustom)
			}
		})
	}
}
