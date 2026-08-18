package matrix

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
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
	SpaceVisibility       string // "invite_only" (default), "public", "from_mattermost"
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

	// passwordPolicy decides what password newly created users get. Defaults to a distinct
	// random password per user; override with SetPasswordPolicy.
	passwordPolicy PasswordPolicy
	// generatedCredentials accumulates passwords created during this run so the caller can
	// persist them. Empty when the policy generates no passwords.
	generatedCredentials []UserCredential

	// checkpointFn, when set, is called every checkpointEvery imported messages with the
	// post ID -> event ID mapping so far. Message import runs for days on a large instance;
	// without this, an interruption loses every message already sent, because the mapping is
	// only written once the whole run finishes and a restart would re-import them as duplicates.
	checkpointFn    func(map[string]string)
	checkpointEvery int

	// reactionCheckpointFn mirrors checkpointFn for the reaction pass, which faces the same
	// problem: a busy instance has thousands of reactions, each one an API call under the rate
	// limiter, and Matrix does not deduplicate annotations server-side. Losing the record of
	// what was already sent means the next run stacks a second copy of every reaction.
	reactionCheckpointFn    func(map[string]string)
	reactionCheckpointEvery int

	// knownMentionUsers holds the localparts of users this run knows exist, derived from the
	// migration's user mapping. A nil map disables mention gating.
	knownMentionUsers map[string]struct{}
	// mentionExistsCache caches homeserver lookups for mention candidates outside the mapping,
	// negative results included, so each distinct name costs at most one request per run.
	mentionExistsCache map[string]bool

	// historyJoins records memberships created purely so a past author could be
	// impersonated while their messages were replayed. LeaveHistoryMemberships undoes them.
	historyJoins []HistoryMembership
}

// HistoryJoins returns the memberships this import created solely to replay history.
func (i *Importer) HistoryJoins() []HistoryMembership {
	return i.historyJoins
}

// NewImporter creates a new importer
func NewImporter(client *Client) *Importer {
	return &Importer{client: client, passwordPolicy: DefaultPasswordPolicy()}
}

// SetPasswordPolicy controls how passwords are assigned to newly created users.
func (i *Importer) SetPasswordPolicy(policy PasswordPolicy) {
	i.passwordPolicy = policy
}

// GeneratedCredentials returns the passwords generated during this import, in creation
// order. Empty when the policy is PasswordModeNone. The caller is responsible for storing
// these securely; they are not written anywhere by the importer.
func (i *Importer) GeneratedCredentials() []UserCredential {
	return i.generatedCredentials
}

// ImportProgressCallback is called to report import progress
type ImportProgressCallback func(stage string, current, total int, item string)

// matrixRoomMention is what Matrix calls a message addressed to everyone in the room.
const matrixRoomMention = "@room"

// mattermostBroadcastMentions maps Mattermost's channel-wide mentions to their Matrix
// spelling, or to "" where Matrix has nothing equivalent.
//
// @all and @channel mean the same thing in Mattermost and translate cleanly. @here does not:
// it addresses whoever happens to be online, a distinction Matrix has no concept of, and
// rendering it as @room in the archive would claim the author addressed more people than they
// did. It stays as written.
var mattermostBroadcastMentions = map[string]string{
	"all":     matrixRoomMention,
	"channel": matrixRoomMention,
	"here":    "",
}

func isMentionChar(b byte) bool {
	return (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') ||
		b == '_' || b == '-' || b == '.'
}

// normalizeMatrixMentions rewrites plain @username mentions to @username:homeserver.
// It skips already-formatted Matrix IDs and Mattermost broadcast mentions.
func (i *Importer) normalizeMatrixMentions(message string) string {
	if message == "" {
		return message
	}

	var out strings.Builder
	out.Grow(len(message) + 16)

	for idx := 0; idx < len(message); {
		if message[idx] != '@' {
			out.WriteByte(message[idx])
			idx++
			continue
		}

		if idx > 0 {
			prev := message[idx-1]
			if isMentionChar(prev) || prev == '@' {
				out.WriteByte('@')
				idx++
				continue
			}
		}

		j := idx + 1
		for j < len(message) && isMentionChar(message[j]) {
			j++
		}
		if j == idx+1 {
			out.WriteByte('@')
			idx++
			continue
		}

		username := message[idx+1 : j]
		for len(username) > 0 && username[len(username)-1] == '.' {
			username = username[:len(username)-1]
			j--
		}
		if username == "" {
			out.WriteByte('@')
			idx++
			continue
		}

		if replacement, isBroadcast := mattermostBroadcastMentions[strings.ToLower(username)]; isBroadcast {
			if replacement != "" {
				out.WriteString(replacement)
			} else {
				out.WriteString(message[idx:j])
			}
			idx = j
			continue
		}

		if j < len(message) && message[j] == ':' {
			out.WriteString(message[idx:j])
			idx = j
			continue
		}

		// Only rewrite names that resolve to a real user. Prose like "email without
		// @example.com" is a valid mention shape but nobody's user ID.
		if !i.mentionTargetExists(username) {
			out.WriteString(message[idx:j])
			idx = j
			continue
		}

		out.WriteString(i.client.FormatUserID(username))
		idx = j
	}

	return out.String()
}

// SetKnownMentionUsers records the users this run knows exist, taking them from a Mattermost
// user ID -> Matrix user ID mapping, and switches mention gating on. Until it is called,
// mentions are rewritten unconditionally.
func (i *Importer) SetKnownMentionUsers(userMapping map[string]string) {
	known := make(map[string]struct{}, len(userMapping))
	for _, matrixUserID := range userMapping {
		if localpart := mentionLocalpart(matrixUserID); localpart != "" {
			known[localpart] = struct{}{}
		}
	}
	i.knownMentionUsers = known
	i.mentionExistsCache = make(map[string]bool)
}

// mentionTargetExists reports whether @username should become a Matrix mention. Users in the
// migration's own mapping are known to exist. Anything else is checked against the homeserver
// once and cached; a name that cannot be verified stays plain text, since a wrong pill corrupts
// the message text while an unlinked @name still reads correctly.
func (i *Importer) mentionTargetExists(username string) bool {
	if i.knownMentionUsers == nil {
		return true
	}

	localpart := strings.ToLower(username)
	if _, ok := i.knownMentionUsers[localpart]; ok {
		return true
	}
	if cached, ok := i.mentionExistsCache[localpart]; ok {
		return cached
	}

	exists, err := i.client.UserExists(localpart)
	if err != nil {
		logger.Warn("Could not verify mention target '%s' (%v); leaving it as plain text", username, err)
		exists = false
	} else if !exists {
		logger.Info("Mention target '%s' does not exist; leaving it as plain text", username)
	}
	i.mentionExistsCache[localpart] = exists
	return exists
}

// mentionLocalpart extracts "alice" from "@alice:example.com".
func mentionLocalpart(matrixUserID string) string {
	localpart := strings.TrimPrefix(matrixUserID, "@")
	if idx := strings.IndexByte(localpart, ':'); idx >= 0 {
		localpart = localpart[:idx]
	}
	return strings.ToLower(localpart)
}

// renderMentions normalizes plain @username mentions to full Matrix IDs and renders the
// message to HTML with mention pills. Returns the plain body, the formatted HTML body, and
// the deduped list of mentioned user IDs (for m.mentions).
func (i *Importer) renderMentions(message string) (body string, formattedHTML string, userIDs []string) {
	body = i.normalizeMatrixMentions(message)
	formattedHTML, userIDs = renderMatrixMessageHTML(body)
	return body, formattedHTML, userIDs
}

// (password generation lives in password.go)

func isNumericOnlyUsername(username string) bool {
	name := strings.TrimSpace(username)
	if name == "" {
		return false
	}
	for _, r := range name {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func isUserNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "m_not_found") && strings.Contains(msg, "user not found")
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

		// Synapse reserves all-numeric localparts for guest accounts.
		if isNumericOnlyUsername(user.Username) {
			logger.Warn("User '%s' skipped: numeric-only usernames are reserved for guests in Matrix", user.Username)
			stats.UsersSkipped++
			continue
		}

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

		// Display name from Mattermost, used both for accounts we create and for filling in
		// the gaps on accounts that already exist.
		displayName := strings.TrimSpace(user.FirstName + " " + user.LastName)
		if displayName == "" {
			displayName = user.Username
		}
		// Lower-cased once, here. Synapse canonicalises a pusher's pushkey but stores the
		// threepid exactly as the admin API was given it, so an address with capitals would
		// later fail the pusher's ownership check with THREEPID_NOT_FOUND and leave that
		// person without email notifications, for no visible reason.
		email := strings.ToLower(strings.TrimSpace(user.Email))

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
			// Anyone who signed in through SSO before the migration lands here, and would
			// otherwise keep an account with no email address - and so no email notifications.
			// Additive: nothing already on the account is overwritten.
			if err := i.client.EnsureUserProfile(mxID, displayName, email); err != nil {
				logger.Warn("Failed to complete profile for existing user '%s': %v", user.Username, err)
			}
			if email != "" {
				i.client.EnsureMASEmail(user.Username, email)
			}
			logger.Info("User '%s' already exists, skipped", user.Username)
			stats.UsersSkipped++
			continue
		}

		// Create the user (CreateUser is idempotent - if user exists, it will update)

		// An empty password means "do not set one" (PasswordModeNone, or PasswordModeLocalOnly
		// for a user who had SSO in Mattermost); the account is then reachable only via
		// SSO/MAS or an admin reset.
		password, err := i.passwordPolicy.GenerateFor(user.AuthService)
		if err != nil {
			logger.Error("Failed to generate password for user '%s': %v", user.Username, err)
			stats.UsersFailed++
			continue
		}

		req := &CreateUserRequest{
			Password:    password,
			DisplayName: displayName,
			Email:       email,
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

		if password != "" {
			i.generatedCredentials = append(i.generatedCredentials, UserCredential{
				Username:     user.Username,
				MatrixUserID: resp.UserID,
				Password:     password,
			})
		}

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

		spacePublic := false // default: invite-only (private)
		if opts != nil {
			switch opts.SpaceVisibility {
			case "public":
				spacePublic = true
			case "from_mattermost":
				spacePublic = team.IsOpen()
			}
		}

		// Create space
		resp, err := i.client.CreateSpace(team.DisplayName, team.Description, spacePublic, roomAlias, owner)
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

// resolveTokenToMattermostUser resolves a token (username/display/nickname) to a Mattermost user.
func resolveTokenToMattermostUser(token string, users []mattermost.User) *mattermost.User {
	tokenLower := strings.ToLower(token)
	normalized := strings.ToLower(strings.ReplaceAll(token, " ", "_"))

	for idx := range users {
		u := &users[idx]
		if u.Username == token || strings.ToLower(u.Username) == tokenLower || strings.ToLower(u.Username) == normalized {
			return u
		}
		display := strings.TrimSpace(u.FirstName + " " + u.LastName)
		if display != "" && (display == token || strings.EqualFold(display, token)) {
			return u
		}
		if u.Nickname != "" && (u.Nickname == token || strings.EqualFold(u.Nickname, token)) {
			return u
		}
	}
	return nil
}

// groupParticipantsAllLocked returns true when all resolvable participants in display_name are deleted/locked.
// If no participants can be resolved, it returns false to avoid accidental skips.
func groupParticipantsAllLocked(displayName string, users []mattermost.User) bool {
	parts := strings.Split(displayName, ",")
	if len(parts) == 0 {
		return false
	}

	seenUserIDs := make(map[string]struct{})
	resolvedCount := 0

	for _, part := range parts {
		token := strings.TrimSpace(part)
		if token == "" {
			continue
		}
		u := resolveTokenToMattermostUser(token, users)
		if u == nil {
			continue
		}
		if _, seen := seenUserIDs[u.ID]; seen {
			continue
		}
		seenUserIDs[u.ID] = struct{}{}
		resolvedCount++
		if !u.IsDeleted() {
			return false
		}
	}

	return resolvedCount > 0
}

// ImportChannelsAsRooms imports channels from Mattermost as Matrix rooms.
// teamByID maps Mattermost team ID to Team (for alias and name); can be nil if opts.PreserveOwnerAndAlias is false.
// users is used to build username->MatrixID map for group channel (type G) creator when creator_id is empty.
// When opts.PreserveOwnerAndAlias is true, each room gets alias teamname-channelname and owner from channel.CreatorID or fallback (or first member for group channels).
func (i *Importer) ImportChannelsAsRooms(channels []mattermost.Channel, existingMapping map[string]string, teamByID map[string]mattermost.Team, users []mattermost.User, userMapping map[string]string, opts *RoomImportOptions, progress ImportProgressCallback) (map[string]string, *ImportStats, error) {
	mapping := make(map[string]string)
	stats := &ImportStats{}
	total := len(channels)
	lockedUserByID := make(map[string]bool, len(users))
	for _, u := range users {
		lockedUserByID[u.ID] = u.IsDeleted()
	}

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

		// Skip group chats when all participants are deleted/locked in Mattermost.
		if channel.IsGroup() && groupParticipantsAllLocked(channel.DisplayName, users) {
			logger.Info("Room '%s' (group channel) skipped: all participants are locked/deleted", channel.DisplayName)
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
		ownerFromLockedCreator := false
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
				if channel.CreatorID != "" {
					if mappedCreator, ok := userMapping[channel.CreatorID]; ok && mappedCreator != "" && mappedCreator == owner && lockedUserByID[channel.CreatorID] {
						ownerFromLockedCreator = true
					}
				}
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
		if ownerFromLockedCreator && opts != nil {
			fallbackAdminID := opts.AdminUserID
			if opts.FallbackCreator != "" {
				fallbackAdminID = i.client.FormatUserID(opts.FallbackCreator)
			}
			if fallbackAdminID != "" && fallbackAdminID != owner {
				logger.Info("Room '%s': creator %q is locked/deactivated; adding fallback admin %s with power level 100", channel.DisplayName, channel.CreatorID, fallbackAdminID)
				// Ensure migration admin has PL 100 first; otherwise SetPowerLevels can fail with user_level (0) < send_level (100).
				if err := i.client.ensureAdminInRoomWithPower(resp.RoomID, owner, 100, fallbackAdminID); err != nil {
					logger.Warn("Room '%s': could not ensure admin has power level 100 before promoting fallback admin %s in room %s: %v", channel.DisplayName, fallbackAdminID, resp.RoomID, err)
				} else if err := i.client.AddUserToRoom(resp.RoomID, fallbackAdminID); err != nil {
					logger.Warn("Room '%s': could not add fallback admin %s to room %s: %v", channel.DisplayName, fallbackAdminID, resp.RoomID, err)
				} else if err := i.client.SetPowerLevels(resp.RoomID, fallbackAdminID, 100); err != nil {
					logger.Warn("Room '%s': could not set fallback admin %s power level 100 in room %s: %v", channel.DisplayName, fallbackAdminID, resp.RoomID, err)
				} else {
					logger.Info("Room '%s': added fallback admin %s with power level 100 (creator locked)", channel.DisplayName, fallbackAdminID)
				}
			}
		}
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
	defaultSpaceOwnerID string,
	progress ImportProgressCallback,
) (*ImportStats, []string, error) {
	stats := &ImportStats{}
	total := len(memberships)
	ensuredSpacesForForceJoin := make(map[string]struct{})
	unjoinableSpaces := make(map[string]struct{})

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

		// Ensure AS admin is in each space with PL100 once before applying force-join memberships.
		if i.client.ForceJoinEnabled() && i.client.HasASToken() {
			if _, blocked := unjoinableSpaces[spaceID]; blocked {
				logger.Warn("Team membership %d/%d skipped: space %s is marked unjoinable for admin", idx+1, total, spaceID)
				stats.MembersSkipped++
				continue
			}
			if _, already := ensuredSpacesForForceJoin[spaceID]; !already {
				if defaultSpaceOwnerID == "" {
					logger.Warn("Team membership %d/%d skipped: default space owner is empty, cannot ensure admin in space %s", idx+1, total, spaceID)
					unjoinableSpaces[spaceID] = struct{}{}
					stats.MembersSkipped++
					continue
				}
				if err := i.client.ensureAdminInSpaceWithPower(spaceID, defaultSpaceOwnerID, 100); err != nil {
					logger.Warn("Team membership %d/%d: could not ensure admin in space %s (owner %s): %v", idx+1, total, spaceID, defaultSpaceOwnerID, err)
					unjoinableSpaces[spaceID] = struct{}{}
					stats.MembersSkipped++
					continue
				}
				ensuredSpacesForForceJoin[spaceID] = struct{}{}
				logger.Info("ApplyTeamMemberships: admin joined space %s (PL 100) for force-join", spaceID)
			}
		}

		if i.client.ForceJoinEnabled() {
			logger.Info("Team membership %d/%d: force-joining %s to space %s", idx+1, total, userID, spaceID)
		} else {
			logger.Info("Team membership %d/%d: inviting %s to space %s", idx+1, total, userID, spaceID)
		}
		if err := i.client.AddUserToRoom(spaceID, userID); err != nil {
			if isUserNotFoundErr(err) {
				logger.Warn("Team membership %d/%d skipped: %s not found in Matrix (%v)", idx+1, total, userID, err)
				stats.MembersSkipped++
				continue
			}
			logger.Error("Team membership %d/%d failed: %s -> %s: %v", idx+1, total, userID, spaceID, err)
			stats.MembersFailed++
			continue
		}

		logger.Success("Team membership %d/%d: %s added to space", idx+1, total, userID)
		stats.MembersAdded++
	}

	logger.Info("Team membership import completed: added=%d, skipped=%d, failed=%d",
		stats.MembersAdded, stats.MembersSkipped, stats.MembersFailed)

	spacesToLeave := make([]string, 0, len(ensuredSpacesForForceJoin))
	for spaceID := range ensuredSpacesForForceJoin {
		spacesToLeave = append(spacesToLeave, spaceID)
	}
	return stats, spacesToLeave, nil
}

// ApplyChannelMemberships invites users to rooms based on channel memberships.
// For group channels (type G), after inviting all members we set equal power levels (50) for everyone to match Mattermost.
// Direct channels (type D) are skipped: both participants were already added during ImportDirectChannelsAsDMs (CreateDirectRoom).
// When force_join is enabled and the client has an AS token, for group (G) and private (P) rooms the admin is invited by the room owner,
// joins with power level 100, force-joins members, then leaves. defaultRoomOwnerID is used when channel has no creator_id (e.g. group channels).
// channels is used to identify group and direct channels; can be nil (then no equal power levels, and no DM skip).
func (i *Importer) ApplyChannelMemberships(
	channels []mattermost.Channel,
	users []mattermost.User,
	memberships []mattermost.ChannelMember,
	userMapping map[string]string,
	roomMapping map[string]string,
	defaultRoomOwnerID string,
	progress ImportProgressCallback,
) (*ImportStats, error) {
	stats := &ImportStats{}
	total := len(memberships)
	failedByType := map[string]int{"O": 0, "P": 0, "G": 0, "D": 0, "?": 0}
	skippedByType := map[string]int{"O": 0, "P": 0, "G": 0, "D": 0, "?": 0}
	bump := func(m map[string]int, channelType string) {
		if _, ok := m[channelType]; ok {
			m[channelType]++
			return
		}
		m["?"]++
	}

	// Channels referenced by memberships that never got a Matrix room. Collected with a
	// reason and reported once at the end: warning per membership says the same thing dozens
	// of times without ever saying why the room is missing.
	unmapped := make(map[string]*unmappedChannel)
	unmappedTotal := 0

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
	// Per channel, keep mapped Matrix users seen in membership data as extra inviter/recovery candidates.
	// This helps when creator/fallback owner cannot invite admin but another known member can.
	channelInviteFallbacks := make(map[string][]string)
	if i.client.ForceJoinEnabled() && i.client.HasASToken() {
		seenByChannel := make(map[string]map[string]struct{})
		for _, membership := range memberships {
			mappedUserID, ok := userMapping[membership.UserID]
			if !ok || mappedUserID == "" {
				continue
			}
			seen, ok := seenByChannel[membership.ChannelID]
			if !ok {
				seen = make(map[string]struct{})
				seenByChannel[membership.ChannelID] = seen
			}
			if _, exists := seen[mappedUserID]; exists {
				continue
			}
			seen[mappedUserID] = struct{}{}
			channelInviteFallbacks[membership.ChannelID] = append(channelInviteFallbacks[membership.ChannelID], mappedUserID)
		}
	}

	// Per group-channel room ID, collect Matrix user IDs that were added (for equal power levels later)
	groupRoomMembers := make(map[string][]string)

	// When force_join + AS: rooms we joined (via owner invite) so we can leave after processing all memberships
	roomsToLeaveAfter := make(map[string]struct{})
	ensuredRoomsForForceJoin := make(map[string]struct{})
	recoveryAttemptedRooms := make(map[string]struct{})
	unjoinableRooms := make(map[string]struct{})
	unjoinableRoomSkipCounts := make(map[string]int)
	adminUserID := ""
	if i.client.ForceJoinEnabled() && i.client.HasASToken() {
		if who, err := i.client.WhoAmI(); err == nil && who != nil && who.UserID != "" {
			adminUserID = who.UserID
		} else if err != nil {
			logger.Warn("ApplyChannelMemberships: could not resolve AS admin user ID via whoami: %v", err)
		}
	}

	logger.Info("Starting channel membership import: %d memberships to process (%d DM channels will be skipped)", total, len(directChannelIDs))

	for idx, membership := range memberships {
		if progress != nil {
			progress("channel_memberships", idx+1, total, "")
		}

		channel, channelKnown := channelByID[membership.ChannelID]
		channelType := "?"
		if channelKnown {
			switch {
			case channel.IsDirect():
				channelType = "D"
			case channel.IsGroup():
				channelType = "G"
			case channel.IsPrivate():
				channelType = "P"
			case channel.IsPublic():
				channelType = "O"
			}
		}

		if directChannelIDs[membership.ChannelID] {
			bump(skippedByType, channelType)
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
				entry, seen := unmapped[membership.ChannelID]
				if !seen {
					ch, known := channelByID[membership.ChannelID]
					entry = &unmappedChannel{
						name:   ch.DisplayName,
						reason: unmappedChannelReason(ch, known, users),
					}
					unmapped[membership.ChannelID] = entry
				}
				entry.count++
				unmappedTotal++
			}
			bump(skippedByType, channelType)
			stats.MembersSkipped++
			continue
		}

		// For non-D rooms with force_join: ensure admin is in room with PL100 once before force-join.
		if i.client.ForceJoinEnabled() && i.client.HasASToken() && !channel.IsDirect() {
			if _, blocked := unjoinableRooms[roomID]; blocked {
				unjoinableRoomSkipCounts[roomID]++
				bump(skippedByType, channelType)
				stats.MembersSkipped++
				continue
			}
			if _, already := ensuredRoomsForForceJoin[roomID]; !already {
				roomOwnerID := defaultRoomOwnerID
				creatorMappedUserID := ""
				roomInviteFallbackCandidates := channelInviteFallbacks[membership.ChannelID]
				if channel.CreatorID != "" {
					if mx, ok := userMapping[channel.CreatorID]; ok && mx != "" {
						roomOwnerID = mx
						creatorMappedUserID = mx
					}
				}
				if roomOwnerID == "" && adminUserID != "" {
					roomOwnerID = adminUserID
				}
				// If owner resolves to admin, prefer a non-admin known member as owner for invite/PL operations.
				if roomOwnerID == adminUserID {
					for _, candidate := range roomInviteFallbackCandidates {
						if candidate != "" && candidate != adminUserID {
							roomOwnerID = candidate
							break
						}
					}
				}
				if roomOwnerID != "" {
					const maxInviteCandidates = 12
					inviteCandidates := make([]string, 0, maxInviteCandidates)
					seenInviteCandidates := make(map[string]struct{}, maxInviteCandidates)
					addInviteCandidate := func(candidate string) {
						if len(inviteCandidates) >= maxInviteCandidates {
							return
						}
						if candidate == "" || candidate == roomOwnerID {
							return
						}
						if _, exists := seenInviteCandidates[candidate]; exists {
							return
						}
						seenInviteCandidates[candidate] = struct{}{}
						inviteCandidates = append(inviteCandidates, candidate)
					}
					addInviteCandidate(creatorMappedUserID)
					// If creator mapping is missing, current membership user is often already in room and can invite admin.
					addInviteCandidate(userID)
					addInviteCandidate(defaultRoomOwnerID)
					for _, candidate := range roomInviteFallbackCandidates {
						addInviteCandidate(candidate)
					}
					logger.Debug("ApplyChannelMemberships: ensuring admin in room %s (owner %s) for membership %s -> room", roomID, roomOwnerID, userID)
					roomEnsureErr := i.client.ensureAdminInRoomWithPower(roomID, roomOwnerID, 100, inviteCandidates...)
					if roomEnsureErr != nil {
						logger.Warn("ApplyChannelMemberships: could not ensure admin in room %s (owner %s): %v", roomID, roomOwnerID, roomEnsureErr)
						// For force-join membership import, admin presence in room is sufficient for Synapse admin join.
						// If we only failed to escalate admin PL, continue and avoid blacklisting the room.
						if isAdminPowerEscalationError(roomEnsureErr) {
							logger.Warn("ApplyChannelMemberships: proceeding without PL escalation for room %s; force-join can continue", roomID)
							roomEnsureErr = nil
							ensuredRoomsForForceJoin[roomID] = struct{}{}
							roomsToLeaveAfter[roomID] = struct{}{}
						}
						// One-time best-effort recovery for unjoinable rooms:
						// promote fallback owner and tag room name so it is easy to find.
						if roomEnsureErr != nil {
							if _, attempted := recoveryAttemptedRooms[roomID]; !attempted {
							recoveryAttemptedRooms[roomID] = struct{}{}
							// Prefer a non-admin recovery owner so invite-as-user has a chance to succeed.
							recoveryOwnerID := defaultRoomOwnerID
							if recoveryOwnerID == "" || recoveryOwnerID == adminUserID {
								recoveryOwnerID = creatorMappedUserID
							}
							if recoveryOwnerID == "" || recoveryOwnerID == adminUserID {
								recoveryOwnerID = userID
							}
							if recoveryOwnerID == "" || recoveryOwnerID == adminUserID {
								for _, candidate := range roomInviteFallbackCandidates {
									if candidate != "" && candidate != adminUserID {
										recoveryOwnerID = candidate
										break
									}
								}
							}
							if recoveryOwnerID == "" {
								recoveryOwnerID = adminUserID
							}
							recoverErr := i.client.TryRecoverUnjoinableRoom(roomID, channel.DisplayName, recoveryOwnerID)
							if recoverErr != nil {
								logger.Warn("ApplyChannelMemberships: recovery failed for room %s: %v", roomID, recoverErr)
							} else {
								logger.Warn("ApplyChannelMemberships: room %s recovered and renamed as unjoinable marker", roomID)
								roomEnsureErr = nil
								ensuredRoomsForForceJoin[roomID] = struct{}{}
								roomsToLeaveAfter[roomID] = struct{}{}
							}
						}
						}
						if roomEnsureErr != nil {
							unjoinableRooms[roomID] = struct{}{}
							unjoinableRoomSkipCounts[roomID] = 0
							logger.Warn("ApplyChannelMemberships: room %s marked unjoinable; remaining memberships for this room will be skipped", roomID)
							bump(skippedByType, channelType)
							stats.MembersSkipped++
							continue
						}
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

		if err := i.client.AddUserToRoom(roomID, userID); err != nil {
			if isUserNotFoundErr(err) {
				logger.Warn("Channel membership %d/%d skipped: %s not found in Matrix (%v)", idx+1, total, userID, err)
				bump(skippedByType, channelType)
				stats.MembersSkipped++
				continue
			}
			logger.Error("Channel membership %d/%d failed [type=%s]: %s -> %s: %v", idx+1, total, channelType, userID, roomID, err)
			bump(failedByType, channelType)
			stats.MembersFailed++
			continue
		}
		// Ensure admin cleanup covers all channels where force-join succeeded in this run.
		if i.client.ForceJoinEnabled() && i.client.HasASToken() && !channel.IsDirect() {
			roomsToLeaveAfter[roomID] = struct{}{}
		}
		if groupChannelIDs[membership.ChannelID] {
			groupRoomMembers[roomID] = append(groupRoomMembers[roomID], userID)
		}

		logger.Success("Channel membership %d/%d: %s added to room", idx+1, total, userID)
		stats.MembersAdded++
	}

	if len(unmapped) > 0 {
		ids := make([]string, 0, len(unmapped))
		for id := range unmapped {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		logger.Warn("ApplyChannelMemberships: %d membership(s) skipped across %d channel(s) with no Matrix room:", unmappedTotal, len(unmapped))
		for _, id := range ids {
			e := unmapped[id]
			logger.Warn("  channel %s %q: %s (%d membership(s))", id, e.name, e.reason, e.count)
		}
	}

	// For group channels, set equal power levels (50) for all members to match Mattermost
	for roomID, memberIDs := range groupRoomMembers {
		if len(memberIDs) == 0 {
			continue
		}
		uniqueMembers := make(map[string]struct{})
		userLevels := make(map[string]int)
		for _, u := range memberIDs {
			uniqueMembers[u] = struct{}{}
			userLevels[u] = 50
		}
		if err := i.client.SetPowerLevelsBulk(roomID, userLevels); err != nil {
			logger.Warn("ApplyChannelMemberships: could not set equal power levels for group room %s: %v", roomID, err)
		} else {
			logger.Info("ApplyChannelMemberships: set equal power level 50 for %d members in group room %s", len(memberIDs), roomID)
		}

		// Group channels with only one participant are typically not collaborative.
		// Add fallback owner for review with PL 100.
		if len(uniqueMembers) == 1 && defaultRoomOwnerID != "" {
			if _, already := uniqueMembers[defaultRoomOwnerID]; !already {
				logger.Warn("ApplyChannelMemberships: group room %s has a single participant; adding fallback owner %s as admin", roomID, defaultRoomOwnerID)
				if err := i.client.AddUserToRoom(roomID, defaultRoomOwnerID); err != nil {
					logger.Warn("ApplyChannelMemberships: could not add fallback owner %s to single-participant group room %s: %v", defaultRoomOwnerID, roomID, err)
				} else if err := i.client.SetPowerLevels(roomID, defaultRoomOwnerID, 100); err != nil {
					logger.Warn("ApplyChannelMemberships: could not set fallback owner %s power level 100 in room %s: %v", defaultRoomOwnerID, roomID, err)
				} else {
					logger.Info("ApplyChannelMemberships: added fallback owner %s with power level 100 to single-participant group room %s", defaultRoomOwnerID, roomID)
				}
			}
		}
	}

	// Leave rooms we joined for force-join so the admin is not left in group/private rooms.
	// This runs after group power-level updates so admin is still present to send PL state.
	leftChannels := 0
	failedLeaveChannels := 0
	for roomID := range roomsToLeaveAfter {
		logger.Debug("ApplyChannelMemberships: LeaveRoom %s (admin leaving after force-join)", roomID)
		if err := i.client.LeaveRoom(roomID); err != nil {
			logger.Warn("ApplyChannelMemberships: admin leave room %s after force-join: %v", roomID, err)
			failedLeaveChannels++
		} else {
			logger.Info("ApplyChannelMemberships: admin left room %s", roomID)
			leftChannels++
		}
	}
	logger.Info("ApplyChannelMemberships: admin cleanup completed: left_channels=%d leave_failures=%d attempted=%d",
		leftChannels, failedLeaveChannels, len(roomsToLeaveAfter))

	logger.Info("Channel membership import completed: added=%d, skipped=%d, failed=%d",
		stats.MembersAdded, stats.MembersSkipped, stats.MembersFailed)
	logger.Info("Channel membership type summary: failed(O=%d,P=%d,G=%d,D=%d,?=%d) skipped(O=%d,P=%d,G=%d,D=%d,?=%d)",
		failedByType["O"], failedByType["P"], failedByType["G"], failedByType["D"], failedByType["?"],
		skippedByType["O"], skippedByType["P"], skippedByType["G"], skippedByType["D"], skippedByType["?"])
	totalUnjoinableSkips := 0
	for _, count := range unjoinableRoomSkipCounts {
		totalUnjoinableSkips += count
	}
	if totalUnjoinableSkips > 0 {
		logger.Info("Channel membership unjoinable summary: rooms=%d memberships_skipped=%d", len(unjoinableRoomSkipCounts), totalUnjoinableSkips)
		for roomID, count := range unjoinableRoomSkipCounts {
			if count > 0 {
				logger.Info("Channel membership unjoinable room: room=%s skipped_memberships=%d", roomID, count)
			}
		}
	}

	return stats, nil
}

// unmappedChannel records a channel that memberships referenced but that has no Matrix room.
type unmappedChannel struct {
	name   string
	reason string
	count  int
}

// unmappedChannelReason explains why a channel has no room, mirroring the conditions under
// which room import skips one. "Not in mapping" on its own sends the reader to the wrong
// place: most of these are deliberate, and the one that is not looks exactly the same.
func unmappedChannelReason(ch mattermost.Channel, known bool, users []mattermost.User) string {
	switch {
	case !known:
		return "not present in the exported assets - re-run export assets"
	case ch.IsDeleted():
		return "deleted in Mattermost, so no room was created"
	case ch.IsDirect():
		return "direct channel - imported separately, and only when matrix.import.import_direct_messages is set"
	case ch.IsGroup() && groupParticipantsAllLocked(ch.DisplayName, users):
		return "group conversation whose participants are all locked or deleted in Mattermost"
	default:
		return "room creation did not succeed - look for this channel in the import assets log"
	}
}

// skipTally counts why items were skipped, in first-seen order, so a run can report the shape
// of what it left behind rather than only how much. A bare "skipped=4" gives an operator
// nothing to act on; "user-not-mapped=1" points straight at a person who lost every DM.
type skipTally struct {
	order  []string
	counts map[string]int
}

func (t *skipTally) add(reason string) {
	if t.counts == nil {
		t.counts = make(map[string]int)
	}
	if _, seen := t.counts[reason]; !seen {
		t.order = append(t.order, reason)
	}
	t.counts[reason]++
}

// String renders the tally as "reason=n, reason=n", or "" when nothing was skipped.
func (t *skipTally) String() string {
	if len(t.order) == 0 {
		return ""
	}
	parts := make([]string, 0, len(t.order))
	for _, reason := range t.order {
		parts = append(parts, fmt.Sprintf("%s=%d", reason, t.counts[reason]))
	}
	return strings.Join(parts, ", ")
}

// ImportDirectChannelsAsDMs imports Mattermost direct message channels (type D) as Matrix DMs.
// Room creator is preferred as a real user (not the AS); is_direct and m.direct are set so both users see the room under "People".
// When channel has no creator_id, name format is "senderID_receiverID" (first = sender, second = receiver); sender is used as room creator.
func (i *Importer) ImportDirectChannelsAsDMs(
	directChannels []mattermost.Channel,
	users []mattermost.User,
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

	lockedUserByID := make(map[string]bool, len(users))
	for _, u := range users {
		lockedUserByID[u.ID] = u.IsDeleted()
	}

	logger.Info("ImportDirectChannelsAsDMs: processing %d direct channels", total)

	// Skipping a DM means a conversation does not arrive. Reasons that are deliberate stay at
	// info; reasons that mean lost content are warnings, so they are visible without turning
	// on debug logging after the fact.
	skips := &skipTally{}

	for idx, channel := range directChannels {
		if progress != nil {
			progress("dm_rooms", idx+1, total, channel.ID)
		}

		if channel.IsDeleted() {
			logger.Debug("DM channel %s deleted in Mattermost, skipping", channel.ID)
			skips.add("deleted")
			stats.RoomsSkipped++
			continue
		}

		if _, exists := existingMapping[channel.ID]; exists {
			logger.Debug("DM channel %s already imported, skipping", channel.ID)
			skips.add("already-imported")
			stats.RoomsSkipped++
			continue
		}

		// Resolve the two Matrix user IDs for this DM
		senderID, receiverID, err := channel.DMParticipantIDs()
		if err != nil {
			logger.Warn("DM channel %s skipped: cannot determine participants from name %q (%v) - this conversation is not migrated",
				channel.ID, channel.Name, err)
			skips.add("unparseable-name")
			stats.RoomsSkipped++
			continue
		}
		if lockedUserByID[senderID] && lockedUserByID[receiverID] {
			logger.Info("DM channel %s skipped: both participants are locked/deleted in Mattermost (%s, %s)", channel.ID, senderID, receiverID)
			skips.add("both-participants-locked")
			stats.RoomsSkipped++
			continue
		}
		creatorMX, ok1 := userMapping[senderID]
		otherMX, ok2 := userMapping[receiverID]
		if !ok1 || !ok2 {
			// The missing user has no Matrix account, so *every* DM of theirs is skipped, not
			// just this one. Usually ignored_users, or a user whose creation failed earlier.
			missing := senderID
			if ok1 {
				missing = receiverID
			}
			logger.Warn("DM channel %s skipped: participant %s has no Matrix user - all of their direct conversations are affected",
				channel.ID, missing)
			skips.add("user-not-mapped")
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
			logger.Warn("DM channel %s skipped: could not resolve both users (creator=%q other=%q) - this conversation is not migrated",
				channel.ID, creatorMX, otherMX)
			skips.add("unresolved-users")
			stats.RoomsSkipped++
			continue
		}
		// Both sides equal is a self-DM (Mattermost notes to self), not an error: it gets
		// its own room with the user as sole member.
		selfDM := creatorMX == otherMX

		resp, err := i.client.CreateDirectRoom(creatorMX, otherMX)
		if err != nil {
			if selfDM {
				logger.Error("ImportDirectChannelsAsDMs: failed to create self-DM for channel %s (%s): %v", channel.ID, creatorMX, err)
			} else {
				logger.Error("ImportDirectChannelsAsDMs: failed to create DM for channel %s (%s <-> %s): %v", channel.ID, creatorMX, otherMX, err)
			}
			stats.RoomsFailed++
			continue
		}

		if selfDM {
			logger.Success("ImportDirectChannelsAsDMs: created self-DM %s for channel %s (%s)", resp.RoomID, channel.ID, creatorMX)
		} else {
			logger.Success("ImportDirectChannelsAsDMs: created DM %s for channel %s (%s <-> %s)", resp.RoomID, channel.ID, creatorMX, otherMX)
		}
		mapping[channel.ID] = resp.RoomID
		stats.RoomsCreated++
	}

	if breakdown := skips.String(); breakdown != "" {
		logger.Info("ImportDirectChannelsAsDMs completed: created=%d, skipped=%d (%s), failed=%d",
			stats.RoomsCreated, stats.RoomsSkipped, breakdown, stats.RoomsFailed)
	} else {
		logger.Info("ImportDirectChannelsAsDMs completed: created=%d, skipped=%d, failed=%d",
			stats.RoomsCreated, stats.RoomsSkipped, stats.RoomsFailed)
	}

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

// SetMessageCheckpoint installs fn, called every `every` imported messages with the
// post ID -> event ID mapping accumulated so far. Passing a nil fn or every <= 0 disables
// checkpointing. The mapping handed to fn is a copy, safe to persist without racing the import.
func (i *Importer) SetMessageCheckpoint(every int, fn func(map[string]string)) {
	i.checkpointEvery = every
	i.checkpointFn = fn
}

// maybeCheckpoint hands the current mapping to the checkpoint callback every checkpointEvery
// messages, so an interrupted import can resume from what it already sent.
func (i *Importer) maybeCheckpoint(mapping map[string]string) {
	if i.checkpointFn == nil || i.checkpointEvery <= 0 {
		return
	}
	if len(mapping)%i.checkpointEvery != 0 {
		return
	}
	snapshot := make(map[string]string, len(mapping))
	for k, v := range mapping {
		snapshot[k] = v
	}
	i.checkpointFn(snapshot)
}

// SetReactionCheckpoint installs fn, called every `every` sent reactions with the
// reaction key -> event ID mapping accumulated so far. Same contract as SetMessageCheckpoint:
// nil fn or every <= 0 disables it, and the mapping handed to fn is a copy.
func (i *Importer) SetReactionCheckpoint(every int, fn func(map[string]string)) {
	i.reactionCheckpointEvery = every
	i.reactionCheckpointFn = fn
}

// maybeReactionCheckpoint hands the reaction mapping to the checkpoint callback every
// reactionCheckpointEvery sent reactions.
func (i *Importer) maybeReactionCheckpoint(mapping map[string]string) {
	if i.reactionCheckpointFn == nil || i.reactionCheckpointEvery <= 0 {
		return
	}
	if len(mapping)%i.reactionCheckpointEvery != 0 {
		return
	}
	snapshot := make(map[string]string, len(mapping))
	for k, v := range mapping {
		snapshot[k] = v
	}
	i.reactionCheckpointFn(snapshot)
}

// ExistingMappings holds existing mappings to skip already imported items
type ExistingMappings struct {
	Users  map[string]string
	Spaces map[string]string
	Rooms  map[string]string
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
	FilesTooLarge    int `json:"files_too_large"` // Files rejected for exceeding max_upload_size_mb

	ReactionsImported    int `json:"reactions_imported"`
	ReactionsSkipped     int `json:"reactions_skipped"`      // Already imported, or no reachable target
	ReactionsFailed      int `json:"reactions_failed"`       // Send failed for an unexpected reason
	ReactionsCustomEmoji int `json:"reactions_custom_emoji"` // Imported as literal :name: text
}

// FileConfig holds file migration settings
type FileConfig struct {
	Mode                 string                            // "link", "upload", or "skip"
	S3PublicURL          string                            // Base URL for S3 files
	LocalDataPath        string                            // Mattermost data path base (local or remote)
	RemoteReadFile       func(path string) ([]byte, error) // Optional remote file reader over SSH
	UploadFallbackToLink bool                              // If true, upload failures may fall back to S3/public links
	MaxUploadSize        int64                             // Max file size for upload
}

func buildPublicFileURL(baseURL, filePath string) string {
	base := strings.TrimSuffix(baseURL, "/")
	rel := strings.TrimPrefix(filePath, "/")
	return base + "/" + rel
}

func resolveLocalMattermostPath(localDataPath, mattermostFilePath string) string {
	relPath := filepath.FromSlash(strings.TrimPrefix(mattermostFilePath, "/"))
	return filepath.Join(localDataPath, relPath)
}

func resolveRemoteMattermostPath(remoteDataPath, mattermostFilePath string) string {
	relPath := strings.TrimPrefix(mattermostFilePath, "/")
	return path.Join(remoteDataPath, relPath)
}

func (i *Importer) sendLinkOrSkip(
	result *ImportMessagesResult,
	roomID string,
	file mattermost.FileInfo,
	fileConfig *FileConfig,
	timestamp int64,
	senderID string,
	threadRootEventID string,
	threadLatestEventID string,
	reason string,
) {
	if fileConfig.S3PublicURL == "" {
		result.Stats.FilesSkipped++
		result.Errors = append(result.Errors, fmt.Sprintf("Skipped file %s for post %s: %s", file.Name, file.PostID, reason))
		return
	}
	fileURL := buildPublicFileURL(fileConfig.S3PublicURL, file.Path)
	var err error
	if threadRootEventID != "" {
		_, err = i.client.SendFileLinkAsReply(roomID, file.Name, fileURL, file.MimeType, file.Size, threadRootEventID, threadLatestEventID, timestamp, senderID)
	} else {
		_, err = i.client.SendFileLink(roomID, file.Name, fileURL, file.MimeType, file.Size, timestamp, senderID)
	}
	if err != nil {
		result.Stats.FilesSkipped++
		result.Errors = append(result.Errors, fmt.Sprintf("Failed to send file link for %s (post %s): %v", file.Name, file.PostID, err))
		return
	}
	result.Stats.FilesLinked++
}

func (i *Importer) importPostFiles(
	result *ImportMessagesResult,
	roomID string,
	files []mattermost.FileInfo,
	fileConfig *FileConfig,
	timestamp int64,
	senderID string,
	threadRootEventID string,
	threadLatestEventID string,
) (int, int64) {
	tooLargeCount := 0
	var maxTooLargeSize int64
	logger.Debug("importPostFiles: room=%s sender=%s files=%d mode=%s", roomID, senderID, len(files), fileConfig.Mode)
	for _, file := range files {
		if file.IsDeleted() {
			logger.Debug("importPostFiles: skipping deleted file id=%s name=%s post=%s", file.ID, file.Name, file.PostID)
			result.Stats.FilesSkipped++
			continue
		}
		if file.Size > fileConfig.MaxUploadSize {
			logger.Debug("importPostFiles: file too large id=%s name=%s size=%d max=%d", file.ID, file.Name, file.Size, fileConfig.MaxUploadSize)
			result.Stats.FilesTooLarge++
			tooLargeCount++
			if file.Size > maxTooLargeSize {
				maxTooLargeSize = file.Size
			}
			if fileConfig.UploadFallbackToLink {
				i.sendLinkOrSkip(result, roomID, file, fileConfig, timestamp, senderID, threadRootEventID, threadLatestEventID, "file exceeds max_upload_size_mb")
			} else {
				result.Stats.FilesSkipped++
				result.Errors = append(result.Errors, fmt.Sprintf("Skipped file %s for post %s: file size %d exceeds max_upload_size_mb (%d bytes)", file.Name, file.PostID, file.Size, fileConfig.MaxUploadSize))
			}
			continue
		}
		if fileConfig.LocalDataPath == "" {
			logger.Debug("importPostFiles: local_data_path empty for file id=%s name=%s", file.ID, file.Name)
			if fileConfig.UploadFallbackToLink {
				i.sendLinkOrSkip(result, roomID, file, fileConfig, timestamp, senderID, threadRootEventID, threadLatestEventID, "mattermost.files.local_data_path is empty")
			} else {
				result.Stats.FilesSkipped++
				result.Errors = append(result.Errors, fmt.Sprintf("Skipped file %s for post %s: mattermost.files.local_data_path is empty", file.Name, file.PostID))
			}
			continue
		}
		localPath := resolveLocalMattermostPath(fileConfig.LocalDataPath, file.Path)
		logger.Debug("importPostFiles: reading local file id=%s name=%s path=%s", file.ID, file.Name, localPath)
		data, err := os.ReadFile(localPath)
		if err != nil && fileConfig.RemoteReadFile != nil {
			remotePath := resolveRemoteMattermostPath(fileConfig.LocalDataPath, file.Path)
			logger.Debug("importPostFiles: local read failed, trying remote read id=%s name=%s path=%s err=%v", file.ID, file.Name, remotePath, err)
			data, err = fileConfig.RemoteReadFile(remotePath)
		}
		if err != nil {
			logger.Debug("importPostFiles: read failed id=%s name=%s err=%v", file.ID, file.Name, err)
			if fileConfig.UploadFallbackToLink {
				i.sendLinkOrSkip(result, roomID, file, fileConfig, timestamp, senderID, threadRootEventID, threadLatestEventID, fmt.Sprintf("cannot read attachment bytes from %s: %v", file.Path, err))
			} else {
				result.Stats.FilesSkipped++
				result.Errors = append(result.Errors, fmt.Sprintf("Skipped file %s for post %s: cannot read attachment bytes from %s: %v", file.Name, file.PostID, file.Path, err))
			}
			continue
		}
		mimeType := strings.TrimSpace(file.MimeType)
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		logger.Debug("importPostFiles: uploading file id=%s name=%s size=%d mime=%s", file.ID, file.Name, file.Size, mimeType)
		uploadResp, err := i.client.UploadMedia(data, file.Name, mimeType)
		if err != nil {
			logger.Debug("importPostFiles: upload failed id=%s name=%s err=%v", file.ID, file.Name, err)
			if fileConfig.UploadFallbackToLink {
				i.sendLinkOrSkip(result, roomID, file, fileConfig, timestamp, senderID, threadRootEventID, threadLatestEventID, fmt.Sprintf("upload failed: %v", err))
			} else {
				result.Stats.FilesSkipped++
				result.Errors = append(result.Errors, fmt.Sprintf("Skipped file %s for post %s: upload failed: %v", file.Name, file.PostID, err))
			}
			continue
		}
		logger.Debug("importPostFiles: upload success id=%s name=%s mxc=%s", file.ID, file.Name, uploadResp.ContentURI)
		if threadRootEventID != "" {
			_, err = i.client.SendUploadedFileAsReply(roomID, uploadResp.ContentURI, file.Name, mimeType, file.Size, file.Width, file.Height, threadRootEventID, threadLatestEventID, timestamp, senderID)
		} else {
			_, err = i.client.SendUploadedFile(roomID, uploadResp.ContentURI, file.Name, mimeType, file.Size, file.Width, file.Height, timestamp, senderID)
		}
		if err != nil {
			logger.Debug("importPostFiles: send uploaded file failed id=%s name=%s err=%v", file.ID, file.Name, err)
			if fileConfig.UploadFallbackToLink {
				i.sendLinkOrSkip(result, roomID, file, fileConfig, timestamp, senderID, threadRootEventID, threadLatestEventID, fmt.Sprintf("send uploaded file failed: %v", err))
			} else {
				result.Stats.FilesSkipped++
				result.Errors = append(result.Errors, fmt.Sprintf("Skipped file %s for post %s: send uploaded file failed: %v", file.Name, file.PostID, err))
			}
			continue
		}
		logger.Debug("importPostFiles: sent uploaded file event id=%s name=%s room=%s", file.ID, file.Name, roomID)
		result.Stats.FilesUploaded++
	}
	return tooLargeCount, maxTooLargeSize
}

// MessageImportCallback is called for each message imported
type MessageImportCallback func(current, total int, channelName string, status string)

// ImportMessagesResult contains the result of message import
type ImportMessagesResult struct {
	Stats   *MessageImportStats
	Mapping map[string]string // MattermostID -> MatrixEventID
	Errors  []string
	// ReactionMapping records the reactions sent by this run, keyed by mattermost.Reaction.Key().
	// Mattermost reactions have no ID of their own, so this is what makes a re-run idempotent.
	ReactionMapping map[string]string
}

// ImportMessages imports messages from Mattermost posts to Matrix rooms
// This requires Application Service token for timestamp support
func (i *Importer) ImportMessages(
	posts []mattermost.Post,
	channelToRoom map[string]string, // Mattermost channel ID -> Matrix room ID
	userMapping map[string]string, // Mattermost user ID -> Matrix user ID
	existingMapping map[string]string, // Mattermost post ID -> Matrix event ID (for resume)
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

	// Gate @name rewriting on users that actually exist.
	i.SetKnownMentionUsers(userMapping)

	// Past authors who have since left their channel are not room members, and the AS cannot
	// send as a non-member. Same reasoning as the with-files path.
	i.historyJoins = append(i.historyJoins, i.ensureHistoryAuthorsJoined(posts, channelToRoom, userMapping)...)

	// Collect all existing mappings
	for k, v := range existingMapping {
		result.Mapping[k] = v
	}

	// Sort posts by timestamp (they should already be sorted, but just in case)
	// This ensures parent messages are imported before replies

	// Newest Matrix event per Mattermost thread root, so a thread's compatibility reply
	// points at the previous message rather than always at the root. Posts arrive in
	// chronological order, so this needs no sorting.
	threadLatest := make(map[string]string)

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

		// Skip Mattermost system messages (joins/leaves, header/purpose changes, etc.);
		// they carry no user content and only pollute the room.
		if post.IsSystemMessage() {
			result.Stats.MessagesSkipped++
			if progress != nil {
				progress(idx+1, total, post.ChannelID, "skipped:system")
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

		messageContent := i.normalizeMatrixMentions(post.Message)

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
				// Send into the thread rooted at the parent post
				resp, sendErr := i.client.SendReplyWithTimestamp(roomID, messageContent, parentEventID, threadLatest[post.RootID], post.CreateAt, senderID)
				if sendErr != nil {
					result.Stats.RepliesFailed++
					result.Errors = append(result.Errors, fmt.Sprintf("Failed to send reply %s: %v", post.ID, sendErr))
					if progress != nil {
						progress(idx+1, total, post.ChannelID, "failed:reply_error")
					}
					continue
				}
				eventID = resp.EventID
				threadLatest[post.RootID] = eventID
				result.Stats.RepliesImported++
			}
		} else {
			// Regular message
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
		i.maybeCheckpoint(result.Mapping)

		if progress != nil {
			progress(idx+1, total, post.ChannelID, "imported")
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
	reactionImport *ReactionImport,
	progress MessageImportCallback,
) (*ImportMessagesResult, error) {
	result := &ImportMessagesResult{
		Stats:           &MessageImportStats{},
		Mapping:         make(map[string]string),
		Errors:          []string{},
		ReactionMapping: make(map[string]string),
	}

	if !i.client.HasASToken() {
		logger.Warn("No Application Service token configured - messages will be imported without original timestamps")
	}

	total := len(posts)
	logger.Info("Starting message import with files: %d posts to process", total)

	// Gate @name rewriting on users that actually exist.
	i.SetKnownMentionUsers(userMapping)

	// Default file config
	if fileConfig == nil {
		fileConfig = &FileConfig{Mode: "skip"}
	}
	if fileConfig.MaxUploadSize <= 0 {
		fileConfig.MaxUploadSize = 50 * 1024 * 1024
	}
	logger.Info(
		"Message file handling config: mode=%s fallback_to_link=%v local_data_path_set=%v remote_reader_set=%v s3_url_set=%v max_upload_size_bytes=%d",
		fileConfig.Mode,
		fileConfig.UploadFallbackToLink,
		fileConfig.LocalDataPath != "",
		fileConfig.RemoteReadFile != nil,
		fileConfig.S3PublicURL != "",
		fileConfig.MaxUploadSize,
	)

	// Collect all existing mappings
	for k, v := range existingMapping {
		result.Mapping[k] = v
	}

	// Newest Matrix event per Mattermost thread root, so a thread's compatibility reply
	// points at the previous message rather than always at the root. Posts arrive in
	// chronological order, so this needs no sorting.
	threadLatest := make(map[string]string)

	// Anyone who posted in a channel and later left it is absent from the room, and the AS
	// cannot send as a non-member. Join them before the loop rather than discovering it one
	// M_FORBIDDEN at a time.
	i.historyJoins = append(i.historyJoins, i.ensureHistoryAuthorsJoined(posts, channelToRoom, userMapping)...)

	// Index every post's target room up front. The reaction pass needs the room of a post that
	// the loop below may skip as already imported, and those skips `continue` before the room
	// is ever resolved.
	roomByPost := make(map[string]string, len(posts))
	for _, post := range posts {
		if roomID, ok := channelToRoom[post.ChannelID]; ok {
			roomByPost[post.ID] = roomID
		}
	}

	// Process messages in order
	totalTooLarge := 0
	var largestTooLargeSize int64
	for idx, post := range posts {
		// Check if already imported
		if _, exists := existingMapping[post.ID]; exists {
			result.Stats.MessagesSkipped++
			if progress != nil {
				progress(idx+1, total, post.ChannelID, "skipped")
			}
			continue
		}

		// Skip Mattermost system messages (joins/leaves, header/purpose changes, etc.);
		// they carry no user content and only pollute the room.
		if post.IsSystemMessage() {
			result.Stats.MessagesSkipped++
			if progress != nil {
				progress(idx+1, total, post.ChannelID, "skipped:system")
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
		messageContent := i.normalizeMatrixMentions(post.Message)
		files := filesByPost[post.ID]

		// Append file links if mode is "link"
		if fileConfig.Mode == "link" && len(files) > 0 && fileConfig.S3PublicURL != "" {
			for _, file := range files {
				fileURL := buildPublicFileURL(fileConfig.S3PublicURL, file.Path)
				messageContent += fmt.Sprintf("\n\n📎 [%s](%s)", file.Name, fileURL)
				result.Stats.FilesLinked++
			}
		}

		// Handle reply
		var eventID string
		attachmentReplyToEventID := ""

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
				resp, sendErr := i.client.SendReplyWithTimestamp(roomID, messageContent, parentEventID, threadLatest[post.RootID], post.CreateAt, senderID)
				if sendErr != nil {
					result.Stats.RepliesFailed++
					result.Errors = append(result.Errors, fmt.Sprintf("Failed to send reply %s: %v", post.ID, sendErr))
					if progress != nil {
						progress(idx+1, total, post.ChannelID, "failed:reply_error")
					}
					continue
				}
				eventID = resp.EventID
				threadLatest[post.RootID] = eventID
				result.Stats.RepliesImported++
				attachmentReplyToEventID = parentEventID
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
		i.maybeCheckpoint(result.Mapping)

		if progress != nil {
			progress(idx+1, total, post.ChannelID, "imported")
		}

		// Upload or link files after the message event is imported.
		// Matrix file attachments are sent as separate m.room.message events.
		if fileConfig.Mode == "upload" && len(files) > 0 {
			tooLargeCount, maxTooLargeSize := i.importPostFiles(result, roomID, files, fileConfig, post.CreateAt, senderID, attachmentReplyToEventID, threadLatest[post.RootID])
			totalTooLarge += tooLargeCount
			if maxTooLargeSize > largestTooLargeSize {
				largestTooLargeSize = maxTooLargeSize
			}
		}

	}

	// Reactions come last: an annotation can only point at an event that already exists.
	if reactionImport != nil && len(reactionImport.Reactions) > 0 {
		i.importReactions(result, reactionImport.Reactions, roomByPost, userMapping,
			reactionImport.AlreadyImported, progress)
	}

	logger.Info("Message import completed: imported=%d, skipped=%d, failed=%d, replies=%d, files_uploaded=%d, files_linked=%d",
		result.Stats.MessagesImported, result.Stats.MessagesSkipped,
		result.Stats.MessagesFailed, result.Stats.RepliesImported, result.Stats.FilesUploaded, result.Stats.FilesLinked)
	if fileConfig.Mode == "upload" && totalTooLarge > 0 {
		logger.Warn("Upload mode: %d files exceeded max_upload_size_mb (%d bytes). Largest rejected file size: %d bytes",
			totalTooLarge, fileConfig.MaxUploadSize, largestTooLargeSize)
	}

	return result, nil
}

// ReactionProgressStage is passed in the channel slot of MessageImportCallback while the
// reaction pass runs, so a front end can label the progress and restart its rate estimate
// instead of reporting reactions as messages.
const ReactionProgressStage = "reactions"

// ReactionImport carries everything the reaction pass needs. A nil value turns reactions off.
type ReactionImport struct {
	Reactions []mattermost.Reaction
	// AlreadyImported holds the reaction keys sent by earlier runs, so a resumed import does
	// not annotate the same event twice.
	AlreadyImported map[string]string
}

// reactionSkipReason reports why a reaction cannot be sent, or "" when it can.
//
// Kept free of I/O so every branch is testable: a reaction silently vanishing is the failure
// mode that matters here, and each of these reasons has a different remedy.
func reactionSkipReason(targetEventID, roomID, senderID, emojiKey string) string {
	switch {
	case targetEventID == "":
		// The post never reached Matrix: a system message, deleted, or a failed send.
		return "target message not imported"
	case roomID == "":
		return "no room mapping"
	case senderID == "":
		return "user not mapped"
	case emojiKey == "":
		return "empty emoji name"
	}
	return ""
}

// importReactions annotates already-imported messages with the reactions they carried in
// Mattermost. It runs after the message pass because an annotation needs the event ID of its
// target, which only exists once that message has been sent.
//
// Reactions from people who have since left the channel are skipped rather than force-joined:
// Synapse refuses an event from a non-member even through the Application Service, and
// re-adding them would put someone back into a room they deliberately left. The tally names
// that case explicitly so the loss is visible rather than silent.
func (i *Importer) importReactions(
	result *ImportMessagesResult,
	reactions []mattermost.Reaction,
	roomByPost map[string]string,
	userMapping map[string]string,
	alreadyImported map[string]string,
	progress MessageImportCallback,
) {
	total := len(reactions)
	logger.Info("Starting reaction import: %d reactions to process", total)

	tally := &skipTally{}

	for idx, reaction := range reactions {
		key := reaction.Key()

		if _, done := alreadyImported[key]; done {
			result.Stats.ReactionsSkipped++
			tally.add("already imported")
			if progress != nil {
				progress(idx+1, total, ReactionProgressStage, "skipped")
			}
			continue
		}
		if _, done := result.ReactionMapping[key]; done {
			// Duplicate row for the same (post, user, emoji). Should not happen given the
			// primary key, but a hand-edited dump could contain one.
			result.Stats.ReactionsSkipped++
			tally.add("duplicate in export")
			continue
		}

		targetEventID := result.Mapping[reaction.PostID]
		roomID := roomByPost[reaction.PostID]
		senderID := userMapping[reaction.UserID]
		emojiKey, custom := ReactionKey(reaction.EmojiName)

		if reason := reactionSkipReason(targetEventID, roomID, senderID, emojiKey); reason != "" {
			result.Stats.ReactionsSkipped++
			tally.add(reason)
			if progress != nil {
				progress(idx+1, total, ReactionProgressStage, "skipped")
			}
			continue
		}

		resp, err := i.client.SendReactionWithTimestamp(roomID, targetEventID, emojiKey, reaction.CreateAt, senderID)
		if err != nil {
			if isNotInRoomError(err) {
				result.Stats.ReactionsSkipped++
				tally.add("sender left the channel")
				if progress != nil {
					progress(idx+1, total, ReactionProgressStage, "skipped")
				}
				continue
			}
			result.Stats.ReactionsFailed++
			result.Errors = append(result.Errors,
				fmt.Sprintf("Failed to send reaction %s on post %s: %v", reaction.EmojiName, reaction.PostID, err))
			if progress != nil {
				progress(idx+1, total, ReactionProgressStage, "failed")
			}
			continue
		}

		result.ReactionMapping[key] = resp.EventID
		result.Stats.ReactionsImported++
		if custom {
			result.Stats.ReactionsCustomEmoji++
		}
		i.maybeReactionCheckpoint(result.ReactionMapping)

		if progress != nil {
			progress(idx+1, total, ReactionProgressStage, "imported")
		}
	}

	logger.Info("Reaction import completed: imported=%d, skipped=%d, failed=%d, custom_emoji=%d",
		result.Stats.ReactionsImported, result.Stats.ReactionsSkipped,
		result.Stats.ReactionsFailed, result.Stats.ReactionsCustomEmoji)
	if summary := tally.String(); summary != "" {
		logger.Info("Reactions skipped by reason: %s", summary)
	}
	if result.Stats.ReactionsCustomEmoji > 0 {
		logger.Info("%d reaction(s) used a custom Mattermost emoji and were imported as literal :name: text",
			result.Stats.ReactionsCustomEmoji)
	}
}

// emailNotificationsDisabledHint recognises the answer Synapse gives when the server has no
// email configuration at all. PusherFactory only registers the "email" pusher type when
// email.enable_notifs is true, so without it every single call fails the same way — which
// is a server-side omission, not hundreds of broken accounts.
func emailNotificationsDisabledHint(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unknown pusher type") || strings.Contains(msg, "pusher type")
}

// EnableEmailNotifications registers an email pusher for every mapped user who has an address,
// so people are told by email about what they missed without having to find the setting first.
//
// Must run after the messages are imported: a new pusher starts from the current stream
// position, so anything imported afterwards counts as unread and would be mailed out.
func (i *Importer) EnableEmailNotifications(
	users []mattermost.User,
	userMapping map[string]string,
	progress ImportProgressCallback,
) (*ImportStats, error) {
	stats := &ImportStats{}
	total := len(users)
	tally := &skipTally{}

	logger.Info("Enabling email notifications for %d users", total)

	if !i.client.HasASToken() {
		return nil, fmt.Errorf("email notifications need the Application Service token: a pusher is registered as the user, which requires ?user_id=")
	}

	serverSideMissing := false

	for idx, user := range users {
		if progress != nil {
			progress("enable_notifications", idx+1, total, user.Username)
		}

		email := strings.ToLower(strings.TrimSpace(user.Email))
		matrixUserID, mapped := userMapping[user.ID]

		switch {
		case email == "":
			stats.UsersSkipped++
			tally.add("no email address")
			continue
		case !mapped:
			stats.UsersSkipped++
			tally.add("user not mapped")
			continue
		case user.IsDeleted():
			// Deactivated accounts cannot read the mail anyway.
			stats.UsersSkipped++
			tally.add("account deactivated")
			continue
		}

		if err := i.client.SetEmailPusher(matrixUserID, email); err != nil {
			if emailNotificationsDisabledHint(err) {
				serverSideMissing = true
				stats.UsersFailed++
				tally.add("email notifications not enabled on the server")
				continue
			}
			if strings.Contains(strings.ToUpper(err.Error()), "THREEPID_NOT_FOUND") {
				// The address is not on the account, so Synapse refuses to let the user be
				// notified at it. Re-running 'import assets' fills these in.
				stats.UsersFailed++
				tally.add("address not set on the account")
				continue
			}
			logger.Warn("Failed to enable email notifications for %s: %v", matrixUserID, err)
			stats.UsersFailed++
			tally.add("api error")
			continue
		}

		stats.UsersCreated++
	}

	logger.Info("Email notifications: enabled=%d, skipped=%d, failed=%d",
		stats.UsersCreated, stats.UsersSkipped, stats.UsersFailed)
	if summary := tally.String(); summary != "" {
		logger.Info("Email notifications by reason: %s", summary)
	}
	if serverSideMissing {
		logger.Warn("Synapse has no email configuration: set email.enable_notifs and email.notif_from, " +
			"roll it out, then run this step again. Until then no pusher of kind 'email' can exist.")
	}

	return stats, nil
}

// LeaveMigratedRooms makes the migration admin leave every room and space it created,
// given the room and space IDs from the asset mapping.
//
// The import steps already leave rooms inline once force-join is done, but a failure there
// is only logged as a warning, which leaves the admin sitting in a private room or someone
// else's DM. This is the sweep for those leftovers, and it is safe to re-run: a room the
// admin is not in counts as already left, not as an error.
func (i *Importer) LeaveMigratedRooms(roomIDs []string, progress ImportProgressCallback) (*ImportStats, error) {
	stats := &ImportStats{}
	total := len(roomIDs)

	logger.Info("LeaveMigratedRooms: processing %d rooms and spaces", total)

	seen := make(map[string]struct{}, total)
	processed := 0

	for _, roomID := range roomIDs {
		if roomID == "" {
			continue
		}
		// A room can appear under several Mattermost IDs; leaving it twice would report a
		// spurious failure on the second attempt.
		if _, dup := seen[roomID]; dup {
			continue
		}
		seen[roomID] = struct{}{}

		processed++
		if progress != nil {
			progress("leave_rooms", processed, total, roomID)
		}

		err := i.client.LeaveRoom(roomID)
		if err == nil {
			logger.Debug("LeaveMigratedRooms: left %s", roomID)
			stats.RoomsLeft++
			continue
		}
		if isNotInRoomError(err) {
			logger.Debug("LeaveMigratedRooms: not a member of %s, nothing to do", roomID)
			stats.RoomsLeaveSkip++
			continue
		}
		logger.Warn("LeaveMigratedRooms: failed to leave %s: %v", roomID, err)
		stats.RoomsLeaveFail++
	}

	logger.Info("LeaveMigratedRooms completed: left=%d, already-out=%d, failed=%d",
		stats.RoomsLeft, stats.RoomsLeaveSkip, stats.RoomsLeaveFail)

	return stats, nil
}

// isNotInRoomError reports whether a leave failed only because the user was not in the
// room to begin with, which for a cleanup sweep is the desired end state rather than an
// error. Synapse answers M_FORBIDDEN for an unknown membership, so the message is matched
// as well as the error code.
func isNotInRoomError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "not in room"),
		strings.Contains(msg, "not a member"),
		strings.Contains(msg, "unknown room"),
		strings.Contains(msg, "m_not_found"):
		return true
	}
	return false
}
