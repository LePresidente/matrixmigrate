package config

import "testing"

func TestGetDeletedUserModeDefaultsToDeactivated(t *testing.T) {
	tests := []struct {
		configured string
		want       string
	}{
		{"", DeletedUserModeDeactivated},
		{DeletedUserModeDeactivated, DeletedUserModeDeactivated},
		{DeletedUserModeLocked, DeletedUserModeLocked},
		{"nonsense", DeletedUserModeDeactivated},
	}
	for _, tt := range tests {
		c := &Config{}
		c.Matrix.Import.DeletedUserMode = tt.configured
		if got := c.GetDeletedUserMode(); got != tt.want {
			t.Errorf("deleted_user_mode %q resolved to %q, want %q", tt.configured, got, tt.want)
		}
	}
}
