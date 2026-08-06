package mattermost

import (
	"fmt"
	"strings"
	"time"

	"github.com/aligundogdu/matrixmigrate/internal/logger"
)

// Exporter handles exporting data from Mattermost
type Exporter struct {
	client *Client
}

// NewExporter creates a new exporter
func NewExporter(client *Client) *Exporter {
	return &Exporter{client: client}
}

// ExportProgressCallback is called to report export progress
type ExportProgressCallback func(stage string, current, total int)

// ExportAssets exports all assets (users, teams, channels). When includeDirectMessages is true,
// also exports direct message channels (type D) into assets.DirectChannels.
func (e *Exporter) ExportAssets(progress ExportProgressCallback, includeDirectMessages bool) (*Assets, error) {
	assets := &Assets{
		ExportedAt: time.Now().UnixMilli(),
		Version:    "1.0",
	}

	// Export users
	if progress != nil {
		progress("users", 0, 0)
	}
	users, err := e.client.GetUsers()
	if err != nil {
		return nil, fmt.Errorf("failed to export users: %w", err)
	}
	assets.Users = users
	if progress != nil {
		progress("users", len(users), len(users))
	}

	// Export teams
	if progress != nil {
		progress("teams", 0, 0)
	}
	teams, err := e.client.GetTeams()
	if err != nil {
		return nil, fmt.Errorf("failed to export teams: %w", err)
	}
	assets.Teams = teams
	if progress != nil {
		progress("teams", len(teams), len(teams))
	}

	// Export channels (O, P, G, and optionally D via GetChannels(includeDirect))
	if progress != nil {
		progress("channels", 0, 0)
	}
	allChannels, err := e.client.GetChannels(includeDirectMessages)
	if err != nil {
		return nil, fmt.Errorf("failed to export channels: %w", err)
	}
	for _, ch := range allChannels {
		if ch.IsDirect() {
			assets.DirectChannels = append(assets.DirectChannels, ch)
		} else {
			assets.Channels = append(assets.Channels, ch)
		}
	}
	if progress != nil {
		progress("channels", len(allChannels), len(allChannels))
	}

	return assets, nil
}

// ExportMemberships exports all memberships (team and channel members)
func (e *Exporter) ExportMemberships(progress ExportProgressCallback) (*Memberships, error) {
	memberships := &Memberships{
		ExportedAt: time.Now().UnixMilli(),
		Version:    "1.0",
	}

	// Export team members
	if progress != nil {
		progress("team_members", 0, 0)
	}
	teamMembers, err := e.client.GetTeamMembers()
	if err != nil {
		return nil, fmt.Errorf("failed to export team members: %w", err)
	}
	memberships.TeamMembers = teamMembers
	if progress != nil {
		progress("team_members", len(teamMembers), len(teamMembers))
	}

	// Export channel members
	if progress != nil {
		progress("channel_members", 0, 0)
	}
	channelMembers, err := e.client.GetChannelMembers()
	if err != nil {
		return nil, fmt.Errorf("failed to export channel members: %w", err)
	}
	memberships.ChannelMembers = channelMembers
	if progress != nil {
		progress("channel_members", len(channelMembers), len(channelMembers))
	}

	return memberships, nil
}

// GetCounts returns the counts of all entities
func (e *Exporter) GetCounts() (users, teams, channels int, err error) {
	users, err = e.client.GetUserCount()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to get user count: %w", err)
	}

	teams, err = e.client.GetTeamCount()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to get team count: %w", err)
	}

	channels, err = e.client.GetChannelCount()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to get channel count: %w", err)
	}

	return users, teams, channels, nil
}

// FilterActiveAssets filters out deleted teams and channels from assets.
// All users are kept (including deleted/deactivated) so they can be imported into Matrix as deactivated for channel history.
func FilterActiveAssets(assets *Assets) *Assets {
	filtered := &Assets{
		ExportedAt: assets.ExportedAt,
		Version:    assets.Version,
	}

	// Keep all users (deleted users are imported into Matrix as deactivated for message attribution)
	filtered.Users = assets.Users

	for _, t := range assets.Teams {
		if !t.IsDeleted() {
			filtered.Teams = append(filtered.Teams, t)
		}
	}

	for _, c := range assets.Channels {
		if !c.IsDeleted() {
			filtered.Channels = append(filtered.Channels, c)
		}
	}

	for _, c := range assets.DirectChannels {
		if !c.IsDeleted() {
			filtered.DirectChannels = append(filtered.DirectChannels, c)
		}
	}

	return filtered
}

// FilterActiveMemberships filters out deleted memberships
func FilterActiveMemberships(memberships *Memberships) *Memberships {
	filtered := &Memberships{
		ExportedAt: memberships.ExportedAt,
		Version:    memberships.Version,
	}

	for _, tm := range memberships.TeamMembers {
		if !tm.IsDeleted() {
			filtered.TeamMembers = append(filtered.TeamMembers, tm)
		}
	}

	// Channel members don't have DeleteAt, copy all
	filtered.ChannelMembers = memberships.ChannelMembers

	return filtered
}

func normalizeIgnoredUsers(ignoredUsers []string) map[string]struct{} {
	ignored := make(map[string]struct{}, len(ignoredUsers))
	for _, username := range ignoredUsers {
		u := strings.ToLower(strings.TrimSpace(username))
		if u == "" {
			continue
		}
		ignored[u] = struct{}{}
	}
	return ignored
}

// GetIgnoredUserIDs resolves configured ignored usernames to Mattermost user IDs.
func GetIgnoredUserIDs(users []User, ignoredUsers []string) map[string]struct{} {
	ignoredByName := normalizeIgnoredUsers(ignoredUsers)
	ignoredIDs := make(map[string]struct{})
	if len(ignoredByName) == 0 {
		return ignoredIDs
	}

	for _, user := range users {
		if _, ok := ignoredByName[strings.ToLower(user.Username)]; ok {
			ignoredIDs[user.ID] = struct{}{}
		}
	}
	return ignoredIDs
}

// FilterIgnoredUsersFromAssets removes users that match configured ignored usernames.
func FilterIgnoredUsersFromAssets(assets *Assets, ignoredUsers []string) *Assets {
	filtered := &Assets{
		ExportedAt:     assets.ExportedAt,
		Version:        assets.Version,
		Teams:          assets.Teams,
		Channels:       assets.Channels,
		DirectChannels: assets.DirectChannels,
	}

	ignoredByName := normalizeIgnoredUsers(ignoredUsers)
	if len(ignoredByName) == 0 {
		filtered.Users = assets.Users
		return filtered
	}

	for _, user := range assets.Users {
		if _, ignore := ignoredByName[strings.ToLower(user.Username)]; ignore {
			continue
		}
		filtered.Users = append(filtered.Users, user)
	}
	return filtered
}

// FilterMembershipsByIgnoredUserIDs removes team/channel memberships for ignored users.
func FilterMembershipsByIgnoredUserIDs(memberships *Memberships, ignoredUserIDs map[string]struct{}) *Memberships {
	filtered := &Memberships{
		ExportedAt: memberships.ExportedAt,
		Version:    memberships.Version,
	}

	if len(ignoredUserIDs) == 0 {
		filtered.TeamMembers = memberships.TeamMembers
		filtered.ChannelMembers = memberships.ChannelMembers
		return filtered
	}

	for _, tm := range memberships.TeamMembers {
		if _, ignore := ignoredUserIDs[tm.UserID]; ignore {
			continue
		}
		filtered.TeamMembers = append(filtered.TeamMembers, tm)
	}
	for _, cm := range memberships.ChannelMembers {
		if _, ignore := ignoredUserIDs[cm.UserID]; ignore {
			continue
		}
		filtered.ChannelMembers = append(filtered.ChannelMembers, cm)
	}
	return filtered
}

// ExportMessages exports all messages (posts) and file attachments
func (e *Exporter) ExportMessages(progress ExportProgressCallback) (*Messages, error) {
	messages := &Messages{
		ExportedAt: time.Now().UnixMilli(),
		Version:    "1.0",
	}

	// Get total count first
	totalCount, err := e.client.GetPostCount()
	if err != nil {
		return nil, fmt.Errorf("failed to get post count: %w", err)
	}

	if progress != nil {
		progress("messages", 0, totalCount)
	}

	// Export posts
	posts, err := e.client.GetPosts()
	if err != nil {
		return nil, fmt.Errorf("failed to export posts: %w", err)
	}
	messages.Posts = posts

	if progress != nil {
		progress("messages", len(posts), totalCount)
	}

	// Export file infos
	if progress != nil {
		progress("files", 0, 0)
	}

	files, err := e.client.GetFileInfos()
	if err != nil {
		// Non-fatal: continue without files
		// Some Mattermost installations might not have files
	} else {
		messages.Files = files
		if progress != nil {
			progress("files", len(files), len(files))
		}
	}

	// Export reactions. Non-fatal like files, but loudly: a silent failure here would look
	// exactly like an instance where nobody ever reacted to anything.
	if progress != nil {
		reactionCount, countErr := e.client.GetReactionCount()
		if countErr != nil {
			reactionCount = 0
		}
		progress("reactions", 0, reactionCount)
	}

	reactions, err := e.client.GetReactions()
	if err != nil {
		logger.Warn("Failed to export reactions, continuing without them: %v", err)
	} else {
		messages.Reactions = reactions
		if len(reactions) == 0 {
			logger.Info("No reactions found to export")
		}
		if progress != nil {
			progress("reactions", len(reactions), len(reactions))
		}
	}

	return messages, nil
}

// GetMessageCount returns the total number of messages
func (e *Exporter) GetMessageCount() (int, error) {
	return e.client.GetPostCount()
}

// GetFileCount returns the total number of files
func (e *Exporter) GetFileCount() (int, error) {
	return e.client.GetFileInfoCount()
}
