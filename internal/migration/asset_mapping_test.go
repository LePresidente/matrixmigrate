package migration

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aligundogdu/matrixmigrate/internal/config"
	"github.com/aligundogdu/matrixmigrate/internal/matrix"
	"github.com/aligundogdu/matrixmigrate/internal/mattermost"
	"github.com/aligundogdu/matrixmigrate/internal/ssh"
	"github.com/aligundogdu/matrixmigrate/pkg/archive"
)

// A checkpoint written by an interrupted re-run is newer than the file state points at, and
// must win where the two disagree while the older file still contributes what only it has.
func TestLoadExistingAssetMappingsPrefersNewerFile(t *testing.T) {
	dir := t.TempDir()

	old := NewMapping("example.com")
	old.MergeUsers(map[string]string{"u1": "@alice:example.com", "u2": "@old:example.com"})
	old.MergeTeams(map[string]string{"t1": "!oldspace:example.com"})
	oldFile := filepath.Join(dir, "asset-mapping-1.json")
	if err := SaveMapping(old, oldFile); err != nil {
		t.Fatal(err)
	}

	newer := NewMapping("example.com")
	newer.MergeUsers(map[string]string{"u2": "@new:example.com"})
	newer.MergeChannels(map[string]string{"c1": "!room:example.com"})
	newFile := filepath.Join(dir, "asset-mapping-2.json")
	if err := SaveMapping(newer, newFile); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(newFile, later, later); err != nil {
		t.Fatal(err)
	}

	got := loadExistingAssetMappings(oldFile, dir)
	if got == nil {
		t.Fatal("no mappings loaded")
	}
	if got.Users["u1"] != "@alice:example.com" {
		t.Errorf("u1 = %q, want it kept from the recorded file", got.Users["u1"])
	}
	if got.Users["u2"] != "@new:example.com" {
		t.Errorf("u2 = %q, want the newer file to win", got.Users["u2"])
	}
	if got.Spaces["t1"] != "!oldspace:example.com" || got.Rooms["c1"] != "!room:example.com" {
		t.Errorf("spaces=%v rooms=%v, want both sources merged", got.Spaces, got.Rooms)
	}

	if loadExistingAssetMappings("", t.TempDir()) != nil {
		t.Error("no sources should give nil")
	}
}

// The mapping records the homeserver the client detected, and the run writes one file.
func TestImportAssetsSavesOneMappingWithDetectedHomeserver(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Data = config.DataConfig{
		AssetsDir:   filepath.Join(dir, "assets"),
		MappingsDir: filepath.Join(dir, "mappings"),
		StateFile:   filepath.Join(dir, "state.json"),
	}
	cfg.Matrix.Homeserver = "configured.example.com"
	for _, d := range []string{cfg.Data.AssetsDir, cfg.Data.MappingsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	assetFile := filepath.Join(cfg.Data.AssetsDir, "mattermost-assets-1.json.gz")
	assets := &mattermost.Assets{
		Channels: []mattermost.Channel{{ID: "c1", Name: "one", DisplayName: "One", Type: "O"}},
	}
	if err := archive.SaveGzipJSON(assetFile, assets); err != nil {
		t.Fatal(err)
	}
	state := NewMigrationState()
	state.CompleteStep(StepExportAssets, assetFile)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/createRoom") {
			_, _ = fmt.Fprint(w, `{"room_id":"!r1:detected.example.com"}`)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	o := &Orchestrator{
		config:        cfg,
		state:         state,
		tunnelManager: ssh.NewTunnelManager(),
		mxClient:      matrix.NewClientWithRateLimit(srv.URL, "admin-token", "detected.example.com", matrix.RateLimitConfig{}),
	}
	result, err := o.ImportAssets(nil)
	if err != nil {
		t.Fatal(err)
	}

	files, _ := filepath.Glob(filepath.Join(cfg.Data.MappingsDir, "asset-mapping-*.json"))
	if len(files) != 1 || files[0] != result.OutputFile {
		t.Fatalf("mapping files = %v, want exactly the output file %q", files, result.OutputFile)
	}
	m, err := LoadMapping(result.OutputFile)
	if err != nil {
		t.Fatal(err)
	}
	if m.Homeserver != "detected.example.com" {
		t.Errorf("homeserver = %q, want the detected one", m.Homeserver)
	}
	if m.Channels["c1"] != "!r1:detected.example.com" {
		t.Errorf("channels = %v", m.Channels)
	}
}
