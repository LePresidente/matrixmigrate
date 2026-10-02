package migration

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/aligundogdu/matrixmigrate/internal/logger"
)

// The two kinds of mapping file kept in the mappings directory. A file of a kind is named
// <kind>-<YYYYMMDD-HHMMSS>.json.
const (
	assetMappingKind   = "asset-mapping"
	messageMappingKind = "message-mapping"
)

// mappingTimestamp is the part of a mapping file name that follows "<kind>-".
var mappingTimestamp = regexp.MustCompile(`^\d{8}-\d{6}\.json$`)

// isMappingFile reports whether path is a file this tool named for the given kind. Anything
// else in the directory - the history-join journal, a hand-made copy, the temp file of an
// atomic write - is not the pruner's to delete.
func isMappingFile(path, kind string) bool {
	timestamp, ok := strings.CutPrefix(filepath.Base(path), kind+"-")
	return ok && mappingTimestamp.MatchString(timestamp)
}

// mappingFilesToPrune picks which of paths to delete so that only the newest keep files of
// kind remain, oldest first. Newest means the latest timestamp in the file name, the same
// rule the importer uses to find the mapping to resume from.
//
// keep <= 0 means pruning is off and nothing is returned. A file whose base name matches one
// of protected is never returned, however old it is: a step's state points at its mapping by
// name, and later steps open exactly that file.
func mappingFilesToPrune(paths []string, kind string, keep int, protected ...string) []string {
	if keep <= 0 {
		return nil
	}

	var candidates []string
	for _, p := range paths {
		if isMappingFile(p, kind) {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) <= keep {
		return nil
	}
	sort.Slice(candidates, func(a, b int) bool {
		return filepath.Base(candidates[a]) < filepath.Base(candidates[b])
	})

	keepNames := make(map[string]struct{}, len(protected))
	for _, p := range protected {
		keepNames[filepath.Base(p)] = struct{}{}
	}

	var prune []string
	for _, p := range candidates[:len(candidates)-keep] {
		if _, ok := keepNames[filepath.Base(p)]; ok {
			continue
		}
		prune = append(prune, p)
	}
	return prune
}

// pruneMappingFiles deletes all but the newest keep mapping files of kind in dir and reports
// how many it removed, how many bytes that freed, and how many it could not remove. A file
// that cannot be removed is logged and left; it is tried again on the next run.
func pruneMappingFiles(dir, kind string, keep int, protected ...string) (removed int, freed int64, failed int) {
	if keep <= 0 {
		return 0, 0, 0
	}
	paths, err := filepath.Glob(filepath.Join(dir, kind+"-*.json"))
	if err != nil {
		logger.Warn("Could not list %s files in %s for pruning: %v", kind, dir, err)
		return 0, 0, 0
	}

	for _, p := range mappingFilesToPrune(paths, kind, keep, protected...) {
		var size int64
		if info, statErr := os.Stat(p); statErr == nil {
			size = info.Size()
		}
		if rmErr := os.Remove(p); rmErr != nil {
			logger.Warn("Could not remove old mapping %s: %v", p, rmErr)
			failed++
			continue
		}
		removed++
		freed += size
	}
	return removed, freed, failed
}

// pruneMappings applies data.keep_mappings to one kind of mapping file. It is called only
// once the step that writes that kind has completed, so a failed or interrupted run never
// deletes the older files an operator would fall back to. current is the file the step just
// wrote; it is kept whatever its name sorts as.
func (o *Orchestrator) pruneMappings(kind, current string) {
	keep := o.config.Data.KeepMappings
	if keep <= 0 {
		return
	}
	removed, freed, failed := pruneMappingFiles(o.config.Data.MappingsDir, kind, keep, current)
	if removed == 0 && failed == 0 {
		return
	}
	logger.Info("Pruned %s files: removed=%d, freed=%d bytes, failed=%d (data.keep_mappings=%d)",
		kind, removed, freed, failed, keep)
}
