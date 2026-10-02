package migration

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/aligundogdu/matrixmigrate/internal/logger"
	"github.com/aligundogdu/matrixmigrate/internal/matrix"
	"github.com/aligundogdu/matrixmigrate/pkg/archive"
)

// historyJoinJournalName is the journal's file name inside the mappings directory. It is not
// timestamped: there is one set of outstanding history joins per migration, carried from run
// to run until they have all been withdrawn.
const historyJoinJournalName = "history-joins.json"

// HistoryJoinJournalPath returns where the history-join journal lives in mappingsDir.
func HistoryJoinJournalPath(mappingsDir string) string {
	return filepath.Join(mappingsDir, historyJoinJournalName)
}

// historyJoinEntry is one journal record, in its on-disk form.
type historyJoinEntry struct {
	RoomID string `json:"room_id"`
	UserID string `json:"user_id"`
}

type historyJoinFile struct {
	Joins []historyJoinEntry `json:"joins"`
}

// HistoryJoinJournal is the on-disk record of memberships the message import created only to
// replay history, and has not yet withdrawn.
//
// The import force-joins people who have since left a channel so their old messages can be
// sent as them, and takes them out again at the end. Held only in memory, an interrupted run
// would leave them in rooms they had left - private ones included - with nothing to say so.
// Each join is therefore written here before it is made, and the journal is cut back to what
// could not be withdrawn once the cleanup has run.
type HistoryJoinJournal struct {
	path  string
	joins []matrix.HistoryMembership
	seen  map[matrix.HistoryMembership]struct{}
}

// LoadHistoryJoinJournal reads the journal at path. A missing file is an empty journal. A file
// that exists but cannot be read or parsed is an error: treating it as empty would forget
// memberships that still have to be withdrawn.
func LoadHistoryJoinJournal(path string) (*HistoryJoinJournal, error) {
	j := &HistoryJoinJournal{path: path, seen: make(map[matrix.HistoryMembership]struct{})}

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return j, nil
	}
	if err != nil {
		return nil, fmt.Errorf("history-join journal %s could not be read: %w", path, err)
	}

	var file historyJoinFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("history-join journal %s could not be parsed: %w; it lists people joined to rooms "+
			"only to replay history, who still have to be removed; restore the file, or move it away once "+
			"those memberships have been dealt with", path, err)
	}
	for _, e := range file.Joins {
		j.insert(matrix.HistoryMembership{RoomID: e.RoomID, UserID: e.UserID})
	}
	return j, nil
}

// Memberships returns the journalled memberships in the order they were first recorded.
func (j *HistoryJoinJournal) Memberships() []matrix.HistoryMembership {
	return append([]matrix.HistoryMembership(nil), j.joins...)
}

// Add records m and writes the journal. A membership already recorded is not written again.
func (j *HistoryJoinJournal) Add(m matrix.HistoryMembership) error {
	if !j.insert(m) {
		return nil
	}
	return j.save()
}

// ReplaceAll makes memberships the journal's whole content and writes it.
func (j *HistoryJoinJournal) ReplaceAll(memberships []matrix.HistoryMembership) error {
	j.joins = nil
	j.seen = make(map[matrix.HistoryMembership]struct{}, len(memberships))
	for _, m := range memberships {
		j.insert(m)
	}
	return j.save()
}

// insert adds m to the in-memory list, reporting whether it was new.
func (j *HistoryJoinJournal) insert(m matrix.HistoryMembership) bool {
	if _, dup := j.seen[m]; dup {
		return false
	}
	j.seen[m] = struct{}{}
	j.joins = append(j.joins, m)
	return true
}

func (j *HistoryJoinJournal) save() error {
	file := historyJoinFile{Joins: make([]historyJoinEntry, 0, len(j.joins))}
	for _, m := range j.joins {
		file.Joins = append(file.Joins, historyJoinEntry{RoomID: m.RoomID, UserID: m.UserID})
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal history-join journal: %w", err)
	}
	if err := archive.WriteFileAtomic(j.path, data, 0o600); err != nil {
		return fmt.Errorf("failed to write history-join journal %s: %w", j.path, err)
	}
	return nil
}

// attachHistoryJoinJournal hands importer the memberships an earlier run left behind, so its
// cleanup withdraws them too, and has every new history join written to journal before it is
// made.
func attachHistoryJoinJournal(importer *matrix.Importer, journal *HistoryJoinJournal) {
	if pending := journal.Memberships(); len(pending) > 0 {
		logger.Info("History-join journal lists %d membership(s) from an earlier run still to withdraw", len(pending))
		importer.AddHistoryJoins(pending...)
	}
	importer.SetHistoryJoinRecorder(func(m matrix.HistoryMembership) {
		if err := journal.Add(m); err != nil {
			// The join goes ahead regardless: the in-memory record still lets this run undo
			// it, and only a crash before the cleanup would leave it unrecorded.
			logger.Warn("Could not record history join of %s to %s: %v", m.UserID, m.RoomID, err)
		}
	})
}

// withdrawHistoryJoins runs importer's history-membership cleanup and cuts journal back to the
// memberships still in place, so a later run retries exactly those.
func withdrawHistoryJoins(importer *matrix.Importer, journal *HistoryJoinJournal) *matrix.LeaveRoomsResult {
	cleanup := importer.LeaveHistoryMemberships()
	if err := journal.ReplaceAll(cleanup.Remaining); err != nil {
		logger.Warn("Could not update the history-join journal: %v; it may still list memberships already withdrawn, which a later run will find already gone", err)
	} else if len(cleanup.Remaining) > 0 {
		logger.Warn("%d history membership(s) could not be withdrawn and stay recorded in %s; run 'import leave-rooms' to retry",
			len(cleanup.Remaining), journal.path)
	}
	return cleanup
}
