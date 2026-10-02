package cli

import (
	"bytes"
	"context"
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
