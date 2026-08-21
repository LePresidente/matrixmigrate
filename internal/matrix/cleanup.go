package matrix

import (
	"fmt"
	"sort"

	"github.com/aligundogdu/matrixmigrate/internal/logger"
	"github.com/aligundogdu/matrixmigrate/internal/mattermost"
)

// RoomRemovalResult reports what a post-migration removal sweep did.
type RoomRemovalResult struct {
	Accounts int // accounts the sweep looked at
	Left     int // (account, room) memberships removed
	Kept     int // memberships deliberately preserved because the account owns the room
	Failed   int // memberships that could not be removed
	Skipped  int // accounts that could not be examined at all
}

// deactivatedRemovalReason is the kick reason recorded in the room, so the leave event says
// why it happened rather than appearing as an unexplained removal years later.
const deactivatedRemovalReason = "Account deactivated during Mattermost migration"

// asBotRemovalReason is the equivalent for the migration's own service account.
const asBotRemovalReason = "Migration finished"

// RemoveDeletedUsersFromRooms takes every deleted Mattermost account out of every Matrix room
// it is still in.
//
// This is the repair half of deletedUsersExcluded. Skipping those accounts during membership
// import keeps the problem from recurring, but it does not undo the memberships an earlier run
// already created, and re-deactivating an already-deactivated account is a no-op in Synapse -
// the parting only happens on the transition, so it will not clean up after itself.
//
// Does nothing under DeletedUserModeLocked, where keeping the memberships is the point.
func (i *Importer) RemoveDeletedUsersFromRooms(
	users []mattermost.User,
	userMapping map[string]string,
	progress ImportProgressCallback,
) *RoomRemovalResult {
	result := &RoomRemovalResult{}

	if i.locksDeletedUsers() {
		logger.Info("deleted_user_mode is %q: deleted accounts keep their room memberships, nothing to remove", DeletedUserModeLocked)
		return result
	}
	if !i.client.HasAdminToken() {
		logger.Warn("No admin token: cannot find or remove the room memberships of deactivated accounts")
		return result
	}

	targets := make([]string, 0)
	seen := make(map[string]struct{})
	for idx := range users {
		if !users[idx].IsDeleted() {
			continue
		}
		mxID, mapped := userMapping[users[idx].ID]
		if !mapped || mxID == "" {
			continue
		}
		if _, dup := seen[mxID]; dup {
			continue
		}
		seen[mxID] = struct{}{}
		targets = append(targets, mxID)
	}
	sort.Strings(targets)

	logger.Info("Removing deactivated accounts from rooms: %d account(s) to check", len(targets))
	for idx, mxID := range targets {
		if progress != nil {
			progress("remove_deactivated_users", idx+1, len(targets), mxID)
		}
		result.Accounts++
		// Admin kick first: these accounts are deactivated, so impersonating them through the
		// application service is the attempt that is expected to fail.
		i.removeFromEveryRoom(mxID, true, deactivatedRemovalReason, result)
	}

	logger.Info("Deactivated-account room removal: accounts=%d, left=%d, kept_owner=%d, failed=%d, unreadable=%d",
		result.Accounts, result.Left, result.Kept, result.Failed, result.Skipped)
	return result
}

// RemoveASBotFromRooms takes the application service's own user out of every room it joined.
//
// The bot is put into rooms as a side effect of the migration: it is the fallback sender for
// posts whose author no longer has a Mattermost account, and it cannot post to a room it is
// not in. LeaveHistoryMemberships undoes the joins made during a message import it observed,
// but not those left behind by an interrupted run, an earlier version, or a run whose cleanup
// failed. This sweep asks the homeserver instead of the run's own bookkeeping, so it clears
// all of them.
func (i *Importer) RemoveASBotFromRooms(progress ImportProgressCallback) *RoomRemovalResult {
	result := &RoomRemovalResult{}

	if !i.client.HasASToken() {
		logger.Info("No Application Service token: there is no migration bot to remove from rooms")
		return result
	}
	botID, err := i.client.ASBotUserID()
	if err != nil {
		logger.Warn("Could not resolve the application service bot user: %v", err)
		result.Skipped++
		return result
	}
	if !i.client.HasAdminToken() {
		logger.Warn("No admin token: cannot list the rooms %s is in", botID)
		result.Skipped++
		return result
	}

	if progress != nil {
		progress("remove_as_bot", 1, 1, botID)
	}
	logger.Info("Removing the migration bot %s from every room it joined", botID)
	result.Accounts++
	// The bot is a live account, so it can leave under its own name through the AS.
	i.removeFromEveryRoom(botID, false, asBotRemovalReason, result)

	logger.Info("Migration bot room removal: left=%d, kept_owner=%d, failed=%d", result.Left, result.Kept, result.Failed)
	if result.Failed > 0 {
		logger.Warn("The migration bot %s is still in %d room(s); re-run 'import leave-rooms' or remove it manually", botID, result.Failed)
	}
	return result
}

// removeFromEveryRoom removes userID from every room the homeserver says it is joined to.
//
// Ownership is the one exception, and it is the same exception LeaveHistoryMemberships makes:
// where an account holds RoomOwnerPowerLevel it is the only thing standing between the room
// and having nobody who can administer it, so the membership is kept and reported rather than
// silently dropped. A room whose power levels cannot be read is treated the same way, because
// leaving a room that turns out to have no other administrator cannot be undone without a
// server admin.
func (i *Importer) removeFromEveryRoom(userID string, preferAdminKick bool, reason string, result *RoomRemovalResult) {
	rooms, err := i.client.AdminUserJoinedRooms(userID)
	if err != nil {
		logger.Warn("Could not list the rooms %s is in (%v); leaving its memberships alone", userID, err)
		result.Skipped++
		return
	}
	if len(rooms) == 0 {
		return
	}
	sort.Strings(rooms)

	for _, roomID := range rooms {
		content, perr := i.client.getPowerLevels(roomID)
		if perr != nil || content == nil {
			logger.Warn("Could not read power levels for room %s (%v); keeping %s in place", roomID, perr, userID)
			result.Kept++
			continue
		}
		if content.Users[userID] >= RoomOwnerPowerLevel {
			logger.Info("Keeping %s in room %s: holds power level %d, so it is the room's owner", userID, roomID, content.Users[userID])
			result.Kept++
			continue
		}

		if rerr := i.removeMembership(roomID, userID, reason, preferAdminKick); rerr != nil {
			logger.Warn("Could not remove %s from room %s: %v", userID, roomID, rerr)
			result.Failed++
			continue
		}
		logger.Info("Removed %s from room %s", userID, roomID)
		result.Left++
	}
}

// removeMembership takes userID out of roomID, trying both routes available.
//
// Neither works everywhere. Leaving through the application service acts as the account
// itself, which a deactivated one cannot do; kicking acts as the admin, which first has to be
// in the room with enough power - not a given in an invite-only room. Whichever is likelier to
// work goes first, and the other is the fallback, so a single failure is never the end of it.
func (i *Importer) removeMembership(roomID, userID, reason string, preferAdminKick bool) error {
	viaAppService := func() error {
		if !i.client.HasASToken() {
			return fmt.Errorf("no Application Service token")
		}
		return i.client.LeaveRoomAsUser(roomID, userID)
	}
	viaAdminKick := func() error {
		if !i.client.HasAdminToken() {
			return fmt.Errorf("no admin token")
		}
		members, merr := i.client.roomMemberIDs(roomID)
		if merr != nil {
			logger.Debug("removeMembership: member list for %s unavailable (%v)", roomID, merr)
		}
		if aerr := i.ensureAdminCanActIn(roomID, members); aerr != nil {
			return fmt.Errorf("admin could not enter %s: %w", roomID, aerr)
		}
		return i.client.KickUserFromRoom(roomID, userID, reason)
	}

	first, second := viaAppService, viaAdminKick
	if preferAdminKick {
		first, second = viaAdminKick, viaAppService
	}

	err := first()
	if err == nil {
		return nil
	}
	logger.Debug("removeMembership: first route failed for %s in %s (%v); trying the other", userID, roomID, err)
	serr := second()
	if serr == nil {
		return nil
	}
	return fmt.Errorf("%v; and %w", err, serr)
}
