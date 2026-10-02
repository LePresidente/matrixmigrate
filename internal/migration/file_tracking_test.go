package migration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aligundogdu/matrixmigrate/internal/mattermost"
)

func writeMappingJSON(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "message-mapping-1.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

var trackingFiles = []mattermost.FileInfo{
	{ID: "f1", PostID: "p1", Name: "a.png"},
	{ID: "f2", PostID: "p1", Name: "b.txt"},
	{ID: "f3", PostID: "p2", Name: "c.txt"}, // p2 was never imported
}

const mappedP1 = `"messages": {"p1": {"mattermost_id": "p1", "matrix_event_id": "$e1"}}`

// A mapping written before file tracking has no "files" key: nothing is known about which
// attachments of its posts made it, so every file of an imported post is taken as sent.
func TestLegacyMappingMarksFilesOfMappedPostsSent(t *testing.T) {
	m, err := LoadMessageMapping(writeMappingJSON(t, `{"version": "1.0", `+mappedP1+`}`))
	if err != nil {
		t.Fatal(err)
	}
	if n := adoptLegacyFileTracking(m, trackingFiles); n != 2 {
		t.Errorf("marked %d files, want 2", n)
	}
	files := m.FileIDs()
	for _, id := range []string{"f1", "f2"} {
		if v, ok := files[id]; !ok || v != "" {
			t.Errorf("file %s = %q, %v; want marked sent with an empty event ID", id, v, ok)
		}
	}
	if _, ok := files["f3"]; ok {
		t.Errorf("f3 belongs to an unimported post but was marked sent")
	}
	// Adopting is a one-off: a second call marks nothing more.
	if n := adoptLegacyFileTracking(m, trackingFiles); n != 0 {
		t.Errorf("second adopt marked %d files, want 0", n)
	}
}

func TestTrackedEmptyMappingMarksNothing(t *testing.T) {
	m, err := LoadMessageMapping(writeMappingJSON(t, `{"version": "1.0", `+mappedP1+`, "files": {}}`))
	if err != nil {
		t.Fatal(err)
	}
	if n := adoptLegacyFileTracking(m, trackingFiles); n != 0 || m.FileCount() != 0 {
		t.Errorf("marked %d files (count %d), want none", n, m.FileCount())
	}
}

// An empty file record must survive a save and load as "tracked, nothing sent", or the next
// run would mistake it for a legacy mapping and mark every failed attachment as sent.
func TestEmptyFileTrackingSurvivesSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "message-mapping-2.json")
	src := NewMessageMapping("example.com")
	src.AddMessage(&MessageMapEntry{MattermostID: "p1", MatrixEventID: "$e1"})
	if err := SaveMessageMapping(src, path); err != nil {
		t.Fatal(err)
	}
	m, err := LoadMessageMapping(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := adoptLegacyFileTracking(m, trackingFiles); n != 0 {
		t.Errorf("a freshly saved mapping was treated as legacy: marked %d files", n)
	}
}

func TestMessageMappingFileAccessors(t *testing.T) {
	m := NewMessageMapping("example.com")
	m.AddFile("f1", "$e1")
	if !m.HasFile("f1") || m.HasFile("f2") || m.FileCount() != 1 {
		t.Errorf("accessors disagree: has f1=%v f2=%v count=%d", m.HasFile("f1"), m.HasFile("f2"), m.FileCount())
	}
	ids := m.FileIDs()
	ids["f9"] = "x"
	if m.HasFile("f9") {
		t.Errorf("FileIDs returned the live map, not a copy")
	}
}
