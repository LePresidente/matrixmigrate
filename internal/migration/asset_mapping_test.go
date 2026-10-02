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

	got, err := loadExistingAssetMappings(oldFile, dir)
	if err != nil {
		t.Fatal(err)
	}
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

	if got, err := loadExistingAssetMappings("", t.TempDir()); got != nil || err != nil {
		t.Errorf("no sources should give nil and no error, got %v, %v", got, err)
	}
	// A recorded file that has since been removed is not an error: there is nothing to lose.
	if _, err := loadExistingAssetMappings(filepath.Join(dir, "gone.json"), dir); err != nil {
		t.Errorf("missing recorded file: %v", err)
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

// The newest asset mapping is the one with the latest timestamp in its name, as for message
// mappings: a file copied or restored later has a newer mtime but is not newer.
func TestGetLatestMappingFilePicksByName(t *testing.T) {
	dir := t.TempDir()
	older := filepath.Join(dir, "asset-mapping-20260101-120000.json")
	newer := filepath.Join(dir, "asset-mapping-20260102-120000.json")
	for _, f := range []string{newer, older} {
		if err := SaveMapping(NewMapping("example.com"), f); err != nil {
			t.Fatal(err)
		}
	}
	touched := time.Now().Add(time.Hour)
	if err := os.Chtimes(older, touched, touched); err != nil {
		t.Fatal(err)
	}

	got, err := GetLatestMappingFile(dir)
	if err != nil || got != newer {
		t.Errorf("GetLatestMappingFile = %q, %v; want %q", got, err, newer)
	}

	if got, err := GetLatestMappingFile(t.TempDir()); got != "" || err != nil {
		t.Errorf("empty dir: got %q, %v; want no file and no error", got, err)
	}
}

// A mapping file that exists but cannot be read must stop the import: starting from nothing
// would create every space, alias-less room and DM a second time.
func TestLoadExistingAssetMappingsCorruptFileIsAnError(t *testing.T) {
	for _, which := range []string{"recorded", "newest"} {
		t.Run(which, func(t *testing.T) {
			dir := t.TempDir()
			good := filepath.Join(dir, "asset-mapping-20260101-120000.json")
			if err := SaveMapping(NewMapping("example.com"), good); err != nil {
				t.Fatal(err)
			}
			corrupt := filepath.Join(dir, "asset-mapping-20260102-120000.json")
			if err := os.WriteFile(corrupt, []byte(`{"users": `), 0o600); err != nil {
				t.Fatal(err)
			}
			recorded := good
			if which == "recorded" {
				recorded = corrupt
			}
			got, err := loadExistingAssetMappings(recorded, dir)
			if err == nil || !strings.Contains(err.Error(), corrupt) {
				t.Errorf("got %v, %v; want an error naming %s", got, err, corrupt)
			}
		})
	}
}

func TestImportAssetsStopsOnCorruptMapping(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Data = config.DataConfig{
		AssetsDir:   filepath.Join(dir, "assets"),
		MappingsDir: filepath.Join(dir, "mappings"),
		StateFile:   filepath.Join(dir, "state.json"),
	}
	cfg.Matrix.Homeserver = "example.com"
	for _, d := range []string{cfg.Data.AssetsDir, cfg.Data.MappingsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	assetFile := filepath.Join(cfg.Data.AssetsDir, "mattermost-assets-1.json.gz")
	assets := &mattermost.Assets{
		Users:    []mattermost.User{{ID: "u1", Username: "alice"}},
		Teams:    []mattermost.Team{{ID: "t1", Name: "team", DisplayName: "Team"}},
		Channels: []mattermost.Channel{{ID: "c1", Name: "one", DisplayName: "One", Type: "O"}},
	}
	if err := archive.SaveGzipJSON(assetFile, assets); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(cfg.Data.MappingsDir, "asset-mapping-20260101-120000.json")
	if err := os.WriteFile(corrupt, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := NewMigrationState()
	state.CompleteStep(StepExportAssets, assetFile)

	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	o := &Orchestrator{
		config:        cfg,
		state:         state,
		tunnelManager: ssh.NewTunnelManager(),
		mxClient:      matrix.NewClientWithRateLimit(srv.URL, "admin-token", "example.com", matrix.RateLimitConfig{}),
	}
	_, err := o.ImportAssets(nil)
	if err == nil || !strings.Contains(err.Error(), corrupt) {
		t.Fatalf("err = %v, want one naming %s", err, corrupt)
	}
	if requests != 0 {
		t.Errorf("%d requests reached the homeserver, want none", requests)
	}
	if step := state.GetStep(StepImportAssets); step.Status != StatusFailed {
		t.Errorf("step status = %s, want failed", step.Status)
	}
}

// A user whose existence could not be confirmed has no mapping, so the message import would
// send their posts as the fallback sender for good. The asset step must save what it did and
// then fail, saying how many users need another run.
func TestImportAssetsFailsWhenUsersUnconfirmed(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Data = config.DataConfig{
		AssetsDir:   filepath.Join(dir, "assets"),
		MappingsDir: filepath.Join(dir, "mappings"),
		StateFile:   filepath.Join(dir, "state.json"),
	}
	cfg.Matrix.Homeserver = "example.com"
	for _, d := range []string{cfg.Data.AssetsDir, cfg.Data.MappingsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	assetFile := filepath.Join(cfg.Data.AssetsDir, "mattermost-assets-1.json.gz")
	assets := &mattermost.Assets{
		Users:    []mattermost.User{{ID: "u1", Username: "alice"}, {ID: "u2", Username: "bob_dev"}},
		Channels: []mattermost.Channel{{ID: "c1", Name: "one", DisplayName: "One", Type: "O"}},
	}
	if err := archive.SaveGzipJSON(assetFile, assets); err != nil {
		t.Fatal(err)
	}
	state := NewMigrationState()
	state.CompleteStep(StepExportAssets, assetFile)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/_synapse/admin/v2/users/@alice"):
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errcode":"M_FORBIDDEN","error":"nope"}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/_synapse/admin/v2/users/"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errcode":"M_NOT_FOUND","error":"User not found"}`))
		case strings.HasSuffix(r.URL.Path, "/createRoom"):
			_, _ = w.Write([]byte(`{"room_id":"!r1:example.com"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)

	o := &Orchestrator{
		config:        cfg,
		state:         state,
		tunnelManager: ssh.NewTunnelManager(),
		mxClient:      matrix.NewClientWithRateLimit(srv.URL, "admin-token", "example.com", matrix.RateLimitConfig{}),
	}
	_, err := o.ImportAssets(nil)
	if err == nil || !strings.Contains(err.Error(), "1 user") || !strings.Contains(err.Error(), "import assets") {
		t.Fatalf("err = %v, want one naming 1 unconfirmed user and asking for import assets again", err)
	}
	if step := state.GetStep(StepImportAssets); step.Status != StatusFailed {
		t.Errorf("step status = %s, want failed", step.Status)
	}
	latest, _ := GetLatestMappingFile(cfg.Data.MappingsDir)
	m, lerr := LoadMapping(latest)
	if lerr != nil {
		t.Fatalf("no mapping saved: %v", lerr)
	}
	if m.Channels["c1"] != "!r1:example.com" || m.Users["u2"] != "@bob_dev:example.com" {
		t.Errorf("saved mapping = %+v, want the room and the created user", m)
	}
	if _, ok := m.Users["u1"]; ok {
		t.Error("the unconfirmed user must not be mapped")
	}
}
