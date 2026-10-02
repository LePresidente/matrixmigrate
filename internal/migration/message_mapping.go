package migration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aligundogdu/matrixmigrate/internal/mattermost"
	"github.com/aligundogdu/matrixmigrate/pkg/archive"
)

// GenerateMessageErrorsFilename returns a timestamped path for the message-error log.
func GenerateMessageErrorsFilename(dir string) string {
	timestamp := time.Now().Format("20060102-150405")
	return filepath.Join(dir, fmt.Sprintf("message-errors-%s.log", timestamp))
}

// WriteMessageErrors writes one error per line to a timestamped file and returns its path.
func WriteMessageErrors(dir string, errs []string) (string, error) {
	path := GenerateMessageErrorsFilename(dir)
	var b strings.Builder
	for _, e := range errs {
		b.WriteString(e)
		b.WriteByte('\n')
	}
	if err := archive.WriteFileAtomic(path, []byte(b.String()), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// CategorizeMessageErrors buckets raw import errors by failure mode for a summary line.
func CategorizeMessageErrors(errs []string) map[string]int {
	counts := make(map[string]int)
	for _, e := range errs {
		switch {
		case strings.Contains(e, "No room mapping"):
			counts["no_room"]++
		case strings.Contains(e, "Failed to send reply"):
			counts["reply_error"]++
		case strings.Contains(e, "Failed to send message"):
			counts["send_error"]++
		case strings.Contains(e, "Failed to send reaction"):
			counts["reaction_error"]++
		case strings.Contains(e, "Failed to pin messages"), strings.Contains(e, "Failed to read pinned messages"):
			counts["pin_error"]++
		case strings.Contains(e, "Parent post"):
			counts["parent_missing"]++
		default:
			counts["other"]++
		}
	}
	return counts
}

// MessageMapping represents the mapping between Mattermost posts and Matrix events
type MessageMapping struct {
	Version    string                     `json:"version"`
	CreatedAt  int64                      `json:"created_at"`
	UpdatedAt  int64                      `json:"updated_at"`
	Homeserver string                     `json:"homeserver"`
	Messages   map[string]*MessageMapEntry `json:"messages"` // key: Mattermost post ID
	// Reactions records which reactions have been sent, keyed by mattermost.Reaction.Key()
	// (post ID, user ID and emoji name joined). Mattermost gives reactions no ID of their own,
	// and Matrix does not deduplicate annotations, so without this a second run would stack a
	// duplicate of every reaction on top of the first.
	Reactions map[string]string `json:"reactions,omitempty"`
	// Files records which attachments have been sent: Mattermost file ID -> event ID of the
	// event that carried it, or "" for a file taken as sent when this record was introduced
	// (see adoptLegacyFileTracking). An attachment missing from it is sent by the next run.
	//
	// omitzero rather than omitempty: an empty map must still be written, because a mapping
	// with no "files" key is read as one that predates file tracking.
	Files map[string]string `json:"files,omitzero"`
	// filesUntracked is set when the file this mapping was loaded from had no "files" key.
	filesUntracked bool
	mu             sync.RWMutex `json:"-"`
}

// MessageMapEntry represents a single message mapping
type MessageMapEntry struct {
	MattermostID  string `json:"mattermost_id"`   // Mattermost post ID
	MatrixEventID string `json:"matrix_event_id"` // Matrix event ID ($xxx)
	ChannelID     string `json:"channel_id"`      // Mattermost channel ID
	RoomID        string `json:"room_id"`         // Matrix room ID
	UserID        string `json:"user_id"`         // Mattermost user ID
	MatrixUserID  string `json:"matrix_user_id"`  // Matrix user ID
	Timestamp     int64  `json:"timestamp"`       // Original message timestamp
	ImportedAt    int64  `json:"imported_at"`     // When this was imported
	IsReply       bool   `json:"is_reply"`        // Whether this is a reply
	RootID        string `json:"root_id,omitempty"` // Mattermost root post ID if reply
}

// NewMessageMapping creates a new message mapping
func NewMessageMapping(homeserver string) *MessageMapping {
	now := time.Now().UnixMilli()
	return &MessageMapping{
		Version:    "1.0",
		CreatedAt:  now,
		UpdatedAt:  now,
		Homeserver: homeserver,
		Messages:   make(map[string]*MessageMapEntry),
		Reactions:  make(map[string]string),
		Files:      make(map[string]string),
	}
}

// AddMessage adds a message mapping
func (m *MessageMapping) AddMessage(entry *MessageMapEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	entry.ImportedAt = time.Now().UnixMilli()
	m.Messages[entry.MattermostID] = entry
	m.UpdatedAt = time.Now().UnixMilli()
}

// GetMessage returns a message mapping by Mattermost ID
func (m *MessageMapping) GetMessage(mattermostID string) (*MessageMapEntry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	entry, exists := m.Messages[mattermostID]
	return entry, exists
}

// HasMessage checks if a message has already been imported
func (m *MessageMapping) HasMessage(mattermostID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	_, exists := m.Messages[mattermostID]
	return exists
}

// AddReaction records a sent reaction under its Mattermost reaction key.
func (m *MessageMapping) AddReaction(reactionKey, matrixEventID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Reactions == nil {
		m.Reactions = make(map[string]string)
	}
	m.Reactions[reactionKey] = matrixEventID
	m.UpdatedAt = time.Now().UnixMilli()
}

// HasReaction reports whether a reaction has already been sent.
func (m *MessageMapping) HasReaction(reactionKey string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	_, exists := m.Reactions[reactionKey]
	return exists
}

// ReactionCount returns the number of recorded reactions.
func (m *MessageMapping) ReactionCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.Reactions)
}

// ReactionKeys returns a copy of the recorded reaction keys and their event IDs, for handing
// to an import run as the set it should not send again.
func (m *MessageMapping) ReactionKeys() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make(map[string]string, len(m.Reactions))
	for k, v := range m.Reactions {
		out[k] = v
	}
	return out
}

// AddFile records a sent attachment under its Mattermost file ID.
func (m *MessageMapping) AddFile(fileID, matrixEventID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Files == nil {
		m.Files = make(map[string]string)
	}
	m.Files[fileID] = matrixEventID
	m.UpdatedAt = time.Now().UnixMilli()
}

// HasFile reports whether an attachment has already been sent.
func (m *MessageMapping) HasFile(fileID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	_, exists := m.Files[fileID]
	return exists
}

// FileCount returns the number of recorded attachments.
func (m *MessageMapping) FileCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.Files)
}

// FileIDs returns a copy of the recorded attachments and their event IDs, for handing to an
// import run as the set it should not send again.
func (m *MessageMapping) FileIDs() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make(map[string]string, len(m.Files))
	for k, v := range m.Files {
		out[k] = v
	}
	return out
}

// adoptLegacyFileTracking brings a mapping written before attachments were tracked up to date:
// every file whose post is already in the mapping is marked as sent, with no event ID. Nothing
// is known about which of those attachments made it, and assuming none did would upload every
// old attachment a second time. It returns how many files it marked; a mapping that already
// tracks files is left alone, and so is one that has been adopted before.
func adoptLegacyFileTracking(m *MessageMapping, files []mattermost.FileInfo) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.filesUntracked {
		return 0
	}
	m.filesUntracked = false
	if m.Files == nil {
		m.Files = make(map[string]string)
	}
	marked := 0
	for idx := range files {
		f := &files[idx]
		if _, imported := m.Messages[f.PostID]; !imported {
			continue
		}
		if _, known := m.Files[f.ID]; known {
			continue
		}
		m.Files[f.ID] = ""
		marked++
	}
	return marked
}

// GetMatrixEventID returns the Matrix event ID for a Mattermost post
func (m *MessageMapping) GetMatrixEventID(mattermostID string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	if entry, exists := m.Messages[mattermostID]; exists {
		return entry.MatrixEventID
	}
	return ""
}

// Count returns the number of mapped messages
func (m *MessageMapping) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.Messages)
}

// GetStats returns statistics about the mapping
func (m *MessageMapping) GetStats() MessageMappingStats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	stats := MessageMappingStats{
		Total:        len(m.Messages),
		ByChannel:   make(map[string]int),
		ByRoom:      make(map[string]int),
	}
	
	for _, entry := range m.Messages {
		if entry.IsReply {
			stats.Replies++
		}
		stats.ByChannel[entry.ChannelID]++
		stats.ByRoom[entry.RoomID]++
	}
	
	return stats
}

// MessageMappingStats holds statistics about message mappings
type MessageMappingStats struct {
	Total     int            `json:"total"`
	Replies   int            `json:"replies"`
	ByChannel map[string]int `json:"by_channel"`
	ByRoom    map[string]int `json:"by_room"`
}

// SaveMessageMapping saves the message mapping to a file
func SaveMessageMapping(mapping *MessageMapping, filepath string) error {
	mapping.mu.Lock()
	defer mapping.mu.Unlock()
	
	mapping.UpdatedAt = time.Now().UnixMilli()
	
	data, err := json.MarshalIndent(mapping, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal message mapping: %w", err)
	}
	
	if err := archive.WriteFileAtomic(filepath, data, 0600); err != nil {
		return fmt.Errorf("failed to write message mapping file: %w", err)
	}
	
	return nil
}

// loadOrCreateMessageMapping returns a fresh mapping when no mapping file exists
// (path is empty). If a file exists but cannot be loaded it fails rather than
// starting empty: an empty mapping would make the import resend every message
// that was already imported.
func loadOrCreateMessageMapping(path, homeserver string) (*MessageMapping, error) {
	if path == "" {
		return NewMessageMapping(homeserver), nil
	}
	m, err := LoadMessageMapping(path)
	if err != nil {
		return nil, fmt.Errorf("message mapping file %s could not be loaded: %w; the import was NOT started, "+
			"because starting without it would send every already-imported message a second time; "+
			"restore the file, or move it away if a fresh import is really intended", path, err)
	}
	return m, nil
}

// LoadMessageMapping loads a message mapping from a file
func LoadMessageMapping(filepath string) (*MessageMapping, error) {
	data, err := os.ReadFile(filepath)
	if err != nil {
		return nil, fmt.Errorf("failed to read message mapping file: %w", err)
	}
	
	var mapping MessageMapping
	if err := json.Unmarshal(data, &mapping); err != nil {
		return nil, fmt.Errorf("failed to unmarshal message mapping: %w", err)
	}
	
	if mapping.Messages == nil {
		mapping.Messages = make(map[string]*MessageMapEntry)
	}
	// Mapping files written before reactions existed have no such key.
	if mapping.Reactions == nil {
		mapping.Reactions = make(map[string]string)
	}
	// Nor did files written before attachments were tracked - and there the absence means
	// something: see adoptLegacyFileTracking.
	if mapping.Files == nil {
		mapping.filesUntracked = true
		mapping.Files = make(map[string]string)
	}
	
	return &mapping, nil
}

// GenerateMessageMappingFilename generates a filename for message mapping
func GenerateMessageMappingFilename(dir string) string {
	timestamp := time.Now().Format("20060102-150405")
	return filepath.Join(dir, fmt.Sprintf("message-mapping-%s.json", timestamp))
}

// GetLatestMessageMappingFile returns the newest message-mapping file in dir by the timestamp
// in its name, or "" when there is none.
func GetLatestMessageMappingFile(dir string) (string, error) {
	return latestFileByName(filepath.Join(dir, "message-mapping-*.json"))
}
