package migration

import (
	"strings"
	"testing"

	"github.com/aligundogdu/matrixmigrate/internal/config"
	"github.com/aligundogdu/matrixmigrate/internal/matrix"
	"github.com/aligundogdu/matrixmigrate/internal/ssh"
)

// ImportMemberships re-applies a completed step rather than skipping it, the same in the CLI
// and the TUI. Here the replay gets as far as looking for its input file.
func TestImportMembershipsReplaysCompletedStep(t *testing.T) {
	state := NewMigrationState()
	state.CompleteStep(StepExportAssets, "")
	state.CompleteStep(StepImportAssets, "")
	state.CompleteStep(StepExportMemberships, "")
	state.CompleteStep(StepImportMemberships, "")

	o := &Orchestrator{
		config:        &config.Config{},
		state:         state,
		tunnelManager: ssh.NewTunnelManager(),
		mxClient:      matrix.NewClient("http://127.0.0.1:1", "admin-token", "example.com"),
	}
	_, err := o.ImportMemberships(nil)
	if err == nil {
		t.Fatal("ImportMemberships skipped the completed step instead of re-applying it")
	}
	if !strings.Contains(err.Error(), "no membership file") {
		t.Fatalf("unexpected error: %v", err)
	}
}
