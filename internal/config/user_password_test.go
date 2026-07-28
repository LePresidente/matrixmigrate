package config

import "testing"

func TestGetUserPasswordModeAuto(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		masEnabled bool
		want       string
	}{
		{"auto with MAS resolves to none", UserPasswordModeAuto, true, UserPasswordModeNone},
		{"auto without MAS resolves to random", UserPasswordModeAuto, false, UserPasswordModeRandom},
		{"unset with MAS resolves to none", "", true, UserPasswordModeNone},
		{"unset without MAS resolves to random", "", false, UserPasswordModeRandom},
		{"explicit random wins over MAS", UserPasswordModeRandom, true, UserPasswordModeRandom},
		{"explicit none without MAS", UserPasswordModeNone, false, UserPasswordModeNone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{}
			c.Matrix.Import.UserPassword.Mode = tt.mode
			c.Matrix.MAS.Enabled = tt.masEnabled

			if got := c.GetUserPasswordMode(); got != tt.want {
				t.Fatalf("GetUserPasswordMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetUserPasswordLength(t *testing.T) {
	c := &Config{}
	if got := c.GetUserPasswordLength(); got != 24 {
		t.Fatalf("unset length should default to 24, got %d", got)
	}

	c.Matrix.Import.UserPassword.Length = 32
	if got := c.GetUserPasswordLength(); got != 32 {
		t.Fatalf("configured length not honoured: got %d", got)
	}
}

func TestValidateUserPassword(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		length  int
		wantErr bool
	}{
		{"defaults are valid", "", 0, false},
		{"random with valid length", UserPasswordModeRandom, 24, false},
		{"bad mode rejected", "shared", 24, true},
		{"length below minimum rejected", UserPasswordModeRandom, 11, true},
		{"length above maximum rejected", UserPasswordModeRandom, 129, true},
		{"boundary lengths accepted", UserPasswordModeRandom, 12, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// An otherwise-empty Config is valid: the SSH/auth checks only apply when a
			// host is configured, so this isolates the user_password validation.
			c := &Config{}
			c.Matrix.Import.UserPassword.Mode = tt.mode
			c.Matrix.Import.UserPassword.Length = tt.length

			err := c.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("expected a validation error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}
