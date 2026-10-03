package migration

import (
	"context"
	"errors"
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

// assertAllExist fails the test for every name missing from dir.
func assertAllExist(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was removed: %v", name, err)
		}
	}
}

// The next run resumes from the file that sorts last under "message-mapping-*.json". When
// that is not the file this run wrote - a copy with a non-timestamp name does that - the
// directory is not in the state pruning assumes, and every file deleted would be a record
// the operator needs to get back to a correct resume point.
func TestPruneSkippedWhenARenamedCopySortsAfterTheNewMapping(t *testing.T) {
	mappingsDir := filepath.Join(t.TempDir(), "mappings")
	f := newMessageImportFixture(t, mappingsDir, onePost())
	f.cfg.Data.KeepMappings = 1
	writeFiles(t, mappingsDir, emptyMessageMapping, oldMessageMappings...)
	writeFiles(t, mappingsDir, emptyMessageMapping, "message-mapping-backup.json")

	if _, err := f.o.ImportMessages(nil); err != nil {
		t.Fatal(err)
	}

	assertAllExist(t, mappingsDir, oldMessageMappings...)
	assertAllExist(t, mappingsDir, "message-mapping-backup.json")
}

// Same hazard with a well-formed name: a mapping written while the clock was ahead outranks
// everything written since, so each real run's output would be the next thing pruned.
func TestPruneSkippedWhenAnExistingMappingIsDatedAfterTheNewOne(t *testing.T) {
	mappingsDir := filepath.Join(t.TempDir(), "mappings")
	f := newMessageImportFixture(t, mappingsDir, onePost())
	f.cfg.Data.KeepMappings = 1
	writeFiles(t, mappingsDir, emptyMessageMapping, oldMessageMappings...)
	writeFiles(t, mappingsDir, emptyMessageMapping, "message-mapping-29990101-000000.json")

	if _, err := f.o.ImportMessages(nil); err != nil {
		t.Fatal(err)
	}

	assertAllExist(t, mappingsDir, oldMessageMappings...)
	assertAllExist(t, mappingsDir, "message-mapping-29990101-000000.json")
}

func TestInterruptedMessageImportPrunesNothing(t *testing.T) {
	mappingsDir := filepath.Join(t.TempDir(), "mappings")
	f := newMessageImportFixture(t, mappingsDir, onePost())
	f.cfg.Data.KeepMappings = 1
	writeFiles(t, mappingsDir, emptyMessageMapping, oldMessageMappings...)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.o.SetContext(ctx)

	_, err := f.o.ImportMessages(nil)
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("ImportMessages error = %v, want ErrInterrupted", err)
	}

	assertAllExist(t, mappingsDir, oldMessageMappings...)
}

// mappings_dir is a directory, not a pattern: a name with glob characters in it must not make
// the pruner reach into sibling directories.
func TestPruneMappingFilesDoesNotTreatTheDirectoryAsAPattern(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "m[ab]")
	sibling := filepath.Join(base, "ma")
	names := []string{"asset-mapping-20240101-010000.json", "asset-mapping-20240102-010000.json"}
	for _, d := range []string{dir, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFiles(t, d, "12345", names...)
	}

	removed, _, failed := pruneMappingFiles(dir, assetMappingKind, 1)

	if removed != 1 || failed != 0 {
		t.Errorf("removed=%d failed=%d, want 1, 0", removed, failed)
	}
	if got := fileNames(t, dir); !reflect.DeepEqual(got, names[1:]) {
		t.Errorf("files left in the directory = %v, want %v", got, names[1:])
	}
	assertAllExist(t, sibling, names...)
}

// newAssetImportFixture is an orchestrator ready to run ImportAssets for one public channel
// against a fake homeserver, with data.keep_mappings set to keep. It returns the orchestrator
// and its mappings directory.
func newAssetImportFixture(t *testing.T, keep int) (*Orchestrator, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Data = config.DataConfig{
		AssetsDir:    filepath.Join(dir, "assets"),
		MappingsDir:  filepath.Join(dir, "mappings"),
		StateFile:    filepath.Join(dir, "state.json"),
		KeepMappings: keep,
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
	return o, cfg.Data.MappingsDir
}

var oldAssetMappings = []string{
	"asset-mapping-20200101-000000.json",
	"asset-mapping-20200102-000000.json",
	"asset-mapping-20200103-000000.json",
}

// saveAssetMappings writes an empty, loadable asset mapping under each name in dir.
func saveAssetMappings(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := SaveMapping(NewMapping("example.com"), filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestImportAssetsPrunesOldAssetMappings(t *testing.T) {
	o, mappingsDir := newAssetImportFixture(t, 2)
	saveAssetMappings(t, mappingsDir, oldAssetMappings...)
	writeFiles(t, mappingsDir, emptyMessageMapping, "message-mapping-20200101-000000.json")

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
	if got := fileNames(t, mappingsDir); !reflect.DeepEqual(got, want) {
		t.Errorf("files left = %v, want %v", got, want)
	}
}

func TestImportAssetsDoesNotPruneWhenAnotherMappingSortsLast(t *testing.T) {
	o, mappingsDir := newAssetImportFixture(t, 1)
	saveAssetMappings(t, mappingsDir, oldAssetMappings...)
	saveAssetMappings(t, mappingsDir, "asset-mapping-29990101-000000.json")

	if _, err := o.ImportAssets(nil); err != nil {
		t.Fatal(err)
	}

	assertAllExist(t, mappingsDir, oldAssetMappings...)
	assertAllExist(t, mappingsDir, "asset-mapping-29990101-000000.json")
}

func TestInterruptedAssetImportPrunesNothing(t *testing.T) {
	o, mappingsDir := newAssetImportFixture(t, 1)
	saveAssetMappings(t, mappingsDir, oldAssetMappings...)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o.SetContext(ctx)

	_, err := o.ImportAssets(nil)
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("ImportAssets error = %v, want ErrInterrupted", err)
	}

	assertAllExist(t, mappingsDir, oldAssetMappings...)
}
