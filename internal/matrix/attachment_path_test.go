package matrix

import (
	"strings"
	"testing"

	"github.com/aligundogdu/matrixmigrate/internal/mattermost"
)

func TestIsSafeAttachmentPath(t *testing.T) {
	ok := []string{
		"20240101/teams/noteam/channels/c1/users/u1/f1/report.pdf",
		"/20240101/teams/noteam/channels/c1/users/u1/f1/report.pdf",
		"/etc/passwd",
	}
	bad := []string{"../x", "a/../../x", "//etc/passwd", "..", "", "/"}
	for _, p := range ok {
		if !isSafeAttachmentPath(p) {
			t.Errorf("%q should be safe", p)
		}
	}
	for _, p := range bad {
		if isSafeAttachmentPath(p) {
			t.Errorf("%q should be unsafe", p)
		}
	}
}

func TestImportPostFilesUnsafePathPerformsNoRead(t *testing.T) {
	i := NewImporter(NewClientWithRateLimit("http://127.0.0.1:1", "t", "example.com", RateLimitConfig{}))
	fc := &FileConfig{
		Mode: "upload", LocalDataPath: t.TempDir(), MaxUploadSize: 1024,
		RemoteReadFile: func(p string) ([]byte, error) {
			t.Errorf("RemoteReadFile called for %q", p)
			return nil, nil
		},
	}
	res := &ImportMessagesResult{Stats: &MessageImportStats{}}
	files := []mattermost.FileInfo{{ID: "f1", PostID: "p1", Name: "x.txt", Path: "../../etc/passwd", Size: 10}}
	i.importPostFiles(res, "!r:example.com", files, fc, 0, "", "", "")
	if res.Stats.FilesSkipped != 1 {
		t.Errorf("FilesSkipped = %d, want 1", res.Stats.FilesSkipped)
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "unsafe attachment path") ||
		!strings.Contains(res.Errors[0], "x.txt") || !strings.Contains(res.Errors[0], "p1") {
		t.Errorf("errors = %v", res.Errors)
	}
}

func TestImportPostFilesOversizedReadIsTooLarge(t *testing.T) {
	i := NewImporter(NewClientWithRateLimit("http://127.0.0.1:1", "t", "example.com", RateLimitConfig{}))
	fc := &FileConfig{
		Mode: "upload", LocalDataPath: t.TempDir(), MaxUploadSize: 10,
		RemoteReadFile: func(string) ([]byte, error) { return make([]byte, 100), nil },
	}
	res := &ImportMessagesResult{Stats: &MessageImportStats{}}
	// The export claims 5 bytes; the file is really 100.
	files := []mattermost.FileInfo{{ID: "f1", PostID: "p1", Name: "x.bin", Path: "a/x.bin", Size: 5}}
	n, max := i.importPostFiles(res, "!r:example.com", files, fc, 0, "", "", "")
	if n != 1 || max != 100 || res.Stats.FilesTooLarge != 1 || res.Stats.FilesSkipped != 1 || res.Stats.FilesUploaded != 0 {
		t.Errorf("n=%d max=%d stats=%+v", n, max, res.Stats)
	}
}
