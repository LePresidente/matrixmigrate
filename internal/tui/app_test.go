package tui

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/aligundogdu/matrixmigrate/internal/config"
	"github.com/aligundogdu/matrixmigrate/internal/i18n"
	"github.com/aligundogdu/matrixmigrate/internal/migration"
)

var (
	keyEnter = tea.KeyMsg{Type: tea.KeyEnter}
	keyEsc   = tea.KeyMsg{Type: tea.KeyEsc}
	keyCtrlC = tea.KeyMsg{Type: tea.KeyCtrlC}
	keyQ     = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}
)

// newTestModel builds a Model over a fresh state file in a temporary directory.
func newTestModel(t *testing.T) Model {
	t.Helper()
	if err := i18n.Init("en"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Data = config.DataConfig{
		AssetsDir:   dir,
		MappingsDir: dir,
		StateFile:   filepath.Join(dir, "state.json"),
	}
	m, err := NewModel(cfg)
	if err != nil {
		t.Fatalf("NewModel: %v", err)
	}
	t.Cleanup(func() { m.orchestrator.Close() })
	return m
}

// withRunningStep marks a step as running on view, recording calls to its cancel function.
func withRunningStep(m Model, view View, cancelled *int) Model {
	m.view = view
	m.step = &runningStep{cancel: func() { *cancelled++ }, done: make(chan struct{})}
	return m
}

func press(t *testing.T, m Model, key tea.KeyMsg) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(key)
	return next.(Model), cmd
}

func TestMenuStartsNothingWhileStepRunning(t *testing.T) {
	var cancelled int
	m := withRunningStep(newTestModel(t), ViewMenu, &cancelled)

	for i, item := range m.menuItems {
		if item.Disabled {
			continue
		}
		m.menuIndex = i
		next, cmd := press(t, m, keyEnter)
		if cmd != nil {
			t.Errorf("enter on %q started a command while a step is running", item.Title)
		}
		if next.view != ViewMenu {
			t.Errorf("enter on %q switched to view %v while a step is running", item.Title, next.view)
		}
	}
}

func TestEscAndQKeepProgressViewWhileStepRunning(t *testing.T) {
	var cancelled int
	m := withRunningStep(newTestModel(t), ViewImportAssets, &cancelled)

	for _, key := range []tea.KeyMsg{keyEsc, keyQ} {
		next, cmd := press(t, m, key)
		if next.view != ViewImportAssets {
			t.Errorf("%s left the progress view (now %v) while a step is running", key, next.view)
		}
		if cmd != nil {
			t.Errorf("%s returned a command while a step is running", key)
		}
	}
	if cancelled != 0 {
		t.Errorf("esc/q cancelled the step %d time(s)", cancelled)
	}
}

func TestCtrlCCancelsRunningStep(t *testing.T) {
	var cancelled int
	m := withRunningStep(newTestModel(t), ViewImportMessages, &cancelled)

	m, cmd := press(t, m, keyCtrlC)
	if cancelled != 1 {
		t.Fatalf("cancel called %d time(s), want 1", cancelled)
	}
	if cmd != nil {
		t.Error("ctrl+c while running returned a command (quit?)")
	}
	if m.view != ViewImportMessages {
		t.Errorf("ctrl+c left the progress view (now %v)", m.view)
	}
	if want := i18n.T("messages.step_stopping"); !strings.Contains(m.View(), want) {
		t.Errorf("progress view does not show %q", want)
	}

	// A second ctrl+c neither quits nor leaves the step behind.
	m, cmd = press(t, m, keyCtrlC)
	if cmd != nil || m.view != ViewImportMessages {
		t.Errorf("second ctrl+c: cmd=%v view=%v, want to stay on the progress view", cmd != nil, m.view)
	}
}

func TestInterruptedStepShowsInterruptedView(t *testing.T) {
	var cancelled int
	m := withRunningStep(newTestModel(t), ViewImportMessages, &cancelled)
	m.stopping = true

	next, _ := m.Update(operationCompleteMsg{err: fmt.Errorf("import messages: %w", migration.ErrInterrupted)})
	m = next.(Model)

	if m.view != ViewInterrupted {
		t.Fatalf("view = %v, want ViewInterrupted", m.view)
	}
	if m.step != nil || m.stopping {
		t.Error("step still marked running after it returned")
	}
	if want := i18n.T("messages.step_interrupted"); !strings.Contains(m.View(), want) {
		t.Errorf("interrupted view does not show %q", want)
	}

	// Back to the menu, and the menu works again.
	m, _ = press(t, m, keyEnter)
	if m.view != ViewMenu {
		t.Errorf("enter on the interrupted view went to %v, want the menu", m.view)
	}
}

func TestFailedStepShowsErrorView(t *testing.T) {
	var cancelled int
	m := withRunningStep(newTestModel(t), ViewImportAssets, &cancelled)

	next, _ := m.Update(operationCompleteMsg{err: fmt.Errorf("boom")})
	m = next.(Model)
	if m.view != ViewError {
		t.Fatalf("view = %v, want ViewError", m.view)
	}
	if m.step != nil {
		t.Error("step still marked running after it returned")
	}
}

func TestStartStepTracksAndReleasesStep(t *testing.T) {
	m := newTestModel(t)
	ran := false
	cmd := m.startStep(func() tea.Msg { ran = true; return nil })
	if m.step == nil {
		t.Fatal("startStep did not mark a step as running")
	}
	run := m.step
	select {
	case <-run.done:
		t.Fatal("done closed before the step ran")
	default:
	}
	cmd()
	if !ran {
		t.Fatal("the step's command was not run")
	}
	select {
	case <-run.done:
	default:
		t.Fatal("done not closed after the step returned")
	}
}

func TestSuccessViewShowsRoomsNotLinked(t *testing.T) {
	m := newTestModel(t)
	m.view = ViewSuccess
	m.successMessage = "done"
	m.operationResult = &migration.OperationResult{RoomsCreated: 3, RoomsLinked: 1, RoomsLinkFailed: 2}
	if want := i18n.T("messages.rooms_link_failed", 2); !strings.Contains(m.View(), want) {
		t.Errorf("success view does not show %q", want)
	}
}

// Pressing enter on a step's menu item must hand back a Model that knows the step is
// running; every guard above depends on it. The command is returned, not run.
func TestEnterOnStepMenuItemMarksStepRunning(t *testing.T) {
	m := newTestModel(t)
	m.menuIndex = -1
	for i, item := range m.menuItems {
		if item.View == ViewExportAssets {
			m.menuIndex = i
		}
	}
	if m.menuIndex < 0 || m.menuItems[m.menuIndex].Disabled {
		t.Fatal("export assets menu item missing or disabled on a fresh state")
	}

	next, cmd := press(t, m, keyEnter)
	if cmd == nil {
		t.Fatal("enter on export assets returned no command")
	}
	if next.step == nil {
		t.Fatal("returned Model does not record the running step")
	}
	if next.view != ViewExportAssets {
		t.Errorf("view = %v, want ViewExportAssets", next.view)
	}
	next.step.cancel()
}
