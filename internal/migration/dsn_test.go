package migration

import "testing"

func TestBuildPostgresDSN(t *testing.T) {
	tests := []struct {
		name                   string
		host                   string
		port                   int
		user, password, dbname string
		sslMode                string
		want                   string
	}{
		{
			// Peer or trust auth over a unix socket has no password. Unquoted, the empty
			// value would swallow `dbname=` and the database name would silently become the
			// user name.
			name: "empty password keeps dbname",
			host: "/var/opt/gitlab/postgresql", port: 5432,
			user: "gitlab-psql", password: "", dbname: "mattermost_production", sslMode: "disable",
			want: "host='/var/opt/gitlab/postgresql' port=5432 user='gitlab-psql' password='' dbname='mattermost_production' sslmode=disable",
		},
		{
			name: "tcp host with password",
			host: "127.0.0.1", port: 5433,
			user: "postgres", password: "s3cret", dbname: "mattermost_production", sslMode: "disable",
			want: "host='127.0.0.1' port=5433 user='postgres' password='s3cret' dbname='mattermost_production' sslmode=disable",
		},
		{
			name: "password containing spaces",
			host: "127.0.0.1", port: 5432,
			user: "mmuser", password: "two words", dbname: "mattermost", sslMode: "disable",
			want: "host='127.0.0.1' port=5432 user='mmuser' password='two words' dbname='mattermost' sslmode=disable",
		},
		{
			name: "password containing a quote and a backslash",
			host: "127.0.0.1", port: 5432,
			user: "mmuser", password: `it's\fine`, dbname: "mattermost", sslMode: "disable",
			want: `host='127.0.0.1' port=5432 user='mmuser' password='it\'s\\fine' dbname='mattermost' sslmode=disable`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildPostgresDSN(tt.host, tt.port, tt.user, tt.password, tt.dbname, tt.sslMode)
			if got != tt.want {
				t.Fatalf("buildPostgresDSN()\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}

func TestBuildPostgresDSNCarriesSSLMode(t *testing.T) {
	// sslmode must reach lib/pq unquoted, or it matches none of the literals it compares
	// against and the connection fails with "unsupported sslmode".
	got := buildPostgresDSN("db.internal", 5432, "mmuser", "s3cret", "mattermost", "require")
	want := "host='db.internal' port=5432 user='mmuser' password='s3cret' dbname='mattermost' sslmode=require"
	if got != want {
		t.Fatalf("buildPostgresDSN()\n got: %s\nwant: %s", got, want)
	}
}
