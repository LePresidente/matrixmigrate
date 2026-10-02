package tui

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/aligundogdu/matrixmigrate/internal/config"
	"github.com/aligundogdu/matrixmigrate/internal/i18n"
	"github.com/aligundogdu/matrixmigrate/internal/matrix"
	"github.com/aligundogdu/matrixmigrate/internal/migration"
	"github.com/aligundogdu/matrixmigrate/internal/version"
)

// View represents different screens in the app
type View int

const (
	ViewMenu View = iota
	ViewExportAssets
	ViewImportAssets
	ViewExportMemberships
	ViewImportMemberships
	ViewExportMessages
	ViewImportMessages
	ViewLeaveRooms
	ViewEnableNotifications
	ViewTestConnection
	ViewStatus
	ViewSettings
	ViewProgress
	ViewError
	ViewSuccess
	ViewInterrupted
)

// Model is the main application model
type Model struct {
	// App state
	config       *config.Config
	orchestrator *migration.Orchestrator
	view         View
	previousView View

	// UI components
	menuItems []MenuItem
	menuIndex int
	spinner   spinner.Model
	width     int
	height    int

	// Progress state
	progressStage   string
	progressCurrent int
	progressTotal   int
	progressItem    string

	// Test results
	testResult *migration.ConnectionTestResult
	testDone   bool

	// Messages
	errorMessage   string
	successMessage string

	// Operation result for detailed stats
	operationResult *migration.OperationResult

	// Program reference for sending messages from goroutines
	program *tea.Program

	// ctx is the parent of every step's context; cancelling it stops a running step.
	ctx context.Context

	// step is the migration step running in the background, nil when none is. While it is
	// set no other step can start and the progress view cannot be left.
	step *runningStep

	// stopping is set once ctrl+c asked the running step to stop.
	stopping bool

	// Quitting
	quitting bool
}

// runningStep is a migration step running in the background. A pointer, so every copy of
// the Model refers to the same one.
type runningStep struct {
	// cancel cancels the context the step runs under.
	cancel context.CancelFunc
	// done is closed when the step's command returns.
	done chan struct{}
}

// MenuItem represents a menu item
type MenuItem struct {
	Title    string
	Desc     string
	View     View
	Disabled bool
	Action   func() tea.Cmd
}

// Init initializes the application
func (m Model) Init() tea.Cmd {
	return m.spinner.Tick
}

// NewModel creates a new application model
func NewModel(cfg *config.Config) (Model, error) {
	// Create orchestrator
	orchestrator, err := migration.NewOrchestrator(cfg)
	if err != nil {
		return Model{}, fmt.Errorf("failed to create orchestrator: %w", err)
	}

	// Create spinner
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = SpinnerStyle

	m := Model{
		config:       cfg,
		orchestrator: orchestrator,
		view:         ViewMenu,
		spinner:      s,
		ctx:          context.Background(),
		width:        80,
		height:       24,
	}

	// Initialize menu items
	m.menuItems = m.createMenuItems()

	return m, nil
}

// createMenuItems creates the main menu items
func (m *Model) createMenuItems() []MenuItem {
	locale := i18n.Current()
	state := m.orchestrator.GetState()

	// Check which steps can be run
	canExportAssets, _ := state.CanRunStep(migration.StepExportAssets)
	canImportAssets, _ := state.CanRunStep(migration.StepImportAssets)
	canExportMemberships, _ := state.CanRunStep(migration.StepExportMemberships)
	canImportMemberships, _ := state.CanRunStep(migration.StepImportMemberships)
	canExportMessages, _ := state.CanRunStep(migration.StepExportMessages)
	canImportMessages, _ := state.CanRunStep(migration.StepImportMessages)
	canLeaveRooms, _ := state.CanRunStep(migration.StepLeaveRooms)
	canEnableNotifications, _ := state.CanRunStep(migration.StepEnableNotifications)

	return []MenuItem{
		{
			Title:    locale.Menu.ExportAssets,
			Desc:     i18n.T("menu.export_assets_desc"),
			View:     ViewExportAssets,
			Disabled: !canExportAssets,
		},
		{
			Title:    locale.Menu.ImportAssets,
			Desc:     i18n.T("menu.import_assets_desc"),
			View:     ViewImportAssets,
			Disabled: !canImportAssets,
		},
		{
			Title:    locale.Menu.ExportMemberships,
			Desc:     i18n.T("menu.export_memberships_desc"),
			View:     ViewExportMemberships,
			Disabled: !canExportMemberships,
		},
		{
			Title:    locale.Menu.ImportMemberships,
			Desc:     i18n.T("menu.import_memberships_desc"),
			View:     ViewImportMemberships,
			Disabled: !canImportMemberships,
		},
		{
			Title:    locale.Menu.ExportMessages,
			Desc:     i18n.T("menu.export_messages_desc"),
			View:     ViewExportMessages,
			Disabled: !canExportMessages,
		},
		{
			Title:    locale.Menu.ImportMessages,
			Desc:     i18n.T("menu.import_messages_desc"),
			View:     ViewImportMessages,
			Disabled: !canImportMessages,
		},
		{
			Title:    locale.Menu.LeaveRooms,
			Desc:     i18n.T("menu.leave_rooms_desc"),
			View:     ViewLeaveRooms,
			Disabled: !canLeaveRooms,
		},
		{
			Title:    locale.Menu.EnableNotifs,
			Desc:     i18n.T("menu.enable_notifications_desc"),
			View:     ViewEnableNotifications,
			Disabled: !canEnableNotifications,
		},
		{
			Title: locale.Menu.TestConnection,
			Desc:  i18n.T("menu.test_connection_desc"),
			View:  ViewTestConnection,
		},
		{
			Title: locale.Menu.Status,
			Desc:  i18n.T("menu.status_desc"),
			View:  ViewStatus,
		},
		{
			Title: locale.Menu.Quit,
			Desc:  i18n.T("menu.quit_desc"),
			View:  ViewMenu,
			Action: func() tea.Cmd {
				return tea.Quit
			},
		},
	}
}

// Update handles messages and updates the model
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKeyPress(msg)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case progressMsg:
		m.progressStage = msg.stage
		m.progressCurrent = msg.current
		m.progressTotal = msg.total
		m.progressItem = msg.item
		return m, nil

	case operationCompleteMsg:
		if m.step != nil {
			m.step.cancel() // releases the step's context
			m.step = nil
		}
		m.stopping = false
		if errors.Is(msg.err, migration.ErrInterrupted) {
			m.errorMessage = msg.err.Error()
			m.view = ViewInterrupted
		} else if msg.err != nil {
			m.errorMessage = msg.err.Error()
			m.view = ViewError
		} else {
			m.successMessage = msg.message
			m.operationResult = msg.result
			m.view = ViewSuccess
		}
		// Refresh menu items
		m.menuItems = m.createMenuItems()
		return m, nil

	case testCompleteMsg:
		m.testResult = msg.result
		m.testDone = true
		// Never pull the user off a running step's progress view.
		if m.step == nil {
			m.view = ViewTestConnection
		}
		return m, nil
	}

	return m, nil
}

// handleKeyPress handles keyboard input
func (m Model) handleKeyPress(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		if m.step != nil {
			// While a step runs, ctrl+c asks it to stop after the item in flight and the
			// view stays put until it returns; q does nothing.
			if msg.String() == "ctrl+c" && !m.stopping {
				m.step.cancel()
				m.stopping = true
			}
			return m, nil
		}
		if m.view == ViewMenu {
			m.quitting = true
			return m, tea.Quit
		}
		// Go back to menu
		m.view = ViewMenu
		return m, nil

	case "up", "k":
		if m.view == ViewMenu {
			m.menuIndex--
			if m.menuIndex < 0 {
				m.menuIndex = len(m.menuItems) - 1
			}
		}
		return m, nil

	case "down", "j":
		if m.view == ViewMenu {
			m.menuIndex++
			if m.menuIndex >= len(m.menuItems) {
				m.menuIndex = 0
			}
		}
		return m, nil

	case "enter", " ":
		if m.step != nil {
			return m, nil
		}
		if m.view == ViewMenu {
			item := m.menuItems[m.menuIndex]
			if item.Disabled {
				return m, nil
			}
			if item.Action != nil {
				return m, item.Action()
			}
			m.previousView = m.view
			m.view = item.View
			// Call first, then return m: handleViewChange records the started step on m, and
			// the order operands of one return statement are evaluated in is unspecified.
			cmd := m.handleViewChange(item.View)
			return m, cmd
		}
		if m.view == ViewError || m.view == ViewSuccess || m.view == ViewInterrupted {
			m.view = ViewMenu
			return m, nil
		}
		return m, nil

	case "esc":
		if m.step == nil && m.view != ViewMenu {
			m.view = ViewMenu
		}
		return m, nil
	}

	return m, nil
}

// handleViewChange returns commands for view transitions
func (m *Model) handleViewChange(view View) tea.Cmd {
	switch view {
	case ViewExportAssets:
		return m.startStep(m.runExportAssets())
	case ViewImportAssets:
		return m.startStep(m.runImportAssets())
	case ViewExportMemberships:
		return m.startStep(m.runExportMemberships())
	case ViewImportMemberships:
		return m.startStep(m.runImportMemberships())
	case ViewLeaveRooms:
		return m.startStep(m.runLeaveRooms())
	case ViewEnableNotifications:
		return m.startStep(m.runEnableNotifications())
	case ViewExportMessages:
		return m.startStep(m.runExportMessages())
	case ViewImportMessages:
		return m.startStep(m.runImportMessages())
	case ViewTestConnection:
		return m.runTestConnection()
	case ViewStatus:
		// Status view doesn't need a command
		return nil
	}
	return nil
}

// startStep marks a step as running and gives it a fresh cancellable context, handed to the
// orchestrator, so ctrl+c can stop it. The returned command runs cmd and then records that
// the step has returned.
func (m *Model) startStep(cmd tea.Cmd) tea.Cmd {
	ctx, cancel := context.WithCancel(m.ctx)
	m.orchestrator.SetContext(ctx)
	run := &runningStep{cancel: cancel, done: make(chan struct{})}
	m.step = run
	m.stopping = false
	m.progressStage, m.progressCurrent, m.progressTotal, m.progressItem = "", 0, 0, ""
	return func() tea.Msg {
		defer close(run.done)
		return cmd()
	}
}

// View renders the UI
func (m Model) View() string {
	if m.quitting {
		return "Goodbye!\n"
	}

	switch m.view {
	case ViewMenu:
		return m.renderMenu()
	case ViewProgress:
		return m.renderProgress()
	case ViewStatus:
		return m.renderStatus()
	case ViewError:
		return m.renderError()
	case ViewSuccess:
		return m.renderSuccess()
	case ViewInterrupted:
		return m.renderInterrupted()
	case ViewTestConnection:
		return m.renderTestConnection()
	case ViewExportAssets, ViewImportAssets, ViewExportMemberships, ViewImportMemberships, ViewExportMessages, ViewImportMessages, ViewLeaveRooms, ViewEnableNotifications:
		return m.renderProgress()
	default:
		return m.renderMenu()
	}
}

// renderMenu renders the main menu
func (m Model) renderMenu() string {
	locale := i18n.Current()

	// Header
	header := LogoStyle.Render(`
 __  __       _        _      __  __ _                 _       
|  \/  | __ _| |_ _ __(_)_  _|  \/  (_) __ _ _ __ __ _| |_ ___ 
| |\/| |/ _` + "`" + ` | __| '__| \ \/ /| |\/| | |/ _` + "`" + ` | '__/ _` + "`" + ` | __/ _ \
| |  | | (_| | |_| |  | |>  < | |  | | | (_| | | | (_| | ||  __/
|_|  |_|\__,_|\__|_|  |_/_/\_\|_|  |_|_|\__, |_|  \__,_|\__\___|
                                        |___/                   `)

	subtitle := SubtitleStyle.Render(locale.App.Description)
	versionInfo := HelpStyle.Render("v" + version.GetFullVersion())

	// Menu items
	var menuContent string
	for i, item := range m.menuItems {
		cursor := "  "
		style := MenuItemStyle
		descStyle := MenuItemDescStyle
		if i == m.menuIndex {
			cursor = IconArrow + " "
			style = MenuItemSelectedStyle
			descStyle = MenuItemDescSelectedStyle
		}
		if item.Disabled {
			style = MenuItemDisabledStyle
			descStyle = MenuItemDescStyle
		}

		menuContent += cursor + style.Render(item.Title) + "\n"
		if i == m.menuIndex && item.Desc != "" {
			menuContent += descStyle.Render("└─ "+item.Desc) + "\n"
		}
	}

	// Help
	help := HelpStyle.Render("↑/↓: navigate • enter: select • q: quit")

	// Combine
	content := lipgloss.JoinVertical(
		lipgloss.Left,
		header,
		subtitle,
		versionInfo,
		"",
		BoxStyle.Render(TitleStyle.Render(locale.Menu.Title)+"\n\n"+menuContent),
		help,
	)

	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, content)
}

// renderProgress renders the progress view
func (m Model) renderProgress() string {
	locale := i18n.Current()

	// Title based on current operation
	var title string
	switch m.view {
	case ViewExportAssets:
		title = locale.Menu.ExportAssets
	case ViewImportAssets:
		title = locale.Menu.ImportAssets
	case ViewExportMemberships:
		title = locale.Menu.ExportMemberships
	case ViewImportMemberships:
		title = locale.Menu.ImportMemberships
	case ViewLeaveRooms:
		title = locale.Menu.LeaveRooms
	case ViewEnableNotifications:
		title = locale.Menu.EnableNotifs
	case ViewExportMessages:
		title = locale.Menu.ExportMessages
	case ViewImportMessages:
		title = locale.Menu.ImportMessages
	case ViewTestConnection:
		title = locale.Menu.TestConnection
	default:
		title = locale.Progress.Exporting
	}

	// Spinner
	spinner := m.spinner.View()

	// Progress info
	var progressInfo string
	if m.progressTotal > 0 {
		percentage := float64(m.progressCurrent) / float64(m.progressTotal) * 100
		bar := renderProgressBar(int(percentage), 40)
		progressInfo = fmt.Sprintf("%s\n\n%s %.0f%% (%d/%d)",
			m.progressStage,
			bar,
			percentage,
			m.progressCurrent,
			m.progressTotal,
		)
		if m.progressItem != "" {
			progressInfo += "\n" + MutedStyle.Render(m.progressItem)
		}
	} else {
		progressInfo = m.progressStage
	}

	lines := []string{TitleStyle.Render(title), "", spinner + " " + progressInfo}
	help := HelpStyle.Render(i18n.T("messages.step_stop_hint"))
	if m.stopping {
		lines = append(lines, "", WarningStyle.Render(i18n.T("messages.step_stopping")))
		help = HelpStyle.Render("")
	}

	content := BoxStyle.Render(lipgloss.JoinVertical(lipgloss.Left, lines...))

	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
		lipgloss.JoinVertical(lipgloss.Center, content, help))
}

// renderProgressBar renders a simple progress bar
func renderProgressBar(percent, width int) string {
	filled := width * percent / 100
	empty := width - filled

	bar := ProgressBarStyle.Render(repeatStr("█", filled)) +
		MutedStyle.Render(repeatStr("░", empty))

	return bar
}

// repeatStr repeats a string n times
func repeatStr(s string, n int) string {
	if n <= 0 {
		return ""
	}
	result := ""
	for i := 0; i < n; i++ {
		result += s
	}
	return result
}

// renderStatus renders the status view
func (m Model) renderStatus() string {
	locale := i18n.Current()
	state := m.orchestrator.GetState()

	// Build status table
	steps := []migration.StepName{
		migration.StepExportAssets,
		migration.StepImportAssets,
		migration.StepExportMemberships,
		migration.StepImportMemberships,
		migration.StepLeaveRooms,
		migration.StepEnableNotifications,
	}

	var rows string
	for _, stepName := range steps {
		step := state.GetStep(stepName)
		icon := GetStatusIcon(string(step.Status))
		style := GetStatusStyle(string(step.Status))

		name := string(stepName)
		status := style.Render(icon + " " + string(step.Status))

		rows += fmt.Sprintf("  %-25s %s\n", name, status)
	}

	content := BoxStyle.Render(
		lipgloss.JoinVertical(
			lipgloss.Left,
			TitleStyle.Render(locale.Status.Title),
			"",
			rows,
		),
	)

	help := HelpStyle.Render("Press esc or q to go back")

	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
		lipgloss.JoinVertical(lipgloss.Center, content, help))
}

// renderError renders the error view
func (m Model) renderError() string {
	content := ErrorBoxStyle.Render(
		lipgloss.JoinVertical(
			lipgloss.Left,
			ErrorStyle.Render(IconCross+" Error"),
			"",
			m.errorMessage,
		),
	)

	help := HelpStyle.Render("Press enter to continue")

	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
		lipgloss.JoinVertical(lipgloss.Center, content, help))
}

// renderInterrupted renders the view for a step stopped by the user. Its progress was saved,
// so this is not shown as a failure.
func (m Model) renderInterrupted() string {
	content := BoxStyle.Render(
		lipgloss.JoinVertical(
			lipgloss.Left,
			WarningStyle.Render(i18n.T("messages.interrupted_title")),
			"",
			i18n.T("messages.step_interrupted"),
			"",
			DimStyle.Render(m.errorMessage),
		),
	)

	help := HelpStyle.Render("Press enter to continue")

	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
		lipgloss.JoinVertical(lipgloss.Center, content, help))
}

// renderSuccess renders the success view with detailed stats
func (m Model) renderSuccess() string {
	var sections []string

	// Title
	sections = append(sections, SuccessStyle.Render(IconCheck+" Success"))
	sections = append(sections, "")
	sections = append(sections, m.successMessage)

	// Show detailed stats if available
	if m.operationResult != nil {
		sections = append(sections, "")
		sections = append(sections, SubtitleStyle.Render("─────────────────────────"))
		sections = append(sections, "")

		r := m.operationResult

		// Export stats
		if r.UsersExported > 0 || r.TeamsExported > 0 || r.ChannelsExported > 0 {
			sections = append(sections, SubtitleStyle.Render("📤 Exported:"))
			if r.UsersExported > 0 {
				sections = append(sections, fmt.Sprintf("   • Users: %d", r.UsersExported))
			}
			if r.TeamsExported > 0 {
				sections = append(sections, fmt.Sprintf("   • Teams: %d", r.TeamsExported))
			}
			if r.ChannelsExported > 0 {
				sections = append(sections, fmt.Sprintf("   • Channels: %d", r.ChannelsExported))
			}
			sections = append(sections, "")
		}

		// Membership export stats
		if r.TeamMembershipsExported > 0 || r.ChannelMembershipsExported > 0 {
			sections = append(sections, SubtitleStyle.Render("📤 Memberships Exported:"))
			if r.TeamMembershipsExported > 0 {
				sections = append(sections, fmt.Sprintf("   • Team memberships: %d", r.TeamMembershipsExported))
			}
			if r.ChannelMembershipsExported > 0 {
				sections = append(sections, fmt.Sprintf("   • Channel memberships: %d", r.ChannelMembershipsExported))
			}
			sections = append(sections, "")
		}

		// Import stats - Users
		if r.UsersCreated > 0 || r.UsersSkipped > 0 || r.UsersFailed > 0 {
			sections = append(sections, SubtitleStyle.Render("👥 Users:"))
			if r.UsersCreated > 0 {
				sections = append(sections, SuccessStyle.Render(fmt.Sprintf("   ✓ Created: %d", r.UsersCreated)))
			}
			if r.UsersSkipped > 0 {
				sections = append(sections, DimStyle.Render(fmt.Sprintf("   ⊘ Skipped: %d", r.UsersSkipped)))
			}
			if r.UsersFailed > 0 {
				sections = append(sections, ErrorStyle.Render(fmt.Sprintf("   ✗ Failed: %d", r.UsersFailed)))
			}
			sections = append(sections, "")
		}

		// Import stats - Spaces
		if r.SpacesCreated > 0 || r.SpacesSkipped > 0 || r.SpacesFailed > 0 {
			sections = append(sections, SubtitleStyle.Render("🏠 Spaces:"))
			if r.SpacesCreated > 0 {
				sections = append(sections, SuccessStyle.Render(fmt.Sprintf("   ✓ Created: %d", r.SpacesCreated)))
			}
			if r.SpacesSkipped > 0 {
				sections = append(sections, DimStyle.Render(fmt.Sprintf("   ⊘ Skipped: %d", r.SpacesSkipped)))
			}
			if r.SpacesFailed > 0 {
				sections = append(sections, ErrorStyle.Render(fmt.Sprintf("   ✗ Failed: %d", r.SpacesFailed)))
			}
			sections = append(sections, "")
		}

		// Import stats - Rooms
		if r.RoomsCreated > 0 || r.RoomsSkipped > 0 || r.RoomsFailed > 0 || r.RoomsLinked > 0 || r.RoomsLinkFailed > 0 {
			sections = append(sections, SubtitleStyle.Render("💬 Rooms:"))
			if r.RoomsCreated > 0 {
				sections = append(sections, SuccessStyle.Render(fmt.Sprintf("   ✓ Created: %d", r.RoomsCreated)))
			}
			if r.RoomsLinked > 0 {
				sections = append(sections, SuccessStyle.Render(fmt.Sprintf("   ✓ Linked to spaces: %d", r.RoomsLinked)))
			}
			if r.RoomsSkipped > 0 {
				sections = append(sections, DimStyle.Render(fmt.Sprintf("   ⊘ Skipped: %d", r.RoomsSkipped)))
			}
			if r.RoomsFailed > 0 {
				sections = append(sections, ErrorStyle.Render(fmt.Sprintf("   ✗ Failed: %d", r.RoomsFailed)))
			}
			if r.RoomsLinkFailed > 0 {
				sections = append(sections, ErrorStyle.Render("   ✗ "+i18n.T("messages.rooms_link_failed", r.RoomsLinkFailed)))
			}
			sections = append(sections, "")
		}

		// Membership import stats
		if r.MembersAdded > 0 || r.MembersSkipped > 0 || r.MembersFailed > 0 {
			sections = append(sections, SubtitleStyle.Render("👤 Memberships:"))
			if r.MembersAdded > 0 {
				sections = append(sections, SuccessStyle.Render(fmt.Sprintf("   ✓ Added: %d", r.MembersAdded)))
			}
			if r.MembersSkipped > 0 {
				sections = append(sections, DimStyle.Render(fmt.Sprintf("   ⊘ Skipped: %d", r.MembersSkipped)))
			}
			if r.MembersFailed > 0 {
				sections = append(sections, ErrorStyle.Render(fmt.Sprintf("   ✗ Failed: %d", r.MembersFailed)))
			}
			sections = append(sections, "")
		}

		// Output file
		if r.OutputFile != "" {
			sections = append(sections, DimStyle.Render("📁 Output: "+r.OutputFile))
		}
	}

	content := SuccessBoxStyle.Width(50).Render(
		lipgloss.JoinVertical(lipgloss.Left, sections...),
	)

	help := HelpStyle.Render("Press enter to continue")

	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
		lipgloss.JoinVertical(lipgloss.Center, content, help))
}

// renderTestConnection renders detailed test results
func (m Model) renderTestConnection() string {
	locale := i18n.Current()

	if !m.testDone || m.testResult == nil {
		// Still running
		content := BoxStyle.Render(
			lipgloss.JoinVertical(
				lipgloss.Center,
				m.spinner.View(),
				"",
				locale.Test.Testing,
			),
		)
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, content)
	}

	// Build test results
	var sections []string

	// Title
	title := TitleStyle.Render(locale.Test.Title)
	sections = append(sections, title)
	sections = append(sections, "")

	// Config section
	if len(m.testResult.ConfigSteps) > 0 {
		configTitle := SubtitleStyle.Render("📋 " + locale.Test.ConfigSection)
		sections = append(sections, configTitle)
		for _, step := range m.testResult.ConfigSteps {
			sections = append(sections, m.formatTestStep(&step))
		}
		sections = append(sections, "")
	}

	// Mattermost section
	mmTitle := SubtitleStyle.Render("🗄️ " + locale.Test.MattermostSection)
	sections = append(sections, mmTitle)
	if len(m.testResult.MattermostSteps) == 0 {
		sections = append(sections, DimStyle.Render("   No tests run"))
	} else {
		for _, step := range m.testResult.MattermostSteps {
			sections = append(sections, m.formatTestStep(&step))
		}
	}
	sections = append(sections, "")

	// Matrix section
	mxTitle := SubtitleStyle.Render("🔷 " + locale.Test.MatrixSection)
	sections = append(sections, mxTitle)
	if len(m.testResult.MatrixSteps) == 0 {
		sections = append(sections, DimStyle.Render("   No tests run"))
	} else {
		for _, step := range m.testResult.MatrixSteps {
			sections = append(sections, m.formatTestStep(&step))
		}
	}
	sections = append(sections, "")

	// Overall result
	if m.testResult.AllPassed {
		sections = append(sections, SuccessStyle.Render(IconCheck+" "+locale.Test.AllPassed))
	} else {
		sections = append(sections, ErrorStyle.Render(IconCross+" "+locale.Test.SomeFailed))
	}

	content := BoxStyle.Width(70).Render(
		lipgloss.JoinVertical(lipgloss.Left, sections...),
	)

	help := HelpStyle.Render("Press esc or q to go back")

	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
		lipgloss.JoinVertical(lipgloss.Center, content, help))
}

// formatTestStep formats a single test step for display
func (m Model) formatTestStep(step *migration.TestStep) string {
	icon := migration.GetTestStatusIcon(step.Status)
	var style lipgloss.Style

	switch step.Status {
	case migration.TestPassed:
		style = SuccessStyle
	case migration.TestFailed:
		style = ErrorStyle
	case migration.TestSkipped:
		style = DimStyle
	case migration.TestWarning:
		style = WarningStyle
	case migration.TestRunning:
		style = PrimaryStyle
	default:
		style = DimStyle
	}

	line := fmt.Sprintf("   %s %s", style.Render(icon), step.Description)

	if step.Details != "" && step.Status == migration.TestPassed {
		line += DimStyle.Render(" (" + step.Details + ")")
	}

	if step.Error != "" {
		line += "\n      " + ErrorStyle.Render("└─ "+step.Error)
	}

	return line
}

// Message types for async operations
type progressMsg struct {
	stage   string
	current int
	total   int
	item    string
}

type operationCompleteMsg struct {
	message string
	err     error
	result  *migration.OperationResult
}

// Run commands for various operations
func (m *Model) runExportAssets() tea.Cmd {
	return func() tea.Msg {
		sendProgress(i18n.T("progress.connecting", "Mattermost"), 0, 0, "")

		// Connect to Mattermost
		if err := m.orchestrator.ConnectMattermost(); err != nil {
			return operationCompleteMsg{err: err}
		}

		sendProgress(i18n.T("progress.stage_exporting_assets"), 0, 0, "")

		// Run export with live progress updates
		progress := func(stage string, current, total int, item string) {
			sendProgress(stage, current, total, item)
		}

		result, err := m.orchestrator.ExportAssets(progress)
		if err != nil {
			return operationCompleteMsg{err: err}
		}

		return operationCompleteMsg{message: i18n.T("messages.assets_exported"), result: result}
	}
}

func (m *Model) runImportAssets() tea.Cmd {
	return func() tea.Msg {
		sendProgress(i18n.T("progress.connecting", "Matrix"), 0, 0, "")

		// Connect to Matrix
		if err := m.orchestrator.ConnectMatrix(); err != nil {
			return operationCompleteMsg{err: err}
		}

		sendProgress(i18n.T("progress.stage_importing_assets"), 0, 0, "")

		// Run import with live progress updates
		progress := func(stage string, current, total int, item string) {
			sendProgress(stage, current, total, item)
		}

		result, err := m.orchestrator.ImportAssets(progress)
		if err != nil {
			return operationCompleteMsg{err: err}
		}

		return operationCompleteMsg{message: i18n.T("messages.assets_imported"), result: result}
	}
}

func (m *Model) runExportMemberships() tea.Cmd {
	return func() tea.Msg {
		sendProgress(i18n.T("progress.connecting", "Mattermost"), 0, 0, "")

		// Connect if not already
		if err := m.orchestrator.ConnectMattermost(); err != nil {
			return operationCompleteMsg{err: err}
		}

		sendProgress(i18n.T("progress.stage_exporting_memberships"), 0, 0, "")

		progress := func(stage string, current, total int, item string) {
			sendProgress(stage, current, total, item)
		}

		result, err := m.orchestrator.ExportMemberships(progress)
		if err != nil {
			return operationCompleteMsg{err: err}
		}

		return operationCompleteMsg{message: i18n.T("messages.memberships_exported"), result: result}
	}
}

func (m *Model) runImportMemberships() tea.Cmd {
	return func() tea.Msg {
		sendProgress(i18n.T("progress.connecting", "Matrix"), 0, 0, "")

		// Connect if not already
		if err := m.orchestrator.ConnectMatrix(); err != nil {
			return operationCompleteMsg{err: err}
		}

		sendProgress(i18n.T("progress.stage_importing_memberships"), 0, 0, "")

		progress := func(stage string, current, total int, item string) {
			sendProgress(stage, current, total, item)
		}

		result, err := m.orchestrator.ImportMemberships(progress)
		if err != nil {
			return operationCompleteMsg{err: err}
		}

		return operationCompleteMsg{message: i18n.T("messages.memberships_imported"), result: result}
	}
}

func (m *Model) runEnableNotifications() tea.Cmd {
	return func() tea.Msg {
		sendProgress(i18n.T("progress.connecting", "Matrix"), 0, 0, "")

		if err := m.orchestrator.ConnectMatrix(); err != nil {
			return operationCompleteMsg{err: err}
		}

		sendProgress(i18n.T("progress.enabling_notifications"), 0, 0, "")

		progress := func(stage string, current, total int, item string) {
			sendProgress(stage, current, total, item)
		}

		result, err := m.orchestrator.EnableEmailNotifications(progress)
		if err != nil {
			return operationCompleteMsg{err: err}
		}

		msg := i18n.T("messages.notifications_enabled", result.UsersCreated)
		if result.UsersFailed > 0 {
			msg = i18n.T("messages.notifications_enabled_partial",
				result.UsersCreated, result.UsersFailed)
		}
		return operationCompleteMsg{message: msg, result: result}
	}
}

func (m *Model) runLeaveRooms() tea.Cmd {
	return func() tea.Msg {
		sendProgress(i18n.T("progress.connecting", "Matrix"), 0, 0, "")

		// Connect if not already
		if err := m.orchestrator.ConnectMatrix(); err != nil {
			return operationCompleteMsg{err: err}
		}

		sendProgress(i18n.T("progress.stage_leaving_rooms"), 0, 0, "")

		progress := func(stage string, current, total int, item string) {
			sendProgress(stage, current, total, item)
		}

		result, err := m.orchestrator.LeaveRooms(progress)
		if err != nil {
			return operationCompleteMsg{err: err}
		}

		msg := i18n.T("messages.leave_rooms_done",
			result.RoomsLeft, result.DeactivatedRoomsLeft, result.BotRoomsLeft)
		if failed := result.RoomsLeaveFailed + result.DeactivatedRoomsFailed + result.BotRoomsFailed; failed > 0 {
			msg = msg + " " + i18n.T("messages.leave_rooms_failures", failed)
		}
		return operationCompleteMsg{message: msg, result: result}
	}
}

func (m *Model) runExportMessages() tea.Cmd {
	return func() tea.Msg {
		sendProgress(i18n.T("progress.connecting", "Mattermost"), 0, 0, "")

		// Connect if not already
		if err := m.orchestrator.ConnectMattermost(); err != nil {
			return operationCompleteMsg{err: err}
		}

		sendProgress(i18n.T("progress.stage_exporting_messages"), 0, 0, "")

		progress := func(stage string, current, total int, item string) {
			sendProgress(stage, current, total, item)
		}

		result, err := m.orchestrator.ExportMessages(progress)
		if err != nil {
			return operationCompleteMsg{err: err}
		}

		msg := i18n.T("messages.messages_exported_done", result.MessagesExported, result.FilesExported)
		return operationCompleteMsg{message: msg}
	}
}

func (m *Model) runImportMessages() tea.Cmd {
	return func() tea.Msg {
		sendProgress(i18n.T("progress.connecting", "Matrix"), 0, 0, "")

		// Connect if not already
		if err := m.orchestrator.ConnectMatrix(); err != nil {
			return operationCompleteMsg{err: err}
		}

		sendProgress(i18n.T("progress.stage_importing_messages"), 0, 0, "")

		progress := func(current, total int, channelName, status string) {
			// The reaction and pin passes share this callback but count their own items.
			label := i18n.T("progress.label_messages")
			switch channelName {
			case matrix.ReactionProgressStage:
				label = i18n.T("progress.label_reactions")
			case matrix.PinProgressStage:
				label = i18n.T("progress.label_pinned")
			}
			sendProgress(fmt.Sprintf("%s: %s", label, status), current, total, channelName)
		}

		result, err := m.orchestrator.ImportMessages(progress)
		if err != nil {
			return operationCompleteMsg{err: err}
		}

		msg := i18n.T("messages.messages_imported_done",
			result.MessagesImported, result.MessagesSkipped, result.MessagesFailed,
			result.FilesLinked, result.FilesUploaded, result.FilesSkipped, result.FilesTooLarge,
			result.ReactionsImported, result.ReactionsSkipped, result.ReactionsFailed,
			result.PinnedRoomsUpdated, result.PinnedEventsAdded, result.PinsFailed)
		return operationCompleteMsg{message: msg}
	}
}

// testCompleteMsg signals test is complete
type testCompleteMsg struct {
	result *migration.ConnectionTestResult
}

func (m *Model) runTestConnection() tea.Cmd {
	return func() tea.Msg {
		result := migration.RunConnectionTests(m.config, nil)
		return testCompleteMsg{result: result}
	}
}

// programInstance holds the running program for sending messages from goroutines
var programInstance *tea.Program

// Run starts the TUI application. Cancelling ctx stops a running step the way ctrl+c does.
// When the TUI ends the orchestrator is closed, after any step still running has stopped.
func Run(ctx context.Context, cfg *config.Config) error {
	model, err := NewModel(cfg)
	if err != nil {
		return err
	}
	if ctx != nil {
		model.ctx = ctx
	}
	defer model.orchestrator.Close()

	programInstance = tea.NewProgram(model, tea.WithAltScreen())
	final, err := programInstance.Run()

	// The keys cannot quit while a step runs, but a signal can end the program. Let the step
	// finish its item and save its progress before the connections close under it.
	if fm, ok := final.(Model); ok && fm.step != nil {
		fmt.Fprintf(os.Stderr, "⚠ %s\n", i18n.T("messages.step_stopping"))
		fm.step.cancel()
		<-fm.step.done
	}
	return err
}

// sendProgress sends a progress message to the TUI from a goroutine
func sendProgress(stage string, current, total int, item string) {
	if programInstance != nil {
		programInstance.Send(progressMsg{
			stage:   stage,
			current: current,
			total:   total,
			item:    item,
		})
	}
}
