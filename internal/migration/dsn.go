package migration

import (
	"fmt"
	"strings"
)

// buildPostgresDSN assembles a lib/pq key/value connection string.
//
// Every value is single-quoted, which is not cosmetic. lib/pq skips whitespace after the "="
// before reading a value, so an unquoted empty one swallows the following key: with no
// password, `user=bob password= dbname=mattermost` parses password as "dbname=mattermost"
// and leaves dbname unset. lib/pq then defaults the database name to the user name, and the
// connection either fails with `database "bob" does not exist` or — worse — silently succeeds
// against the wrong database when one happens to match the user.
//
// Empty passwords are the normal case for peer or trust authentication over a unix socket,
// which is how the bundled PostgreSQL in a GitLab Omnibus install is reached. Quoting also
// makes passwords containing spaces or quotes work, which they previously did not.
//
// sslMode is passed through as given - callers resolve it with config.ResolveDBSSLMode. It is
// the one value left unquoted: lib/pq compares it against a fixed set of literals, and a
// quoted value matches none of them.
func buildPostgresDSN(host string, port int, user, password, dbname, sslMode string) string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		quoteDSNValue(host),
		port,
		quoteDSNValue(user),
		quoteDSNValue(password),
		quoteDSNValue(dbname),
		sslMode,
	)
}

// quoteDSNValue single-quotes a connection-string value, escaping backslashes and quotes the
// way lib/pq's parser expects.
func quoteDSNValue(v string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
}
