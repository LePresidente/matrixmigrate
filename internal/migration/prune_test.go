package migration

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/aligundogdu/matrixmigrate/internal/config"
	"github.com/aligundogdu/matrixmigrate/internal/matrix"
	"github.com/aligundogdu/matrixmigrate/internal/mattermost"
	"github.com/aligundogdu/matrixmigrate/internal/ssh"
	"github.com/aligundogdu/matrixmigrate/pkg/archive"
)

func TestMappingFilesToPrune(t *testing.T) {
	old := []string{
		"m/message-mapping-20240101-010000.json",
		"m/message-mapping-20240102-010000.json",
		"m/message-mapping-20240103-010000.json",
		"m/message-mapping-20240104-010000.json",
	}
	tests := []struct {
		name      string
		paths     []string
		keep      int
		protected []string
		want      []string
	}{
		{
			name:  "keeps the newest by the timestamp in the name",
			paths: []string{old[2], old[0], old[3], old[1]},
			keep:  2,
			want:  []string{old[0], old[1]},
		},
		{
			// Zero is the default and means the feature is off: nothing is ever deleted
			// unless the operator asked for it.
			name:  "keep zero deletes nothing",
			paths: old,
			keep:  0,
			want:  nil,
		},
		{
			name:  "fewer files than the limit deletes nothing",
			paths: old[:2],
			keep:  14,
			want:  nil,
		},
		{
			// The file a step's state points at must survive even when it is old, because
			// later steps open it by that exact name.
			name:      "a protected file survives although it is old",
			paths:     old,
			keep:      1,
			protected: []string{"elsewhere/message-mapping-20240101-010000.json"},
			want:      []string{old[1], old[2]},
		},
		{
			// Only files this tool named are candidates. The journal, a hand-made copy, an
			// atomic-write temp file and the other kind of mapping are all left alone.
			name: "names that are not this kind's timestamped files are ignored",
			paths: []string{
				old[0], old[1], old[2],
				"m/history-joins.json",
				"m/message-mapping-backup.json",
				"m/message-mapping-20240101-010000.json.bak",
				"m/.message-mapping-20240105-010000.json.tmp-123",
				"m/asset-mapping-20230101-010000.json",
			},
			keep: 1,
			want: []string{old[0], old[1]},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mappingFilesToPrune(tt.paths, messageMappingKind, tt.keep, tt.protected...)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("mappingFilesToPrune() = %v, want %v", got, tt.want)
			}
		})
	}
}

// writeFiles creates each name in dir with the given content.
func writeFiles(t *testing.T, dir, content string, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// fileNames lists the base names in dir, sorted.
func fileNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func TestPruneMappingFilesRemovesOldFilesFromDisk(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "12345",
		"asset-mapping-20240101-010000.json",
		"asset-mapping-20240102-010000.json",
		"asset-mapping-20240103-010000.json",
		"message-mapping-20240101-010000.json",
		"history-joins.json",
	)

	removed, freed, failed := pruneMappingFiles(dir, assetMappingKind, 1)

	if removed != 2 || freed != 10 || failed != 0 {
		t.Errorf("removed=%d freed=%d failed=%d, want 2, 10, 0", removed, freed, failed)
	}
	want := []string{
		"asset-mapping-20240103-010000.json",
		"history-joins.json",
		"message-mapping-20240101-010000.json",
	}
	if got := fileNames(t, dir); !reflect.DeepEqual(got, want) {
		t.Errorf("files left = %v, want %v", got, want)
	}
}

const emptyMessageMapping = `{"version": "1.0", "messages": {}, "files": {}}`

var oldMessageMappings = []string{
	"message-mapping-20200101-000000.json",
	"message-mapping-20200102-000000.json",
	"message-mapping-20200103-000000.json",
}

func TestImportMessagesPrunesOldMessageMappings(t *testing.T) {
	mappingsDir := filepath.Join(t.TempDir(), "mappings")
	f := newMessageImportFixture(t, mappingsDir, onePost())
	f.cfg.Data.KeepMappings = 2
	writeFiles(t, mappingsDir, emptyMessageMapping, oldMessageMappings...)
	writeFiles(t, mappingsDir, `{"joins": []}`, "history-joins.json")

	result, err := f.o.ImportMessages(nil)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"history-joins.json",
		oldMessageMappings[2],
		filepath.Base(result.MappingFile),
	}
	sort.Strings(want)
	if got := fileNames(t, mappingsDir); !reflect.DeepEqual(got, want) {
		t.Errorf("files left = %v, want %v", got, want)
	}
}

func TestImportMessagesKeepsEveryMappingByDefault(t *testing.T) {
	mappingsDir := filepath.Join(t.TempDir(), "mappings")
	f := newMessageImportFixture(t, mappingsDir, onePost())
	writeFiles(t, mappingsDir, emptyMessageMapping, oldMessageMappings...)

	if _, err := f.o.ImportMessages(nil); err != nil {
		t.Fatal(err)
	}

	for _, name := range oldMessageMappings {
		if _, err := os.Stat(filepath.Join(mappingsDir, name)); err != nil {
			t.Errorf("%s was removed although data.keep_mappings is not set: %v", name, err)
		}
	}
}

// A step that did not complete has proved nothing about its newest file, so it must not be
// allowed to delete the older ones an operator would fall back to.
func TestFailedMessageImportPrunesNothing(t *testing.T) {
	mappingsDir := filepath.Join(t.TempDir(), "mappings")
	f := newMessageImportFixture(t, mappingsDir, onePost())
	f.cfg.Data.KeepMappings = 1
	writeFiles(t, mappingsDir, emptyMessageMapping, oldMessageMappings[:2]...)
	writeFiles(t, mappingsDir, "{not json", oldMessageMappings[2])

	if _, err := f.o.ImportMessages(nil); err == nil {
		t.Fatal("ImportMessages succeeded although the newest mapping is corrupt")
	}

	for _, name := range oldMessageMappings {
		if _, err := os.Stat(filepath.Join(mappingsDir, name)); err != nil {
			t.Errorf("%s was removed by a failed step: %v", name, err)
		}
	}
}

func TestImportAssetsPrunesOldAssetMappings(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Data = config.DataConfig{
		AssetsDir:    filepath.Join(dir, "assets"),
		MappingsDir:  filepath.Join(dir, "mappings"),
		StateFile:    filepath.Join(dir, "state.json"),
		KeepMappings: 2,
	}
	cfg.Matrix.Homeserver = "example.com"
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
	oldAssetMappings := []string{
		"asset-mapping-20200101-000000.json",
		"asset-mapping-20200102-000000.json",
		"asset-mapping-20200103-000000.json",
	}
	for _, name := range oldAssetMappings {
		if err := SaveMapping(NewMapping("example.com"), filepath.Join(cfg.Data.MappingsDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	writeFiles(t, cfg.Data.MappingsDir, emptyMessageMapping, "message-mapping-20200101-000000.json")
	state := NewMigrationState()
	state.CompleteStep(StepExportAssets, assetFile)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/createRoom") {
			_, _ = fmt.Fprint(w, `{"room_id":"!r1:example.com"}`)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	o := &Orchestrator{
		config:        cfg,
		state:         state,
		tunnelManager: ssh.NewTunnelManager(),
		mxClient:      matrix.NewClientWithRateLimit(srv.URL, "admin-token", "example.com", matrix.RateLimitConfig{}),
	}
	result, err := o.ImportAssets(nil)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		oldAssetMappings[2],
		filepath.Base(result.OutputFile),
		"message-mapping-20200101-000000.json",
	}
	sort.Strings(want)
	if got := fileNames(t, cfg.Data.MappingsDir); !reflect.DeepEqual(got, want) {
		t.Errorf("files left = %v, want %v", got, want)
	}
}
