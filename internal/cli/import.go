package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/aligundogdu/matrixmigrate/internal/i18n"
	"github.com/aligundogdu/matrixmigrate/internal/matrix"
	"github.com/aligundogdu/matrixmigrate/internal/migration"
)

// progressStageFor maps a MessageImportCallback channel name to the label and unit noun the
// CLI progress line should use for it. An empty label means channelName is the base message
// pass and the caller's current label/unit should be left alone.
func progressStageFor(channelName string) (label, unit string) {
	switch channelName {
	case matrix.ReactionProgressStage:
		return "Reactions", "reactions"
	case matrix.PinProgressStage:
		return "Pinned messages", "rooms"
	default:
		return "", ""
	}
}

func formatETA(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	totalSeconds := int(d.Seconds())
	hours := totalSeconds / 3600
	minutes := (totalSeconds % 3600) / 60
	seconds := totalSeconds % 60
	return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, seconds)
}

var importCmd = &cobra.Command{
	Use:   "import [assets|memberships|messages|leave-rooms|enable-notifications]",
	Short: "Import data to Matrix",
	Long: `Import data to Matrix Synapse server.

Available subcommands:
  assets                - Create users, spaces, and rooms in Matrix
  memberships           - Apply team and channel memberships in Matrix
  messages              - Import all messages to Matrix rooms
  leave-rooms           - Make the migration admin leave all migrated rooms and spaces
  enable-notifications  - Turn on email notifications for migrated users`,
}

var importAssetsCmd = &cobra.Command{
	Use:   "assets",
	Short: "Import users, spaces, and rooms to Matrix",
	Long:  `Create users, spaces, and rooms in Matrix based on exported Mattermost data.`,
	RunE:  runImportAssets,
}

var importMembershipsCmd = &cobra.Command{
	Use:   "memberships",
	Short: "Apply memberships in Matrix",
	Long:  `Add users to spaces and rooms in Matrix based on Mattermost memberships.`,
	RunE:  runImportMemberships,
}

var importMessagesCmd = &cobra.Command{
	Use:   "messages",
	Short: "Import messages to Matrix",
	Long: `Import all messages to Matrix rooms.

This command requires Application Service (AS) configuration to preserve
original message timestamps. Without AS, messages will be imported with
current timestamps.

Requires: appservice.enabled=true and MATRIX_AS_TOKEN env var`,
	RunE: runImportMessages,
}

var importLeaveRoomsCmd = &cobra.Command{
	Use:     "leave-rooms",
	Aliases: []string{"leave_rooms"},
	Short:   "Remove the migration's own accounts from all migrated rooms",
	Long: `Take every account the migration put into rooms for its own convenience back out again.

Three sweeps, in this order:

  1. Deactivated users. Synapse removes an account from every room when it is
     deactivated, so a deactivated account sitting in a member list is a leftover from
     an earlier run. Only runs under deleted_user_mode: deactivated - under "locked"
     those memberships are meant to stay.
  2. The application service bot. It joins rooms only so it can post on behalf of
     authors whose Mattermost account no longer exists.
  3. The migration admin. The import steps already leave rooms as they go, but a failed
     leave is only logged as a warning, which leaves the admin inside private rooms and
     other users' direct messages.

An account that holds power level 100 in a room is left in place: it is the room's owner,
and a room with no administrator cannot be repaired without a server admin.

Messages are unaffected. Matrix keeps events after their sender leaves the room.

Safe to re-run: an account already out of a room counts as already removed.`,
	RunE: runImportLeaveRooms,
}

var importEnableNotificationsCmd = &cobra.Command{
	Use:     "enable-notifications",
	Aliases: []string{"enable_notifications"},
	Short:   "Turn on email notifications for migrated users",
	Long: `Register an email pusher for every migrated user who has an email address, so they
are told by email about messages they missed without having to find the setting first.

Nothing does this by itself: Synapse only creates a pusher on its own registration path,
which never runs when accounts come from MAS, and even natively it is skipped for SSO logins.

Run this AFTER importing messages. A new pusher starts from the current stream position, so
running it earlier would email the entire message import to everyone.

Requires: appservice.enabled=true, and email.enable_notifs plus email.notif_from configured
on the homeserver.

Safe to re-run: Synapse updates the existing pusher rather than adding a second one.`,
	RunE: runImportEnableNotifications,
}

var membershipsSkipCompleted bool

func init() {
	importCmd.AddCommand(importAssetsCmd)
	importCmd.AddCommand(importMembershipsCmd)
	importCmd.AddCommand(importMessagesCmd)
	importCmd.AddCommand(importLeaveRoomsCmd)
	importCmd.AddCommand(importEnableNotificationsCmd)

	// By default, re-running membership import re-applies against the latest snapshot so
	// users who joined channels after the first run get added (force-join is idempotent for
	// users already in a room). Pass --skip-completed to keep the old run-once behaviour.
	importMembershipsCmd.Flags().BoolVar(&membershipsSkipCompleted, "skip-completed", false,
		"skip membership import if it already completed once (default: re-apply to pick up new members)")
}

func runImportAssets(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	printInfo(i18n.T("messages.migration_started"))

	// Create orchestrator
	orch, err := migration.NewOrchestrator(cfg)
	if err != nil {
		return fmt.Errorf("failed to create orchestrator: %w", err)
	}
	defer orch.Close()

	// Check prerequisites
	state := orch.GetState()
	canRun, reason := state.CanRunStep(migration.StepImportAssets)
	if !canRun {
		return fmt.Errorf("cannot run step: %s", reason)
	}

	// Connect to Matrix
	printInfo(i18n.T("progress.connecting", "Matrix"))
	if err := orch.ConnectMatrix(); err != nil {
		return err
	}
	printSuccess(i18n.T("progress.connected", "Matrix"))

	// Import assets
	printInfo(i18n.T("progress.importing"))
	progress := func(stage string, current, total int, item string) {
		if total > 0 {
			printProgress("%s: %d/%d - %s", stage, current, total, item)
		} else {
			printProgress("%s...", stage)
		}
	}

	result, err := orch.ImportAssets(progress)
	if err != nil {
		return err
	}

	printSuccess(i18n.T("messages.mapping_saved", result.OutputFile))
	printInfo(fmt.Sprintf("  Users: created=%d, skipped=%d, failed=%d",
		result.UsersCreated, result.UsersSkipped, result.UsersFailed))
	printInfo(fmt.Sprintf("  Spaces: created=%d, skipped=%d, failed=%d",
		result.SpacesCreated, result.SpacesSkipped, result.SpacesFailed))
	printInfo(fmt.Sprintf("  Rooms: created=%d, skipped=%d, failed=%d, linked=%d",
		result.RoomsCreated, result.RoomsSkipped, result.RoomsFailed, result.RoomsLinked))
	printSuccess(i18n.T("messages.step_completed", "import_assets"))

	return nil
}

func runImportMemberships(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	printInfo(i18n.T("messages.migration_started"))

	// Create orchestrator
	orch, err := migration.NewOrchestrator(cfg)
	if err != nil {
		return fmt.Errorf("failed to create orchestrator: %w", err)
	}
	defer orch.Close()

	// Check prerequisites
	state := orch.GetState()
	canRun, reason := state.CanRunStep(migration.StepImportMemberships)
	if !canRun {
		return fmt.Errorf("cannot run step: %s", reason)
	}
	if step := state.GetStep(migration.StepImportMemberships); step.Status == migration.StatusCompleted {
		if membershipsSkipCompleted {
			printInfo("Membership import already completed in state; skipping replay (--skip-completed).")
			printSuccess(i18n.T("messages.migration_completed"))
			return nil
		}
		printInfo("Membership import already completed; re-applying to pick up new members (force-join is idempotent).")
		orch.SetForceMembershipReplay(true)
	}

	// Connect to Matrix
	printInfo(i18n.T("progress.connecting", "Matrix"))
	if err := orch.ConnectMatrix(); err != nil {
		return err
	}
	printSuccess(i18n.T("progress.connected", "Matrix"))

	// Import memberships
	printInfo(i18n.T("progress.importing"))
	progress := func(stage string, current, total int, item string) {
		if total > 0 {
			printProgress("%s: %d/%d", stage, current, total)
		} else {
			printProgress("%s...", stage)
		}
	}

	result, err := orch.ImportMemberships(progress)
	if err != nil {
		return err
	}

	printInfo(fmt.Sprintf("  Members: added=%d, skipped=%d, failed=%d",
		result.MembersAdded, result.MembersSkipped, result.MembersFailed))
	printSuccess(i18n.T("messages.step_completed", "import_memberships"))
	printSuccess(i18n.T("messages.migration_completed"))

	return nil
}

func runImportMessages(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	printInfo(i18n.T("messages.migration_started"))

	// Check if AppService is enabled
	if !cfg.UseAppService() {
		printWarning("Application Service is not configured. Messages will be imported WITHOUT original timestamps.")
		printInfo("To preserve timestamps, configure appservice in config.yaml and set MATRIX_AS_TOKEN env var")
	}

	// Create orchestrator
	orch, err := migration.NewOrchestrator(cfg)
	if err != nil {
		return fmt.Errorf("failed to create orchestrator: %w", err)
	}
	defer orch.Close()

	// Check prerequisites
	state := orch.GetState()
	canRun, reason := state.CanRunStep(migration.StepImportMessages)
	if !canRun {
		return fmt.Errorf("cannot run step: %s", reason)
	}

	// Connect to Matrix
	printInfo(i18n.T("progress.connecting", "Matrix"))
	if err := orch.ConnectMatrix(); err != nil {
		return err
	}
	printSuccess(i18n.T("progress.connected", "Matrix"))

	// Import messages
	printInfo("Importing messages...")
	startedAt := time.Now()
	lastProgressPrint := time.Time{}
	const progressPrintInterval = 10 * time.Second
	// The reaction and pin passes reuse this callback with counters of their own, so the label,
	// the rate and the ETA all have to start over when either begins - otherwise reactions or
	// pinned rooms are reported under the previous pass's label, at a rate averaged over a run
	// that has already finished.
	label := "Messages"
	unit := "msg"
	progress := func(current, total int, channelName, status string) {
		if total <= 0 {
			return
		}
		stageLabel, stageUnit := progressStageFor(channelName)
		if stageLabel != "" && label != stageLabel {
			label = stageLabel
			unit = stageUnit
			startedAt = time.Now()
			lastProgressPrint = time.Time{}
		}
		now := time.Now()
		if current < total && !lastProgressPrint.IsZero() && now.Sub(lastProgressPrint) < progressPrintInterval {
			return
		}

		percent := float64(current) / float64(total) * 100
		elapsed := now.Sub(startedAt)
		ratePerSec := float64(current) / elapsed.Seconds()
		remaining := total - current
		etaText := "calculating..."
		if ratePerSec > 0 && remaining > 0 {
			etaSeconds := float64(remaining) / ratePerSec
			etaText = formatETA(time.Duration(etaSeconds * float64(time.Second)))
		} else if remaining <= 0 {
			etaText = "00:00:00"
		}

		printInfo("%s: %d/%d (%.1f%%) | rate: %.1f %s/s | ETA: %s | %s",
			label, current, total, percent, ratePerSec, unit, etaText, status)
		lastProgressPrint = now
	}

	result, err := orch.ImportMessages(progress)
	if err != nil {
		return err
	}

	printInfo(fmt.Sprintf("  Messages: imported=%d, skipped=%d, failed=%d",
		result.MessagesImported, result.MessagesSkipped, result.MessagesFailed))
	printInfo(fmt.Sprintf("  Replies: imported=%d, failed=%d",
		result.RepliesImported, result.RepliesFailed))
	printInfo(fmt.Sprintf("  Files: linked=%d, uploaded=%d, skipped=%d, too_large=%d",
		result.FilesLinked, result.FilesUploaded, result.FilesSkipped, result.FilesTooLarge))
	printInfo(fmt.Sprintf("  Reactions: imported=%d, skipped=%d, failed=%d, custom_emoji=%d",
		result.ReactionsImported, result.ReactionsSkipped, result.ReactionsFailed, result.ReactionsCustomEmoji))
	printInfo(fmt.Sprintf("  Pinned: rooms_updated=%d unchanged=%d events_added=%d skipped=%d failed=%d",
		result.PinnedRoomsUpdated, result.PinnedRoomsUnchanged, result.PinnedEventsAdded,
		result.PinsSkipped, result.PinsFailed))

	if result.MappingFile != "" {
		printSuccess(i18n.T("messages.mapping_saved", result.MappingFile))
	}

	printSuccess(i18n.T("messages.step_completed", "import_messages"))

	return nil
}

func runImportLeaveRooms(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	printInfo(i18n.T("messages.migration_started"))

	// Create orchestrator
	orch, err := migration.NewOrchestrator(cfg)
	if err != nil {
		return fmt.Errorf("failed to create orchestrator: %w", err)
	}
	defer orch.Close()

	// Check prerequisites
	state := orch.GetState()
	canRun, reason := state.CanRunStep(migration.StepLeaveRooms)
	if !canRun {
		return fmt.Errorf("cannot run step: %s", reason)
	}

	// Connect to Matrix
	printInfo(i18n.T("progress.connecting", "Matrix"))
	if err := orch.ConnectMatrix(); err != nil {
		return err
	}
	printSuccess(i18n.T("progress.connected", "Matrix"))

	printInfo(i18n.T("progress.leaving_rooms"))
	progress := func(stage string, current, total int, item string) {
		if total > 0 {
			printProgress("%s: %d/%d", stage, current, total)
		} else {
			printProgress("%s...", stage)
		}
	}

	result, err := orch.LeaveRooms(progress)
	if err != nil {
		return err
	}

	printInfo(fmt.Sprintf("  Deactivated users: checked=%d, removed=%d, kept-as-owner=%d, failed=%d",
		result.DeactivatedAccounts, result.DeactivatedRoomsLeft, result.DeactivatedRoomsKept, result.DeactivatedRoomsFailed))
	printInfo(fmt.Sprintf("  Migration bot: rooms-left=%d, kept-as-owner=%d, failed=%d",
		result.BotRoomsLeft, result.BotRoomsKept, result.BotRoomsFailed))
	printInfo(fmt.Sprintf("  Admin rooms: left=%d, already-out=%d, failed=%d",
		result.RoomsLeft, result.RoomsLeaveSkip, result.RoomsLeaveFailed))
	printSuccess(i18n.T("messages.step_completed", "leave_rooms"))

	return nil
}

func runImportEnableNotifications(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	printInfo(i18n.T("messages.migration_started"))

	orch, err := migration.NewOrchestrator(cfg)
	if err != nil {
		return fmt.Errorf("failed to create orchestrator: %w", err)
	}
	defer orch.Close()

	state := orch.GetState()
	canRun, reason := state.CanRunStep(migration.StepEnableNotifications)
	if !canRun {
		return fmt.Errorf("cannot run step: %s", reason)
	}

	printInfo(i18n.T("progress.connecting", "Matrix"))
	if err := orch.ConnectMatrix(); err != nil {
		return err
	}
	printSuccess(i18n.T("progress.connected", "Matrix"))

	printInfo(i18n.T("progress.enabling_notifications"))
	progress := func(stage string, current, total int, item string) {
		if total > 0 {
			printProgress("%s: %d/%d", stage, current, total)
		} else {
			printProgress("%s...", stage)
		}
	}

	result, err := orch.EnableEmailNotifications(progress)
	if err != nil {
		return err
	}

	printInfo(fmt.Sprintf("  Users: enabled=%d, skipped=%d, failed=%d",
		result.UsersCreated, result.UsersSkipped, result.UsersFailed))
	printSuccess(i18n.T("messages.step_completed", "enable_notifications"))

	return nil
}
