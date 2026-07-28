package migration

import (
	"encoding/csv"
	"os"
	"testing"

	"github.com/aligundogdu/matrixmigrate/internal/matrix"
)

func TestWriteUserPasswords(t *testing.T) {
	dir := t.TempDir()
	creds := []matrix.UserCredential{
		{Username: "alice_dev", MatrixUserID: "@alice_dev:example.com", Password: "pw-one"},
		{Username: "bob_dev", MatrixUserID: "@bob_dev:example.com", Password: "pw,two"}, // comma must survive CSV quoting
	}

	path, err := WriteUserPasswords(dir, creds)
	if err != nil {
		t.Fatalf("WriteUserPasswords returned error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("credentials file must be 0600, got %o", perm)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}

	if len(rows) != 3 {
		t.Fatalf("want header + 2 rows, got %d rows", len(rows))
	}
	if rows[0][0] != "mattermost_username" || rows[0][2] != "password" {
		t.Fatalf("unexpected header: %v", rows[0])
	}
	if rows[1][1] != "@alice_dev:example.com" || rows[1][2] != "pw-one" {
		t.Fatalf("unexpected first row: %v", rows[1])
	}
	if rows[2][2] != "pw,two" {
		t.Fatalf("comma in password was not preserved: %q", rows[2][2])
	}
}

func TestWriteUserPasswordsNoCredsWritesNothing(t *testing.T) {
	dir := t.TempDir()

	path, err := WriteUserPasswords(dir, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "" {
		t.Fatalf("expected no file to be written, got %q", path)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty dir, found %d entries", len(entries))
	}
}
