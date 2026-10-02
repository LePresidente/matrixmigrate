package migration

import (
	"context"
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

// An interrupt during message import must leave every message that was sent recorded in the
// saved mapping, and the step marked failed with ErrInterrupted, so the next run resumes
// instead of sending them again.
func TestImportMessagesInterruptedSavesProgress(t *testing.T) {
	const sentBeforeInterrupt = 2
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

	posts := make([]mattermost.Post, 6)
	for idx := range posts {
		posts[idx] = mattermost.Post{ID: fmt.Sprintf("p%d", idx), ChannelID: "c1", UserID: "u-alice", Message: "hello", CreateAt: int64(1000 + idx)}
	}
	exportFile := filepath.Join(cfg.Data.AssetsDir, "mattermost-messages-1.json.gz")
	if err := archive.SaveGzipJSON(exportFile, &mattermost.Messages{Posts: posts}); err != nil {
		t.Fatal(err)
	}
	assetMapping := NewMapping("example.com")
	assetMapping.MergeUsers(map[string]string{"u-alice": "@alice:example.com"})
	assetMapping.MergeChannels(map[string]string{"c1": "!r:example.com"})
	assetFile := filepath.Join(cfg.Data.MappingsDir, "asset-mapping-1.json")
	if err := SaveMapping(assetMapping, assetFile); err != nil {
		t.Fatal(err)
	}

	state := NewMigrationState()
	state.CompleteStep(StepExportMessages, exportFile)
	state.CompleteStep(StepImportAssets, assetFile)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	sends := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/send/m.room.message/"):
			mu.Lock()
			sends++
			n := sends
			mu.Unlock()
			if n == sentBeforeInterrupt {
				cancel()
			}
			_, _ = fmt.Fprintf(w, `{"event_id":"$e%d"}`, n)
		case strings.HasSuffix(r.URL.Path, "/members"):
			_, _ = w.Write([]byte(`{"members":["@alice:example.com"]}`))
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
	o.SetContext(ctx)

	_, importErr := o.ImportMessages(nil)
	if !errors.Is(importErr, ErrInterrupted) {
		t.Fatalf("err = %v, want one wrapping ErrInterrupted", importErr)
	}

	saved, err := LoadState(cfg.Data.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	if step := saved.GetStep(StepImportMessages); step.Status != StatusFailed || !strings.Contains(step.ErrorMessage, "interrupted") {
		t.Errorf("saved step = %s %q, want failed and interrupted", step.Status, step.ErrorMessage)
	}

	mappingFile, err := GetLatestMessageMappingFile(cfg.Data.MappingsDir)
	if err != nil || mappingFile == "" {
		t.Fatalf("no message mapping saved (%v)", err)
	}
	m, err := LoadMessageMapping(mappingFile)
	if err != nil {
		t.Fatal(err)
	}
	if m.Count() != sentBeforeInterrupt {
		t.Errorf("saved mapping holds %d messages, want %d", m.Count(), sentBeforeInterrupt)
	}
	if msg := importErr.Error(); !strings.Contains(msg, mappingFile) || !strings.Contains(msg, "run the same command again") {
		t.Errorf("error should name the saved mapping and say how to resume: %v", importErr)
	}
}
