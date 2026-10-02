package archive

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to path so that a reader never observes a
// partially written file. The data goes to a temp file in the same directory
// (so the final rename stays on one filesystem), is synced, given perm, and
// renamed over path. On any failure the temp file is removed.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	return writeAtomic(path, perm, func(f *os.File) error {
		_, err := f.Write(data)
		return err
	})
}

// writeAtomic is WriteFileAtomic with a streaming writer callback. The callback
// must not close the file.
func writeAtomic(path string, perm os.FileMode, write func(f *os.File) error) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpName := tmp.Name()
	closed := false
	defer func() {
		if err != nil {
			if !closed {
				tmp.Close()
			}
			os.Remove(tmpName)
		}
	}()

	if err = write(tmp); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("failed to sync temp file: %w", err)
	}
	closed = true
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}
	if err = os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("failed to set permissions: %w", err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("failed to rename temp file: %w", err)
	}
	return nil
}
