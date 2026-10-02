package migration

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aligundogdu/matrixmigrate/internal/matrix"
)

func TestHistoryJoinJournalMissingFileIsEmpty(t *testing.T) {
	j, err := LoadHistoryJoinJournal(filepath.Join(t.TempDir(), "history-joins.json"))
	if err != nil {
		t.Fatalf("a missing journal should load empty, got %v", err)
	}
	if len(j.Memberships()) != 0 {
		t.Fatalf("expected no memberships, got %#v", j.Memberships())
	}
}

func TestHistoryJoinJournalRoundTripAndDeduplication(t *testing.T) {
	path := HistoryJoinJournalPath(t.TempDir())
	j, err := LoadHistoryJoinJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	a := matrix.HistoryMembership{RoomID: "!r1:example.com", UserID: "@alice:example.com"}
	b := matrix.HistoryMembership{RoomID: "!r2:example.com", UserID: "@bob_dev:example.com"}
	for _, m := range []matrix.HistoryMembership{a, b, a} {
		if err := j.Add(m); err != nil {
			t.Fatalf("add: %v", err)
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("journal should be written on add: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("journal should be private (0600), got %o", perm)
	}

	reloaded, err := LoadHistoryJoinJournal(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := reloaded.Memberships()
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("expected [a b] after de-duplication, got %#v", got)
	}

	if err := reloaded.ReplaceAll([]matrix.HistoryMembership{b}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	again, err := LoadHistoryJoinJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Memberships(); len(got) != 1 || got[0] != b {
		t.Fatalf("expected only b after replace-all, got %#v", got)
	}

	if err := again.ReplaceAll(nil); err != nil {
		t.Fatalf("replace with nothing: %v", err)
	}
	empty, err := LoadHistoryJoinJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Memberships()) != 0 {
		t.Fatalf("expected an empty journal, got %#v", empty.Memberships())
	}
}

func TestHistoryJoinJournalCorruptFileIsAnError(t *testing.T) {
	path := HistoryJoinJournalPath(t.TempDir())
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHistoryJoinJournal(path); err == nil {
		t.Fatal("a corrupt journal must not load as empty")
	}
}

func TestWithdrawHistoryJoinsKeepsOnlyWhatCouldNotBeWithdrawn(t *testing.T) {
	// A journal left by an interrupted run: alice can be removed now, bob's leave fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/state/m.room.power_levels"):
			_, _ = w.Write([]byte(`{"users":{}}`))
		case strings.HasSuffix(r.URL.Path, "/leave") && r.URL.Query().Get("user_id") == "@bob_dev:example.com":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN","error":"boom"}`))
		case strings.Contains(r.URL.Path, "/account/whoami"):
			_, _ = w.Write([]byte(`{"user_id":"@admin:example.com"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	path := HistoryJoinJournalPath(t.TempDir())
	journal, err := LoadHistoryJoinJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	alice := matrix.HistoryMembership{RoomID: "!r1:example.com", UserID: "@alice:example.com"}
	bob := matrix.HistoryMembership{RoomID: "!r1:example.com", UserID: "@bob_dev:example.com"}
	if err := journal.ReplaceAll([]matrix.HistoryMembership{alice, bob}); err != nil {
		t.Fatal(err)
	}

	client := matrix.NewClient(srv.URL, "admin-token", "example.com")
	client.SetASToken("as-token")
	importer := matrix.NewImporter(client)
	attachHistoryJoinJournal(importer, journal)
	res := withdrawHistoryJoins(importer, journal)
	if res.Left != 1 || res.Failed != 1 {
		t.Fatalf("expected 1 left and 1 failed, got %+v", res)
	}

	reloaded, err := LoadHistoryJoinJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Memberships(); len(got) != 1 || got[0] != bob {
		t.Fatalf("journal should keep only the failed leave, got %#v", got)
	}
}
