package matrix

import (
	"fmt"
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
	n, max, _ := i.importPostFiles(res, "!r:example.com", files, fc, 0, "", "", "")
	if n != 1 || max != 100 || res.Stats.FilesTooLarge != 1 || res.Stats.FilesSkipped != 1 || res.Stats.FilesUploaded != 0 {
		t.Errorf("n=%d max=%d stats=%+v", n, max, res.Stats)
	}
}

// The link fallback of upload mode must refuse an unsafe path too: the link would name a file
// outside the data directory. Reached here through a file too large to upload.
func TestSendLinkOrSkipUnsafePathSendsNothing(t *testing.T) {
	rc := &requestCounter{}
	srv := newRequestCounter(t, rc)
	c := NewClientWithRateLimit(srv.URL, "admin-token", "example.com", RateLimitConfig{})
	c.SetASToken("as-token")
	i := NewImporter(c)
	fc := &FileConfig{
		Mode: "upload", LocalDataPath: t.TempDir(), MaxUploadSize: 10,
		UploadFallbackToLink: true, S3PublicURL: "https://files.example.com",
	}
	res := &ImportMessagesResult{Stats: &MessageImportStats{}}
	files := []mattermost.FileInfo{{ID: "f1", PostID: "p1", Name: "x.bin", Path: "../../etc/passwd", Size: 100}}
	_, _, eventID := i.importPostFiles(res, "!r:example.com", files, fc, 0, "@alice:example.com", "", "")

	if total, _ := rc.counts(); total != 0 || eventID != "" {
		t.Errorf("%d requests made, event %q; want no link sent", total, eventID)
	}
	if res.Stats.FilesLinked != 0 || len(res.FileMapping) != 0 {
		t.Errorf("stats = %+v files = %v, want nothing linked or recorded", res.Stats, res.FileMapping)
	}
	found := false
	for _, e := range res.Errors {
		if strings.Contains(e, "unsafe attachment path") && strings.Contains(e, "p1") {
			found = true
		}
	}
	if !found {
		t.Errorf("errors = %v, want the unsafe path reported", res.Errors)
	}
}

// Link mode appends a link per attachment to the post's text; an unsafe path gets none.
func TestLinkModeSkipsUnsafeAttachmentPath(t *testing.T) {
	as := &attachmentServer{}
	i, _ := attachmentFixture(t, as, nil)
	fc := &FileConfig{Mode: "link", S3PublicURL: "https://files.example.com"}
	safe := mattermost.FileInfo{ID: "f1", PostID: "p1", Name: "ok.txt", Path: "20240101/f1/ok.txt"}
	unsafe := mattermost.FileInfo{ID: "f2", PostID: "p1", Name: "bad.txt", Path: "../../etc/passwd"}
	posts := []mattermost.Post{{ID: "p1", ChannelID: "c1", UserID: "u-alice", Message: "see", CreateAt: 1000}}

	result, err := i.ImportMessagesWithFiles(posts, attachmentRooms, interruptTestUsers, nil,
		filesByPostOf(safe, unsafe), nil, fc, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sends, _, _ := as.snapshot()
	if len(sends) != 1 {
		t.Fatalf("sends = %v, want one", sends)
	}
	body := fmt.Sprint(sends[0]["body"])
	if !strings.Contains(body, "https://files.example.com/20240101/f1/ok.txt") {
		t.Errorf("body %q lacks the safe link", body)
	}
	if strings.Contains(body, "bad.txt") || strings.Contains(body, "passwd") {
		t.Errorf("body %q links the unsafe path", body)
	}
	if result.Stats.FilesLinked != 1 || result.Stats.FilesSkipped != 1 {
		t.Errorf("stats = %+v, want 1 linked, 1 skipped", result.Stats)
	}
}
