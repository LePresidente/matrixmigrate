package config

import "testing"

func TestResolveDBSSLMode(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		discovered string
		host       string
		want       string
	}{
		{
			// The whole point of the setting: a remote database must not be reached in
			// cleartext just because nobody said otherwise.
			name: "remote host defaults to require",
			host: "db.internal", want: DBSSLModeRequire,
		},
		{
			// Loopback is also where an SSH tunnel terminates, so this is the path every
			// pre-existing tunnelled config takes.
			name: "loopback defaults to disable",
			host: "127.0.0.1", want: DBSSLModeDisable,
		},
		{
			name: "unix socket directory defaults to disable",
			host: "/var/opt/gitlab/postgresql", want: DBSSLModeDisable,
		},
		{
			name: "localhost defaults to disable",
			host: "localhost", want: DBSSLModeDisable,
		},
		{
			name: "ipv6 loopback defaults to disable",
			host: "::1", want: DBSSLModeDisable,
		},
		{
			name:       "explicit config wins over everything",
			configured: DBSSLModeVerifyFull, discovered: DBSSLModeDisable, host: "127.0.0.1",
			want: DBSSLModeVerifyFull,
		},
		{
			// Mattermost reaches this database without TLS; matching it beats failing to
			// connect to a server that has none.
			name:       "discovered wins over the host default",
			discovered: DBSSLModeDisable, host: "db.internal",
			want: DBSSLModeDisable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveDBSSLMode(tt.configured, tt.discovered, tt.host); got != tt.want {
				t.Fatalf("ResolveDBSSLMode(%q, %q, %q) = %q, want %q",
					tt.configured, tt.discovered, tt.host, got, tt.want)
			}
		})
	}
}

func TestValidateRejectsUnsupportedSSLMode(t *testing.T) {
	// lib/pq rejects libpq's "prefer", so accepting it here would only defer the failure to
	// the first query.
	c := &Config{}
	c.Mattermost.Database.SSLMode = "prefer"
	if err := c.Validate(); err == nil {
		t.Fatal("Validate() accepted ssl_mode=prefer, which lib/pq does not support")
	}
}
