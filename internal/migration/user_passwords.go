package migration

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aligundogdu/matrixmigrate/internal/matrix"
)

// GenerateUserPasswordsFilename returns a timestamped path for the generated-credentials file.
func GenerateUserPasswordsFilename(dir string) string {
	timestamp := time.Now().Format("20060102-150405")
	return filepath.Join(dir, fmt.Sprintf("user-passwords-%s.csv", timestamp))
}

// WriteUserPasswords writes generated user credentials to a timestamped CSV and returns its
// path. The file is created 0600 because it contains live passwords; distribute and delete it.
//
// Returns an empty path and no error when there are no credentials to write.
func WriteUserPasswords(dir string, creds []matrix.UserCredential) (string, error) {
	if len(creds) == 0 {
		return "", nil
	}

	path := GenerateUserPasswordsFilename(dir)
	// O_EXCL so a same-second rerun cannot silently overwrite an earlier set of credentials.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if err := w.Write([]string{"mattermost_username", "matrix_user_id", "password"}); err != nil {
		return "", err
	}
	for _, c := range creds {
		if err := w.Write([]string{c.Username, c.MatrixUserID, c.Password}); err != nil {
			return "", err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return "", err
	}
	return path, nil
}
