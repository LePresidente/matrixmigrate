package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aligundogdu/matrixmigrate/internal/i18n"
)

func TestAnnounceInterruptPrintsOnceAndRestoresDefaultHandling(t *testing.T) {
	if err := i18n.Init("en"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		announceInterrupt(ctx, func() { close(stopped) }, make(chan struct{}), &out)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("announceInterrupt did not return after the context was cancelled")
	}
	select {
	case <-stopped:
	default:
		t.Error("stop was not called, so a second signal would not kill the process")
	}
	if got := out.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "Interrupt received") {
		t.Errorf("printed %q, want the interrupt line once", got)
	}
}

func TestAnnounceInterruptSilentWhenCommandFinishes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		announceInterrupt(ctx, cancel, finished, &out)
		close(done)
	}()

	// Execute closes finished before calling stop, which also cancels the context.
	close(finished)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("announceInterrupt did not return after the command finished")
	}
	if out.Len() != 0 {
		t.Errorf("printed %q after a normal finish, want nothing", out.String())
	}
}

// A config file that cannot be loaded is an error in TUI mode too, not a "no config" notice
// with exit status 0.
func TestRootCommandReturnsConfigLoadError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("matrix: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"--config", path})
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
		cfgFile = ""
	})

	if err := rootCmd.Execute(); err == nil {
		t.Fatal("root command returned no error for a config file that does not parse")
	}
}

// While the TUI owns the terminal it reports the interrupt itself; the CLI line would land in
// the middle of its screen. Default signal handling is still restored.
func TestAnnounceInterruptSilentWhileTUIRunning(t *testing.T) {
	tuiRunning.Store(true)
	defer tuiRunning.Store(false)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		announceInterrupt(ctx, func() { close(stopped) }, make(chan struct{}), &out)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("announceInterrupt did not return after the context was cancelled")
	}
	select {
	case <-stopped:
	default:
		t.Error("stop was not called, so a second signal would not kill the process")
	}
	if out.Len() != 0 {
		t.Errorf("printed %q while the TUI was running", out.String())
	}
}
