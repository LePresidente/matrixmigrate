package matrix

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/aligundogdu/matrixmigrate/internal/logger"
	"github.com/aligundogdu/matrixmigrate/internal/mattermost"
)

// RoomImportOptions configures owner and alias when importing rooms/spaces from Mattermost.
// When PreserveOwnerAndAlias is true, rooms get a local alias (team+name) and owner from creator_id or fallback.
type RoomImportOptions struct {
	PreserveOwnerAndAlias bool   // Enable setting owner and room_alias_name from Mattermost data
	FallbackCreator       string // Matrix localpart when creator_id is empty (e.g. "admin")
	AdminUserID           string // Full Matrix user ID used when fallback user does not exist
}

// sanitizeAliasLocalpart returns a Matrix room alias localpart (allowed: 0-9a-zA-Z._=-).
func sanitizeAliasLocalpart(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '.' || r == '_' || r == '=' || r == '-' || unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
		} else if r == ' ' || r == '/' || r == '+' {
			b.WriteRune('_')
		}
	}
	// Collapse multiple underscores and trim
	out := b.String()
	for strings.Contains(out, "__") {
		out = strings.ReplaceAll(out, "__", "_")
	}
	out = strings.Trim(out, "_")
	if out == "" {
		out = "room"
	}
	return out
}

// Importer handles importing data to Matrix
type Importer struct {
	client *Client
}

// NewImporter creates a new importer
func NewImporter(client *Client) *Importer {
	return &Importer{client: client}
}

// ImportProgressCallback is called to report import progress
type ImportProgressCallback func(stage string, current, total int, item string)

// GenerateRandomPassword generates a random password for new users
func GenerateRandomPassword() string {
	// In production, use crypto/rand for secure random password
	return "ChangeMe123!" // Placeholder - users should change this
}

// ImportUsers imports users from Mattermost to Matrix
func (i *Importer) ImportUsers(users []mattermost.User, existingMapping map[string]string, progress ImportProgressCallback) (map[string]string, *ImportStats, error) {
	mapping := make(map[string]string)
	stats := &ImportStats{}
	total := len(users)

	logger.Info("Starting user import: %d users to process", total)

	// Copy existing mappings
	for k, v := range existingMapping {
		mapping[k] = v
	}
	logger.Info("Existing mappings copied: %d entries", len(existingMapping))

	for idx, user := range users {
		logger.Info("Processing user %d/%d: %s (ID: %s)", idx+1, total, user.Username, user.ID)
		
		if progress != nil {
			progress("users", idx+1, total, user.Username)
		}

		// Deleted/locked users are imported as deactivated so they exist for channel history and show as locked in synapse-admin
		deactivated := user.IsDeleted()

		// Skip if already in mapping
		if _, exists := existingMapping[user.ID]; exists {
			logger.Info("User '%s' already in mapping, skipping", user.Username)
			stats.UsersSkipped++
			continue
		}

		// Try to check if user exists, but don't fail if check fails
		// (some Matrix servers only allow checking local users)
		exists := false
		existsCheck, err := i.client.UserExists(user.Username)
		if err != nil {
			// If check fails with "Can only look up local users", ignore it
			// CreateUser is idempotent anyway, so we can just try to create
			if strings.Contains(err.Error(), "Can only look up local users") {
				logger.Info("UserExists check not available for '%s', will try to create", user.Username)
			} else {
				logger.Warn("UserExists check failed for '%s': %v, will try to create anyway", user.Username, err)
			}
		} else {
			exists = existsCheck
		}

		if exists {
			// User already exists, add to mapping
			mxID := i.client.FormatUserID(user.Username)
			mapping[user.ID] = mxID
			// Keep them deactivated if they are deleted/locked in Mattermost
			if deactivated {
				if err := i.client.SetUserDeactivated(mxID, true); err != nil {
					logger.Warn("Failed to set existing user '%s' as deactivated: %v", user.Username, err)
				} else {
					logger.Info("User '%s' already exists; set deactivated (locked) to match Mattermost", user.Username)
				}
			}
			logger.Info("User '%s' already exists, skipped", user.Username)
			stats.UsersSkipped++
			continue
		}

		// Create the user (CreateUser is idempotent - if user exists, it will update)
		displayName := strings.TrimSpace(user.FirstName + " " + user.LastName)
		if displayName == "" {
			displayName = user.Username
		}

		req := &CreateUserRequest{
			Password:    GenerateRandomPassword(),
			DisplayName: displayName,
			Email:       strings.TrimSpace(user.Email),
			Admin:       false,
			Deactivated: deactivated,
		}
		if deactivated {
			logger.Info("User '%s' is deleted/locked in Mattermost; creating in Matrix as deactivated (locked)", user.Username)
		}

		resp, err := i.client.CreateUser(user.Username, req)
		if err != nil {
			// Check if error is because user already exists
			if strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "M_USER_IN_USE") {
				// User exists, add to mapping
				mapping[user.ID] = i.client.FormatUserID(user.Username)
				logger.Info("User '%s' already exists (detected during create), skipped", user.Username)
				stats.UsersSkipped++
				continue
			}
			logger.Error("Failed to create user '%s': %v", user.Username, err)
			stats.UsersFailed++
			continue
		}
		logger.Success("Created user '%s' -> %s", user.Username, resp.UserID)

		mapping[user.ID] = resp.UserID
		stats.UsersCreated++
	}

	return mapping, stats, nil
}

// resolveRoomOwner returns the Matrix user ID to use as room/space owner.
// Prefer creatorID from mapping; if empty use fallback localpart (if that user exists), else adminUserID.
func (i *Importer) resolveRoomOwner(mattermostCreatorID string, userMapping map[string]string, opts *RoomImportOptions) string {
	if opts == nil || !opts.PreserveOwnerAndAlias {
		return ""
	}
	if mattermostCreatorID != "" {
		if mxID, ok := userMapping[mattermostCreatorID]; ok && mxID != "" {
			logger.Info("resolveRoomOwner: mattermost creator_id %s -> %s (from user mapping)", mattermostCreatorID, mxID)
			return mxID
		}
		logger.Info("resolveRoomOwner: creator_id %q not in user mapping or empty; trying fallback", mattermostCreatorID)
	}
	// No creator_id or not in mapping: use fallback localpart
	if opts.FallbackCreator != "" {
		fallbackID := i.client.FormatUserID(opts.FallbackCreator)
		exists, err := i.client.UserExists(opts.FallbackCreator)
		if err != nil {
			logger.Warn("resolveRoomOwner: fallback user %s exists check failed: %v; using admin", opts.FallbackCreator, err)
		} else if exists {
			logger.Info("resolveRoomOwner: using fallback_room_creator %s -> %s", opts.FallbackCreator, fallbackID)
			return fallbackID
		} else {
			logger.Info("resolveRoomOwner: fallback user %s does not exist on server; using admin %s", opts.FallbackCreator, opts.AdminUserID)
		}
	}
	logger.Info("resolveRoomOwner: using admin user %s", opts.AdminUserID)
	return opts.AdminUserID
}

// ImportTeamsAsSpaces imports teams from Mattermost as Matrix spaces.
// When opts.PreserveOwnerAndAlias is true, each space gets alias from team name and owner from opts (teams have no creator_id).
func (i *Importer) ImportTeamsAsSpaces(teams []mattermost.Team, existingMapping map[string]string, userMapping map[string]string, opts *RoomImportOptions, progress ImportProgressCallback) (map[string]string, *ImportStats, error) {
	mapping := make(map[string]string)
	stats := &ImportStats{}
	total := len(teams)

	if opts != nil && opts.PreserveOwnerAndAlias {
		logger.Info("ImportTeamsAsSpaces: preserve_owner_and_alias enabled; spaces will get alias from team name, owner from fallback or admin")
	}

	// Copy existing mappings
	for k, v := range existingMapping {
		mapping[k] = v
	}

	for idx, team := range teams {
		if progress != nil {
			progress("spaces", idx+1, total, team.DisplayName)
		}

		// Skip deleted teams
		if team.IsDeleted() {
			stats.SpacesSkipped++
			continue
		}

		// Skip if already imported (exists in mapping)
		if _, exists := existingMapping[team.ID]; exists {
			logger.Info("Space '%s' already imported, skipped", team.DisplayName)
			stats.SpacesSkipped++
			continue
		}

		var roomAlias, owner string
		if opts != nil && opts.PreserveOwnerAndAlias {
			roomAlias = sanitizeAliasLocalpart(team.Name)
			owner = i.resolveRoomOwner("", userMapping, opts) // teams have no creator_id
			logger.Info("Space '%s' (team %s): alias=%q owner=%s (teams have no creator_id)", team.DisplayName, team.Name, roomAlias, owner)
		}

		// Create space
		resp, err := i.client.CreateSpace(team.DisplayName, team.Description, team.IsOpen(), roomAlias, owner)
		if err != nil {
			logger.Error("Failed to create space '%s': %v", team.DisplayName, err)
			stats.SpacesFailed++
			continue
		}

		logger.Success("Created space '%s' -> %s", team.DisplayName, resp.RoomID)
		mapping[team.ID] = resp.RoomID
		stats.SpacesCreated++
	}

	return mapping, stats, nil
}

// firstMemberFromGroupDisplayName returns the Matrix user ID for the first *enabled* member in a group channel's display_name (e.g. "user1, user2, user3").
// Tries each comma-separated name in order; skips deleted/disabled users and uses the next name.
func firstMemberFromGroupDisplayName(displayName string, users []mattermost.User, userMapping map[string]string) string {
	parts := strings.Split(displayName, ",")
	for _, part := range parts {
		token := strings.TrimSpace(part)
		if token == "" {
			continue
		}
		mxID := resolveTokenToMatrixID(token, users, userMapping)
		if mxID != "" {
			return mxID
		}
	}
	return ""
}

// resolveTokenToMatrixID resolves a single token (username or display name) to a Matrix user ID.
// Only returns a user that is not deleted and is in userMapping.
func resolveTokenToMatrixID(token string, users []mattermost.User, userMapping map[string]string) string {
	tokenLower := strings.ToLower(token)
	normalized := strings.ToLower(strings.ReplaceAll(token, " ", "_"))

	for _, u := range users {
		if u.IsDeleted() {
			continue
		}
		if mxID, ok := userMapping[u.ID]; !ok || mxID == "" {
			continue
		}
		if u.Username == token || strings.ToLower(u.Username) == tokenLower || strings.ToLower(u.Username) == normalized {
			return userMapping[u.ID]
		}
		display := strings.TrimSpace(u.FirstName + " " + u.LastName)
		if display != "" && (display == token || strings.EqualFold(display, token)) {
			return userMapping[u.ID]
		}
		if u.Nickname != "" && (u.Nickname == token || strings.EqualFold(u.Nickname, token)) {
			return userMapping[u.ID]
		}
	}
	return ""
}

// ImportChannelsAsRooms imports channels from Mattermost as Matrix rooms.
// teamByID maps Mattermost team ID to Team (for alias and name); can be nil if opts.PreserveOwnerAndAlias is false.
// users is used to build username->MatrixID map for group channel (type G) creator when creator_id is empty.
// When opts.PreserveOwnerAndAlias is true, each room gets alias teamname-channelname and owner from channel.CreatorID or fallback (or first member for group channels).
func (i *Importer) ImportChannelsAsRooms(channels []mattermost.Channel, existingMapping map[string]string, teamByID map[string]mattermost.Team, users []mattermost.User, userMapping map[string]string, opts *RoomImportOptions, progress ImportProgressCallback) (map[string]string, *ImportStats, error) {
	mapping := make(map[string]string)
	stats := &ImportStats{}
	total := len(channels)

	if opts != nil && opts.PreserveOwnerAndAlias && teamByID != nil {
		logger.Info("ImportChannelsAsRooms: preserve_owner_and_alias enabled; rooms will get alias team+channel, owner from creator_id or first member (group) or fallback or admin")
	}

	// Copy existing mappings
	for k, v := range existingMapping {
		mapping[k] = v
	}

	for idx, channel := range channels {
		if progress != nil {
			progress("rooms", idx+1, total, channel.DisplayName)
		}

		// Skip deleted channels
		if channel.IsDeleted() {
			stats.RoomsSkipped++
			continue
		}

		// Skip direct messages (2-person DMs)
		if channel.IsDirect() {
			stats.RoomsSkipped++
			continue
		}

		// Skip if already imported (exists in mapping)
		if _, exists := existingMapping[channel.ID]; exists {
			logger.Info("Room '%s' already imported, skipped", channel.DisplayName)
			stats.RoomsSkipped++
			continue
		}

		topic := channel.Purpose
		if topic == "" {
			topic = channel.Header
		}

		var roomAlias, owner string
		if opts != nil && opts.PreserveOwnerAndAlias && teamByID != nil {
			if team, ok := teamByID[channel.TeamID]; ok {
				roomAlias = sanitizeAliasLocalpart(team.Name + "-" + channel.Name)
			} else {
				roomAlias = sanitizeAliasLocalpart(channel.Name)
			}
			// Group channels (type G) often have empty creator_id; use first member from display_name as creator
			if channel.IsGroup() && channel.CreatorID == "" {
				owner = firstMemberFromGroupDisplayName(channel.DisplayName, users, userMapping)
				if owner != "" {
					logger.Info("Room '%s' (group channel): alias=%q owner=%s (first member from display_name)", channel.DisplayName, roomAlias, owner)
				} else {
					logger.Warn("Room '%s' (group channel): could not resolve any member (all may be disabled or not in mapping) from display_name %q; using fallback", channel.DisplayName, channel.DisplayName)
					owner = i.resolveRoomOwner("", userMapping, opts)
					logger.Info("Room '%s' (group channel): alias=%q owner=%s (fallback)", channel.DisplayName, roomAlias, owner)
				}
			} else {
				owner = i.resolveRoomOwner(channel.CreatorID, userMapping, opts)
				logger.Info("Room '%s' (channel %s): alias=%q owner=%s creator_id=%q", channel.DisplayName, channel.Name, roomAlias, owner, channel.CreatorID)
			}
		}

		resp, err := i.client.CreateRegularRoom(channel.DisplayName, topic, channel.IsPublic(), roomAlias, owner)
		if err != nil {
			logger.Error("Failed to create room '%s': %v", channel.DisplayName, err)
			stats.RoomsFailed++
			continue
		}

		logger.Success("Created room '%s' -> %s", channel.DisplayName, resp.RoomID)
		mapping[channel.ID] = resp.RoomID
		stats.RoomsCreated++
	}

	return mapping, stats, nil
}

// ApplyTeamMemberships invites users to spaces based on team memberships
func (i *Importer) ApplyTeamMemberships(
	memberships []mattermost.TeamMember,
	userMapping map[string]string,
	spaceMapping map[string]string,
	progress ImportProgressCallback,
) (*ImportStats, error) {
	stats := &ImportStats{}
	total := len(memberships)

	logger.Info("Starting team membership import: %d memberships to process", total)

	for idx, membership := range memberships {
		if progress != nil {
			progress("team_memberships", idx+1, total, "")
		}

		// Skip deleted memberships
		if membership.IsDeleted() {
			logger.Info("Team membership %d/%d: deleted, skipping", idx+1, total)
			stats.MembersSkipped++
			continue
		}

		// Get Matrix IDs
		userID, userExists := userMapping[membership.UserID]
		spaceID, spaceExists := spaceMapping[membership.TeamID]

		if !userExists || !spaceExists {
			if !userExists {
				logger.Warn("Team membership %d/%d skipped: user %s not in mapping", idx+1, total, membership.UserID)
			}
			if !spaceExists {
				logger.Warn("Team membership %d/%d skipped: team %s not in mapping", idx+1, total, membership.TeamID)
			}
			stats.MembersSkipped++
			continue
		}

		if i.client.ForceJoinEnabled() {
			logger.Info("Team membership %d/%d: force-joining %s to space %s", idx+1, total, userID, spaceID)
		} else {
			logger.Info("Team membership %d/%d: inviting %s to space %s", idx+1, total, userID, spaceID)
		}
		if err := i.client.AddUserToRoom(spaceID, userID); err != nil {
			logger.Error("Team membership %d/%d failed: %s -> %s: %v", idx+1, total, userID, spaceID, err)
			stats.MembersFailed++
			continue
		}

		logger.Success("Team membership %d/%d: %s added to space", idx+1, total, userID)
		stats.MembersAdded++
	}

	logger.Info("Team membership import completed: added=%d, skipped=%d, failed=%d", 
		stats.MembersAdded, stats.MembersSkipped, stats.MembersFailed)

	return stats, nil
}

// ApplyChannelMemberships invites users to rooms based on channel memberships.
// For group channels (type G), after inviting all members we set equal power levels (50) for everyone to match Mattermost.
// Direct channels (type D) are skipped: both participants were already added during ImportDirectChannelsAsDMs (CreateDirectRoom).
// When force_join is enabled and the client has an AS token, for group (G) and private (P) rooms the admin is invited by the room owner,
// joins with power level 100, force-joins members, then leaves. defaultRoomOwnerID is used when channel has no creator_id (e.g. group channels).
// channels is used to identify group and direct channels; can be nil (then no equal power levels, and no DM skip).
func (i *Importer) ApplyChannelMemberships(
	channels []mattermost.Channel,
	memberships []mattermost.ChannelMember,
	userMapping map[string]string,
	roomMapping map[string]string,
	defaultRoomOwnerID string,
	progress ImportProgressCallback,
) (*ImportStats, error) {
	stats := &ImportStats{}
	total := len(memberships)

	channelByID := make(map[string]mattermost.Channel)
	groupChannelIDs := make(map[string]bool)
	directChannelIDs := make(map[string]bool)
	for _, ch := range channels {
		channelByID[ch.ID] = ch
		if ch.IsGroup() {
			groupChannelIDs[ch.ID] = true
		}
		if ch.IsDirect() {
			directChannelIDs[ch.ID] = true
		}
	}

	// Per group-channel room ID, collect Matrix user IDs that were added (for equal power levels later)
	groupRoomMembers := make(map[string][]string)

	// When force_join + AS: rooms we joined (via owner invite) so we can leave after processing all memberships
	roomsToLeaveAfter := make(map[string]struct{})
	ensuredRoomsForForceJoin := make(map[string]struct{})

	logger.Info("Starting channel membership import: %d memberships to process (%d DM channels will be skipped)", total, len(directChannelIDs))

	for idx, membership := range memberships {
		if progress != nil {
			progress("channel_memberships", idx+1, total, "")
		}

		if directChannelIDs[membership.ChannelID] {
			stats.MembersSkipped++
			continue
		}

		userID, userExists := userMapping[membership.UserID]
		roomID, roomExists := roomMapping[membership.ChannelID]

		if !userExists || !roomExists {
			if !userExists {
				logger.Warn("Channel membership %d/%d skipped: user %s not in mapping", idx+1, total, membership.UserID)
			}
			if !roomExists {
				logger.Warn("Channel membership %d/%d skipped: channel %s not in mapping", idx+1, total, membership.ChannelID)
			}
			stats.MembersSkipped++
			continue
		}

		// For group (G) and private (P) rooms with force_join: ensure admin is in the room once (owner invites, admin joins with PL 100).
		channel := channelByID[membership.ChannelID]
		if i.client.ForceJoinEnabled() && i.client.HasASToken() && (channel.IsGroup() || channel.IsPrivate()) {
			if _, already := ensuredRoomsForForceJoin[roomID]; !already {
				roomOwnerID := defaultRoomOwnerID
				ownerFromCreator := false
				if channel.CreatorID != "" {
					if mx, ok := userMapping[channel.CreatorID]; ok && mx != "" {
						roomOwnerID = mx
						ownerFromCreator = true
					}
				}
				if roomOwnerID != "" {
					logger.Debug("ApplyChannelMemberships: ensuring admin in room %s (owner %s) for membership %s -> room", roomID, roomOwnerID, userID)
					roomEnsureErr := i.client.ensureAdminInRoomWithPower(roomID, roomOwnerID, 100)
					if roomEnsureErr != nil && ownerFromCreator && defaultRoomOwnerID != "" && defaultRoomOwnerID != roomOwnerID {
						logger.Debug("ApplyChannelMemberships: retry ensure admin in room %s with fallback owner %s (primary owner %s failed)", roomID, defaultRoomOwnerID, roomOwnerID)
						if retryErr := i.client.ensureAdminInRoomWithPower(roomID, defaultRoomOwnerID, 100); retryErr == nil {
							roomEnsureErr = nil
						} else {
							roomEnsureErr = retryErr
						}
					}
					if roomEnsureErr != nil {
						logger.Warn("ApplyChannelMemberships: could not ensure admin in room %s (owner %s): %v", roomID, roomOwnerID, roomEnsureErr)
					} else {
						ensuredRoomsForForceJoin[roomID] = struct{}{}
						roomsToLeaveAfter[roomID] = struct{}{}
						logger.Info("ApplyChannelMemberships: admin joined room %s (PL 100) for force-join", roomID)
					}
				}
			}
		}

		if i.client.ForceJoinEnabled() {
			logger.Info("Channel membership %d/%d: force-joining %s to room %s", idx+1, total, userID, roomID)
		} else {
			logger.Info("Channel membership %d/%d: inviting %s to room %s", idx+1, total, userID, roomID)
		}
		logger.Debug("ApplyChannelMemberships: AddUserToRoom room=%s user=%s (membership %d/%d)", roomID, userID, idx+1, total)

		if groupChannelIDs[membership.ChannelID] {
			groupRoomMembers[roomID] = append(groupRoomMembers[roomID], userID)
		}

		if err := i.client.AddUserToRoom(roomID, userID); err != nil {
			logger.Error("Channel membership %d/%d failed: %s -> %s: %v", idx+1, total, userID, roomID, err)
			stats.MembersFailed++
			continue
		}

		logger.Success("Channel membership %d/%d: %s added to room", idx+1, total, userID)
		stats.MembersAdded++
	}

	// Leave rooms we joined for force-join so the admin is not left in group/private rooms
	for roomID := range roomsToLeaveAfter {
		logger.Debug("ApplyChannelMemberships: LeaveRoom %s (admin leaving after force-join)", roomID)
		if err := i.client.LeaveRoom(roomID); err != nil {
			logger.Warn("ApplyChannelMemberships: admin leave room %s after force-join: %v", roomID, err)
		} else {
			logger.Info("ApplyChannelMemberships: admin left room %s", roomID)
		}
	}

	// For group channels, set equal power levels (50) for all members to match Mattermost
	for roomID, memberIDs := range groupRoomMembers {
		if len(memberIDs) == 0 {
			continue
		}
		userLevels := make(map[string]int)
		for _, u := range memberIDs {
			userLevels[u] = 50
		}
		if err := i.client.SetPowerLevelsBulk(roomID, userLevels); err != nil {
			logger.Warn("ApplyChannelMemberships: could not set equal power levels for group room %s: %v", roomID, err)
		} else {
			logger.Info("ApplyChannelMemberships: set equal power level 50 for %d members in group room %s", len(memberIDs), roomID)
		}
	}

	logger.Info("Channel membership import completed: added=%d, skipped=%d, failed=%d",
		stats.MembersAdded, stats.MembersSkipped, stats.MembersFailed)

	return stats, nil
}

// ImportDirectChannelsAsDMs imports Mattermost direct message channels (type D) as Matrix DMs.
// Room creator is preferred as a real user (not the AS); is_direct and m.direct are set so both users see the room under "People".
// When channel has no creator_id, name format is "senderID_receiverID" (first = sender, second = receiver); sender is used as room creator.
func (i *Importer) ImportDirectChannelsAsDMs(
	directChannels []mattermost.Channel,
	userMapping map[string]string,
	existingMapping map[string]string,
	progress ImportProgressCallback,
) (map[string]string, *ImportStats, error) {
	mapping := make(map[string]string)
	stats := &ImportStats{}
	total := len(directChannels)

	for k, v := range existingMapping {
		mapping[k] = v
	}

	if !i.client.HasASToken() {
		logger.Warn("ImportDirectChannelsAsDMs: Application Service token not set; DMs will be created but m.direct (People list) cannot be set for users")
	}

	logger.Info("ImportDirectChannelsAsDMs: processing %d direct channels", total)

	for idx, channel := range directChannels {
		if progress != nil {
			progress("dm_rooms", idx+1, total, channel.ID)
		}

		if channel.IsDeleted() {
			logger.Info("DM channel %s deleted, skipping", channel.ID)
			stats.RoomsSkipped++
			continue
		}

		if _, exists := existingMapping[channel.ID]; exists {
			logger.Info("DM channel %s already imported, skipping", channel.ID)
			stats.RoomsSkipped++
			continue
		}

		// Resolve the two Matrix user IDs for this DM
		senderID, receiverID, err := channel.DMParticipantIDs()
		if err != nil {
			logger.Error("ImportDirectChannelsAsDMs: channel %s: %v", channel.ID, err)
			stats.RoomsFailed++
			continue
		}
		creatorMX, ok1 := userMapping[senderID]
		otherMX, ok2 := userMapping[receiverID]
		if !ok1 || !ok2 {
			if !ok1 {
				logger.Warn("ImportDirectChannelsAsDMs: channel %s sender %s not in user mapping, skipping", channel.ID, senderID)
			}
			if !ok2 {
				logger.Warn("ImportDirectChannelsAsDMs: channel %s receiver %s not in user mapping, skipping", channel.ID, receiverID)
			}
			stats.RoomsSkipped++
			continue
		}
		// When Mattermost has creator_id, use it as room creator; other participant is the one from name that isn't creator
		if channel.CreatorID != "" {
			if mx, ok := userMapping[channel.CreatorID]; ok {
				creatorMX = mx
				if channel.CreatorID == senderID {
					otherMX = userMapping[receiverID]
				} else {
					otherMX = userMapping[senderID]
				}
			}
		}

		if creatorMX == "" || otherMX == "" {
			logger.Warn("ImportDirectChannelsAsDMs: channel %s could not resolve both users (creator=%q other=%q), skipping", channel.ID, creatorMX, otherMX)
			stats.RoomsSkipped++
			continue
		}
		if creatorMX == otherMX {
			logger.Warn("ImportDirectChannelsAsDMs: channel %s same user for both sides, skipping", channel.ID)
			stats.RoomsSkipped++
			continue
		}

		resp, err := i.client.CreateDirectRoom(creatorMX, otherMX)
		if err != nil {
			logger.Error("ImportDirectChannelsAsDMs: failed to create DM for channel %s (%s <-> %s): %v", channel.ID, creatorMX, otherMX, err)
			stats.RoomsFailed++
			continue
		}

		logger.Success("ImportDirectChannelsAsDMs: created DM %s for channel %s (%s <-> %s)", resp.RoomID, channel.ID, creatorMX, otherMX)
		mapping[channel.ID] = resp.RoomID
		stats.RoomsCreated++
	}

	logger.Info("ImportDirectChannelsAsDMs completed: created=%d, skipped=%d, failed=%d",
		stats.RoomsCreated, stats.RoomsSkipped, stats.RoomsFailed)

	return mapping, stats, nil
}

// LinkRoomsToSpaces links rooms to their parent spaces based on channel-team relationships.
// When userMapping and defaultSpaceOwnerID are provided and the client has an AS token, the admin is invited
// into each space and room (by the owner) with power level 50 before linking, so invite-only/restricted rooms work.
// publicRoomJoinRules: "space_members" (default) sets public rooms to restricted join so only space members can join; "public" leaves join rule as-is.
func (i *Importer) LinkRoomsToSpaces(
	channels []mattermost.Channel,
	spaceMapping map[string]string,
	roomMapping map[string]string,
	userMapping map[string]string,
	defaultSpaceOwnerID string,
	publicRoomJoinRules string,
	progress ImportProgressCallback,
) (*ImportStats, error) {
	stats := &ImportStats{}
	total := len(channels)
	ensuredSpaceIDs := make(map[string]struct{})

	for idx, channel := range channels {
		if progress != nil {
			progress("linking", idx+1, total, channel.DisplayName)
		}

		if channel.TeamID == "" {
			continue
		}
		if channel.IsDirect() {
			continue // DMs are not linked to spaces
		}

		spaceID, spaceExists := spaceMapping[channel.TeamID]
		roomID, roomExists := roomMapping[channel.ID]

		if !spaceExists || !roomExists {
			continue
		}

		// When rooms/spaces were created as owner via AS, admin must be invited (by owner) and given power before linking.
		if i.client.HasASToken() && defaultSpaceOwnerID != "" {
			if _, done := ensuredSpaceIDs[spaceID]; !done {
				logger.Debug("LinkRoomsToSpaces: ensuring admin in space %s (room %q)", spaceID, channel.DisplayName)
				if err := i.client.ensureAdminInSpaceWithPower(spaceID, defaultSpaceOwnerID, 50); err != nil {
					logger.Warn("LinkRoomsToSpaces: could not ensure admin in space %s: %v (linking may fail)", spaceID, err)
				} else {
					logger.Info("LinkRoomsToSpaces: admin ensured in space %s", spaceID)
				}
				ensuredSpaceIDs[spaceID] = struct{}{}
			}
			roomOwnerID := defaultSpaceOwnerID
			ownerFromCreator := false
			if channel.CreatorID != "" && userMapping != nil {
				if mx, ok := userMapping[channel.CreatorID]; ok && mx != "" {
					roomOwnerID = mx
					ownerFromCreator = true
				}
			}
			logger.Debug("LinkRoomsToSpaces: ensuring admin in room %s (owner %s) for room %q", roomID, roomOwnerID, channel.DisplayName)
			roomEnsureErr := i.client.ensureAdminInRoomWithPower(roomID, roomOwnerID, 50)
			if roomEnsureErr != nil && ownerFromCreator && defaultSpaceOwnerID != "" && defaultSpaceOwnerID != roomOwnerID {
				logger.Debug("LinkRoomsToSpaces: retry ensure admin in room %s with fallback owner %s (primary owner %s failed)", roomID, defaultSpaceOwnerID, roomOwnerID)
				if retryErr := i.client.ensureAdminInRoomWithPower(roomID, defaultSpaceOwnerID, 50); retryErr == nil {
					roomEnsureErr = nil
				} else {
					roomEnsureErr = retryErr
				}
			}
			if roomEnsureErr != nil {
				logger.Warn("LinkRoomsToSpaces: could not ensure admin in room %s (owner %s): %v", roomID, roomOwnerID, roomEnsureErr)
			}
		}

		logger.Debug("LinkRoomsToSpaces: AddRoomToSpace room %q -> space %s", channel.DisplayName, spaceID)
		if err := i.client.AddRoomToSpace(spaceID, roomID, true); err != nil {
			logger.Error("Failed to link room '%s' to space: %v", channel.DisplayName, err)
			stats.RoomsLinkFailed++
			continue
		}

		// Set space as parent of room
		logger.Debug("LinkRoomsToSpaces: SetRoomParent room %q roomID=%s spaceID=%s", channel.DisplayName, roomID, spaceID)
		if err := i.client.SetRoomParent(roomID, spaceID, true); err != nil {
			// Non-critical error, room is still linked as child
			logger.Warn("Failed to set parent for room '%s': %v", channel.DisplayName, err)
		}

		// Public rooms: optionally set join_rules to "restricted" so only space members can join
		if channel.IsPublic() && publicRoomJoinRules == "space_members" {
			logger.Debug("LinkRoomsToSpaces: SetJoinRulesRestricted room %q", channel.DisplayName)
			if err := i.client.SetJoinRulesRestricted(roomID, spaceID); err != nil {
				logger.Warn("Failed to set join_rules (Space members) for public room '%s': %v", channel.DisplayName, err)
			} else {
				logger.Info("Set join_rules to Space members for public room '%s'", channel.DisplayName)
			}
		}

		logger.Success("Linked room '%s' to space", channel.DisplayName)
		stats.RoomsLinked++
	}

	return stats, nil
}

// ImportAssetsResult holds the result of importing assets
type ImportAssetsResult struct {
	UserMapping  map[string]string
	SpaceMapping map[string]string
	RoomMapping  map[string]string
	Stats        *ImportStats
}

// ExistingMappings holds existing mappings to skip already imported items
type ExistingMappings struct {
	Users    map[string]string
	Spaces   map[string]string
	Rooms    map[string]string
}

// ImportAssets imports all assets (users, teams as spaces, channels as rooms).
// If existingMappings is provided, already imported items will be skipped.
// roomOpts configures owner and local alias for rooms/spaces when PreserveOwnerAndAlias is true.
func (i *Importer) ImportAssets(assets *mattermost.Assets, existingMappings *ExistingMappings, roomOpts *RoomImportOptions, progress ImportProgressCallback) (*ImportAssetsResult, error) {
	result := &ImportAssetsResult{
		Stats: &ImportStats{},
	}

	logger.Info("=== ImportAssets Started ===")
	logger.Info("Assets to import: %d users, %d teams, %d channels",
		len(assets.Users), len(assets.Teams), len(assets.Channels))

	// Initialize empty mappings if not provided
	if existingMappings == nil {
		existingMappings = &ExistingMappings{
			Users:  make(map[string]string),
			Spaces: make(map[string]string),
			Rooms:  make(map[string]string),
		}
		logger.Info("No existing mappings provided, starting fresh")
	} else {
		logger.Info("Existing mappings: %d users, %d spaces, %d rooms",
			len(existingMappings.Users), len(existingMappings.Spaces), len(existingMappings.Rooms))
	}

	// Import users
	logger.Info("=== Starting User Import ===")
	userMapping, userStats, err := i.ImportUsers(assets.Users, existingMappings.Users, progress)
	if err != nil {
		logger.Error("User import failed: %v", err)
		return nil, fmt.Errorf("failed to import users: %w", err)
	}
	result.UserMapping = userMapping
	result.Stats.UsersCreated = userStats.UsersCreated
	result.Stats.UsersSkipped = userStats.UsersSkipped
	result.Stats.UsersFailed = userStats.UsersFailed
	logger.Info("User import completed: created=%d, skipped=%d, failed=%d",
		userStats.UsersCreated, userStats.UsersSkipped, userStats.UsersFailed)

	// Import teams as spaces (with optional owner and alias from roomOpts)
	spaceMapping, spaceStats, err := i.ImportTeamsAsSpaces(assets.Teams, existingMappings.Spaces, userMapping, roomOpts, progress)
	if err != nil {
		return nil, fmt.Errorf("failed to import teams: %w", err)
	}
	result.SpaceMapping = spaceMapping
	result.Stats.SpacesCreated = spaceStats.SpacesCreated
	result.Stats.SpacesSkipped = spaceStats.SpacesSkipped
	result.Stats.SpacesFailed = spaceStats.SpacesFailed

	// Build teamByID for channel alias (team+name)
	teamByID := make(map[string]mattermost.Team)
	for _, t := range assets.Teams {
		teamByID[t.ID] = t
	}

	// Import channels as rooms (with optional owner and alias from roomOpts; users used for group channel first-member)
	roomMapping, roomStats, err := i.ImportChannelsAsRooms(assets.Channels, existingMappings.Rooms, teamByID, assets.Users, userMapping, roomOpts, progress)
	if err != nil {
		return nil, fmt.Errorf("failed to import channels: %w", err)
	}
	result.RoomMapping = roomMapping
	result.Stats.RoomsCreated = roomStats.RoomsCreated
	result.Stats.RoomsSkipped = roomStats.RoomsSkipped
	result.Stats.RoomsFailed = roomStats.RoomsFailed

	return result, nil
}

// MessageImportStats holds statistics about message import
type MessageImportStats struct {
	MessagesImported int `json:"messages_imported"`
	MessagesSkipped  int `json:"messages_skipped"` // Already imported
	MessagesFailed   int `json:"messages_failed"`
	RepliesImported  int `json:"replies_imported"`
	RepliesFailed    int `json:"replies_failed"`  // Reply target not found
	FilesLinked      int `json:"files_linked"`    // Files added as links
	FilesUploaded    int `json:"files_uploaded"`  // Files uploaded to Matrix
	FilesSkipped     int `json:"files_skipped"`   // Files skipped
}

// FileConfig holds file migration settings
type FileConfig struct {
	Mode         string // "link", "upload", or "skip"
	S3PublicURL  string // Base URL for S3 files
	MaxUploadSize int64 // Max file size for upload
}

// MessageImportCallback is called for each message imported
type MessageImportCallback func(current, total int, channelName string, status string)

// ImportMessagesResult contains the result of message import
type ImportMessagesResult struct {
	Stats    *MessageImportStats
	Mapping  map[string]string // MattermostID -> MatrixEventID
	Errors   []string
}

// ImportMessages imports messages from Mattermost posts to Matrix rooms
// This requires Application Service token for timestamp support
func (i *Importer) ImportMessages(
	posts []mattermost.Post,
	channelToRoom map[string]string,      // Mattermost channel ID -> Matrix room ID
	userMapping map[string]string,         // Mattermost user ID -> Matrix user ID
	existingMapping map[string]string,     // Mattermost post ID -> Matrix event ID (for resume)
	progress MessageImportCallback,
) (*ImportMessagesResult, error) {
	result := &ImportMessagesResult{
		Stats:   &MessageImportStats{},
		Mapping: make(map[string]string),
		Errors:  []string{},
	}
	
	if !i.client.HasASToken() {
		logger.Warn("No Application Service token configured - messages will be imported without original timestamps")
	}
	
	total := len(posts)
	logger.Info("Starting message import: %d posts to process", total)
	
	// Collect all existing mappings
	for k, v := range existingMapping {
		result.Mapping[k] = v
	}
	
	// Sort posts by timestamp (they should already be sorted, but just in case)
	// This ensures parent messages are imported before replies
	
	// Process messages in order
	for idx, post := range posts {
		// Check if already imported
		if _, exists := existingMapping[post.ID]; exists {
			result.Stats.MessagesSkipped++
			if progress != nil {
				progress(idx+1, total, post.ChannelID, "skipped")
			}
			continue
		}
		
		// Get target room
		roomID, roomExists := channelToRoom[post.ChannelID]
		if !roomExists {
			result.Stats.MessagesFailed++
			result.Errors = append(result.Errors, fmt.Sprintf("No room mapping for channel %s (post %s)", post.ChannelID, post.ID))
			if progress != nil {
				progress(idx+1, total, post.ChannelID, "failed:no_room")
			}
			continue
		}
		
		// Get sender
		senderID, userExists := userMapping[post.UserID]
		if !userExists {
			// Fall back to empty sender (will use AS bot)
			senderID = ""
			logger.Warn("No user mapping for user %s, message will be sent as AS bot", post.UserID)
		}
		
		// Handle reply
		var eventID string
		
		if post.IsReply() {
			// This is a reply - find parent event ID
			parentEventID, parentExists := result.Mapping[post.RootID]
			if !parentExists {
				// Parent not yet imported or doesn't exist
				result.Stats.RepliesFailed++
				result.Errors = append(result.Errors, fmt.Sprintf("Parent post %s not found for reply %s", post.RootID, post.ID))
				
				// Import as regular message instead of failing
				resp, sendErr := i.client.SendMessageWithTimestamp(roomID, post.Message, post.CreateAt, senderID)
				if sendErr != nil {
					result.Stats.MessagesFailed++
					result.Errors = append(result.Errors, fmt.Sprintf("Failed to send message %s: %v", post.ID, sendErr))
					if progress != nil {
						progress(idx+1, total, post.ChannelID, "failed:send_error")
					}
					continue
				}
				eventID = resp.EventID
			} else {
				// Send as reply
				resp, sendErr := i.client.SendReplyWithTimestamp(roomID, post.Message, parentEventID, post.CreateAt, senderID)
				if sendErr != nil {
					result.Stats.RepliesFailed++
					result.Errors = append(result.Errors, fmt.Sprintf("Failed to send reply %s: %v", post.ID, sendErr))
					if progress != nil {
						progress(idx+1, total, post.ChannelID, "failed:reply_error")
					}
					continue
				}
				eventID = resp.EventID
				result.Stats.RepliesImported++
			}
		} else {
			// Regular message
			resp, sendErr := i.client.SendMessageWithTimestamp(roomID, post.Message, post.CreateAt, senderID)
			if sendErr != nil {
				result.Stats.MessagesFailed++
				result.Errors = append(result.Errors, fmt.Sprintf("Failed to send message %s: %v", post.ID, sendErr))
				if progress != nil {
					progress(idx+1, total, post.ChannelID, "failed:send_error")
				}
				continue
			}
			eventID = resp.EventID
		}
		
		// Store mapping
		result.Mapping[post.ID] = eventID
		result.Stats.MessagesImported++
		
		if progress != nil {
			progress(idx+1, total, post.ChannelID, "imported")
		}
		
		// Log progress every 100 messages
		if (idx+1) % 100 == 0 {
			logger.Info("Message import progress: %d/%d (%.1f%%)", idx+1, total, float64(idx+1)/float64(total)*100)
		}
	}
	
	logger.Info("Message import completed: imported=%d, skipped=%d, failed=%d, replies=%d",
		result.Stats.MessagesImported, result.Stats.MessagesSkipped, 
		result.Stats.MessagesFailed, result.Stats.RepliesImported)
	
	return result, nil
}

// ImportMessagesWithFiles imports messages with file attachments
// filesByPost maps post ID to list of file infos
func (i *Importer) ImportMessagesWithFiles(
	posts []mattermost.Post,
	channelToRoom map[string]string,
	userMapping map[string]string,
	existingMapping map[string]string,
	filesByPost map[string][]mattermost.FileInfo,
	fileConfig *FileConfig,
	progress MessageImportCallback,
) (*ImportMessagesResult, error) {
	result := &ImportMessagesResult{
		Stats:   &MessageImportStats{},
		Mapping: make(map[string]string),
		Errors:  []string{},
	}
	
	if !i.client.HasASToken() {
		logger.Warn("No Application Service token configured - messages will be imported without original timestamps")
	}
	
	total := len(posts)
	logger.Info("Starting message import with files: %d posts to process", total)
	
	// Default file config
	if fileConfig == nil {
		fileConfig = &FileConfig{Mode: "skip"}
	}
	
	// Collect all existing mappings
	for k, v := range existingMapping {
		result.Mapping[k] = v
	}
	
	// Process messages in order
	for idx, post := range posts {
		// Check if already imported
		if _, exists := existingMapping[post.ID]; exists {
			result.Stats.MessagesSkipped++
			if progress != nil {
				progress(idx+1, total, post.ChannelID, "skipped")
			}
			continue
		}
		
		// Get target room
		roomID, roomExists := channelToRoom[post.ChannelID]
		if !roomExists {
			result.Stats.MessagesFailed++
			result.Errors = append(result.Errors, fmt.Sprintf("No room mapping for channel %s (post %s)", post.ChannelID, post.ID))
			if progress != nil {
				progress(idx+1, total, post.ChannelID, "failed:no_room")
			}
			continue
		}
		
		// Get sender
		senderID, userExists := userMapping[post.UserID]
		if !userExists {
			senderID = ""
			logger.Warn("No user mapping for user %s, message will be sent as AS bot", post.UserID)
		}
		
		// Build message content with files
		messageContent := post.Message
		files := filesByPost[post.ID]
		
		// Append file links if mode is "link"
		if fileConfig.Mode == "link" && len(files) > 0 && fileConfig.S3PublicURL != "" {
			for _, file := range files {
				fileURL := strings.TrimSuffix(fileConfig.S3PublicURL, "/") + "/" + file.Path
				messageContent += fmt.Sprintf("\n\n📎 [%s](%s)", file.Name, fileURL)
				result.Stats.FilesLinked++
			}
		}
		
		// Handle reply
		var eventID string
		
		if post.IsReply() {
			parentEventID, parentExists := result.Mapping[post.RootID]
			if !parentExists {
				result.Stats.RepliesFailed++
				result.Errors = append(result.Errors, fmt.Sprintf("Parent post %s not found for reply %s", post.RootID, post.ID))
				
				resp, sendErr := i.client.SendMessageWithTimestamp(roomID, messageContent, post.CreateAt, senderID)
				if sendErr != nil {
					result.Stats.MessagesFailed++
					result.Errors = append(result.Errors, fmt.Sprintf("Failed to send message %s: %v", post.ID, sendErr))
					if progress != nil {
						progress(idx+1, total, post.ChannelID, "failed:send_error")
					}
					continue
				}
				eventID = resp.EventID
			} else {
				resp, sendErr := i.client.SendReplyWithTimestamp(roomID, messageContent, parentEventID, post.CreateAt, senderID)
				if sendErr != nil {
					result.Stats.RepliesFailed++
					result.Errors = append(result.Errors, fmt.Sprintf("Failed to send reply %s: %v", post.ID, sendErr))
					if progress != nil {
						progress(idx+1, total, post.ChannelID, "failed:reply_error")
					}
					continue
				}
				eventID = resp.EventID
				result.Stats.RepliesImported++
			}
		} else {
			resp, sendErr := i.client.SendMessageWithTimestamp(roomID, messageContent, post.CreateAt, senderID)
			if sendErr != nil {
				result.Stats.MessagesFailed++
				result.Errors = append(result.Errors, fmt.Sprintf("Failed to send message %s: %v", post.ID, sendErr))
				if progress != nil {
					progress(idx+1, total, post.ChannelID, "failed:send_error")
				}
				continue
			}
			eventID = resp.EventID
		}
		
		// Store mapping
		result.Mapping[post.ID] = eventID
		result.Stats.MessagesImported++
		
		if progress != nil {
			progress(idx+1, total, post.ChannelID, "imported")
		}
		
		// Log progress every 100 messages
		if (idx+1) % 100 == 0 {
			logger.Info("Message import progress: %d/%d (%.1f%%) - files linked: %d",
				idx+1, total, float64(idx+1)/float64(total)*100, result.Stats.FilesLinked)
		}
	}
	
	logger.Info("Message import completed: imported=%d, skipped=%d, failed=%d, replies=%d, files_linked=%d",
		result.Stats.MessagesImported, result.Stats.MessagesSkipped, 
		result.Stats.MessagesFailed, result.Stats.RepliesImported, result.Stats.FilesLinked)
	
	return result, nil
}

