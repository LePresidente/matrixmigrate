package config

import "testing"

func TestValidateFileMode(t *testing.T) {
	tests := []struct {
		name          string
		mode          string
		localDataPath string
		s3PublicURL   string
		wantErr       bool
	}{
		{"unset mode is valid", "", "", "", false},
		{"link needs no local path", "link", "", "", false},
		{"skip needs no local path", "skip", "", "", false},
		{"upload with a path", "upload", "/var/opt/gitlab/mattermost/data", "", false},
		// The case that cost a full migration run: accepted, then every attachment is
		// skipped one by one during message import.
		{"upload without a path is rejected", "upload", "", "", true},
		{"unknown mode is rejected", "copy", "/data", "", true},
		// GetFileMode() resolves an unset mode to "link" when an S3 URL is present and to
		// "skip" otherwise, so neither can require a local path.
		{"unset mode with s3 url", "", "", "https://s3.example.com/bucket", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{}
			c.Mattermost.Files.Mode = tt.mode
			c.Mattermost.Files.LocalDataPath = tt.localDataPath
			c.Mattermost.Files.S3PublicURL = tt.s3PublicURL

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
