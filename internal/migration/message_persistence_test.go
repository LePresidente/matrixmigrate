package migration

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aligundogdu/matrixmigrate/internal/config"
	"github.com/aligundogdu/matrixmigrate/internal/matrix"
	"github.com/aligundogdu/matrixmigrate/internal/mattermost"
	"github.com/aligundogdu/matrixmigrate/internal/ssh"
	"github.com/aligundogdu/matrixmigrate/pkg/archive"
)

// messageImportFixture is an orchestrator ready to run ImportMessages against a fake homeserver
// that answers sends with $e1, $e2, ... and uploads with an mxc URI, counting both.
type messageImportFixture struct {
	o   *Orchestrator
	cfg *config.Config

	mu       sync.Mutex
	requests int
	sends    int
	uploads  []string
}

func (f *messageImportFixture) counts() (requests, sends int, uploads []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests, f.sends, append([]string(nil), f.uploads...)
}

// newMessageImportFixture writes messages as the export, an asset mapping placing alice and
// channel c1, and a state in which both prerequisite steps are complete. Asset files live
// outside mappingsDir, so that directory may be made unusable by a test.
func newMessageImportFixture(t *testing.T, mappingsDir string, messages *mattermost.Messages) *messageImportFixture {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Data = config.DataConfig{
		AssetsDir:   filepath.Join(dir, "assets"),
		MappingsDir: mappingsDir,
		StateFile:   filepath.Join(dir, "state.json"),
	}
	cfg.Matrix.Homeserver = "example.com"
	for _, d := range []string{cfg.Data.AssetsDir, cfg.Data.MappingsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	exportFile := filepath.Join(cfg.Data.AssetsDir, "mattermost-messages-1.json.gz")
	if err := archive.SaveGzipJSON(exportFile, messages); err != nil {
		t.Fatal(err)
	}
	assetMapping := NewMapping("example.com")
	assetMapping.MergeUsers(map[string]string{"u-alice": "@alice:example.com"})
	assetMapping.MergeChannels(map[string]string{"c1": "!r:example.com"})
	assetFile := filepath.Join(dir, "asset-mapping-1.json")
	if err := SaveMapping(assetMapping, assetFile); err != nil {
		t.Fatal(err)
	}
	state := NewMigrationState()
	state.CompleteStep(StepExportMessages, exportFile)
	state.CompleteStep(StepImportAssets, assetFile)

	f := &messageImportFixture{cfg: cfg}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests++
		f.mu.Unlock()
		switch {
		case strings.Contains(r.URL.Path, "/send/m.room.message/"):
			f.mu.Lock()
			f.sends++
			n := f.sends
			f.mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"event_id":"$e%d"}`, n)
		case strings.Contains(r.URL.Path, "/media/v3/upload"):
			name := r.URL.Query().Get("filename")
			f.mu.Lock()
			f.uploads = append(f.uploads, name)
			f.mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"content_uri":"mxc://example.com/%s"}`, name)
		case strings.HasSuffix(r.URL.Path, "/members"):
			_, _ = w.Write([]byte(`{"members":["@alice:example.com"]}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)

	f.o = &Orchestrator{
		config:        cfg,
		state:         state,
		tunnelManager: ssh.NewTunnelManager(),
		mxClient:      matrix.NewClientWithRateLimit(srv.URL, "admin-token", "example.com", matrix.RateLimitConfig{}),
	}
	return f
}

func onePost() *mattermost.Messages {
	return &mattermost.Messages{Posts: []mattermost.Post{{ID: "p1", ChannelID: "c1", UserID: "u-alice", Message: "hello", CreateAt: 1000}}}
}

// When the newest message mapping cannot even be looked for, the import must not start from
// nothing: it would send every message again.
func TestImportMessagesFailsWhenMappingLookupFails(t *testing.T) {
	// A '[' makes the directory an invalid glob pattern.
	f := newMessageImportFixture(t, filepath.Join(t.TempDir(), "mappings["), onePost())

	_, err := f.o.ImportMessages(nil)
	if err == nil {
		t.Fatal("ImportMessages succeeded, want the lookup failure reported")
	}
	if requests, _, _ := f.counts(); requests != 0 {
		t.Errorf("%d requests reached the homeserver, want none", requests)
	}
	if step := f.o.state.GetStep(StepImportMessages); step.Status != StatusFailed {
		t.Errorf("step status = %s, want failed", step.Status)
	}
}

// A final mapping save that fails must fail the step: completing it would leave every message
// sent since the last checkpoint unrecorded, to be sent again by the next run.
func TestImportMessagesFailsWhenFinalMappingSaveFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	mappingsDir := filepath.Join(t.TempDir(), "mappings")
	f := newMessageImportFixture(t, mappingsDir, onePost())
	if err := os.Chmod(mappingsDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(mappingsDir, 0o700) })

	_, err := f.o.ImportMessages(nil)
	if err == nil || !strings.Contains(err.Error(), mappingsDir) || !strings.Contains(err.Error(), "checkpoint") {
		t.Fatalf("err = %v, want one naming the mapping file and the last checkpoint", err)
	}
	if errors.Is(err, ErrInterrupted) {
		t.Errorf("err = %v, should not claim an interrupt", err)
	}
	if step := f.o.state.GetStep(StepImportMessages); step.Status != StatusFailed {
		t.Errorf("step status = %s, want failed", step.Status)
	}
}

func TestMessageMappingSaveFailureNamesFileAndCheckpoint(t *testing.T) {
	err := messageMappingSaveFailure("/data/mappings/message-mapping-1.json", errors.New("disk full"))
	msg := err.Error()
	for _, want := range []string{"/data/mappings/message-mapping-1.json", "disk full", "checkpoint"} {
		if !strings.Contains(msg, want) {
			t.Errorf("%q does not mention %q", msg, want)
		}
	}
}

// A message mapping written before attachments were tracked, imported again in upload mode:
// the attachments of posts it already holds are taken as sent - nothing is uploaded for them -
// and recorded so in the saved mapping, while a post it does not hold gets its file uploaded
// once.
func TestImportMessagesAdoptsLegacyMappingWithoutReupload(t *testing.T) {
	files := []mattermost.FileInfo{
		{ID: "f1", PostID: "p1", Name: "a.png", Path: "20240101/f1/a.png", Size: 6, MimeType: "image/png"},
		{ID: "f2", PostID: "p1", Name: "b.txt", Path: "20240101/f2/b.txt", Size: 6, MimeType: "text/plain"},
		{ID: "f3", PostID: "p2", Name: "c.txt", Path: "20240101/f3/c.txt", Size: 6, MimeType: "text/plain"},
	}
	messages := &mattermost.Messages{
		Posts: []mattermost.Post{
			{ID: "p1", ChannelID: "c1", UserID: "u-alice", Message: "old", CreateAt: 1000},
			{ID: "p2", ChannelID: "c1", UserID: "u-alice", Message: "new", CreateAt: 1001},
		},
		Files: files,
	}
	mappingsDir := filepath.Join(t.TempDir(), "mappings")
	f := newMessageImportFixture(t, mappingsDir, messages)

	dataDir := t.TempDir()
	for _, file := range files {
		p := filepath.Join(dataDir, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("bytes!"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.cfg.Mattermost.Files.Mode = "upload"
	f.cfg.Mattermost.Files.LocalDataPath = dataDir

	legacy := `{"version": "1.0", "messages": {"p1": {"mattermost_id": "p1", "matrix_event_id": "$old"}}}`
	if err := os.WriteFile(filepath.Join(mappingsDir, "message-mapping-20200101-000000.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := f.o.ImportMessages(nil); err != nil {
		t.Fatal(err)
	}

	_, _, uploads := f.counts()
	if len(uploads) != 1 || uploads[0] != "c.txt" {
		t.Errorf("uploads = %v, want only c.txt, once", uploads)
	}
	latest, err := GetLatestMessageMappingFile(mappingsDir)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := LoadMessageMapping(latest)
	if err != nil {
		t.Fatal(err)
	}
	sent := saved.FileIDs()
	for _, id := range []string{"f1", "f2"} {
		if _, ok := sent[id]; !ok {
			t.Errorf("%s of the already-imported post is not marked sent: %v", id, sent)
		}
	}
	if sent["f3"] == "" {
		t.Errorf("f3 was uploaded but its event is not recorded: %v", sent)
	}
}
