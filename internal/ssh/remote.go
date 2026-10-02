package ssh

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/aligundogdu/matrixmigrate/internal/config"
)

// RemoteExecutor executes commands on remote servers via SSH
type RemoteExecutor struct {
	client *ssh.Client
}

// NewRemoteExecutor creates a new remote executor with key auth
func NewRemoteExecutor(cfg config.SSHConfig, passphrase string) (*RemoteExecutor, error) {
	return NewRemoteExecutorWithPassword(cfg, passphrase, "")
}

// NewRemoteExecutorWithPassword creates a new remote executor with optional password auth
func NewRemoteExecutorWithPassword(cfg config.SSHConfig, passphrase, password string) (*RemoteExecutor, error) {
	sshConfig, err := newClientConfig(cfg, passphrase, password, 30*time.Second)
	if err != nil {
		return nil, err
	}

	// Connect to SSH server
	client, err := ssh.Dial("tcp", dialAddress(cfg), sshConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to SSH server: %w", err)
	}

	return &RemoteExecutor{client: client}, nil
}

// Close closes the SSH connection
func (r *RemoteExecutor) Close() error {
	if r.client != nil {
		return r.client.Close()
	}
	return nil
}

// ReadFileAsUser reads a file from the remote server as the SSH user, without sudo.
func (r *RemoteExecutor) ReadFileAsUser(path string) ([]byte, error) {
	session, err := r.client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	if err := session.Run("cat " + shellQuote(path)); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("failed to read file %s as SSH user: %s", path, detail)
	}

	return stdout.Bytes(), nil
}

// ReadFile reads a file from the remote server, falling back to `sudo cat` when the SSH
// user cannot read it. Used for Mattermost's config.json; attachments use ReadFileAsUser
// unless mattermost.files.read_with_sudo is set.
func (r *RemoteExecutor) ReadFile(path string) ([]byte, error) {
	session, err := r.client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	// Use cat to read the file, with sudo if needed.
	quotedPath := shellQuote(path)
	cmd := fmt.Sprintf("cat %s 2>/dev/null || sudo cat %s", quotedPath, quotedPath)
	if err := session.Run(cmd); err != nil {
		return nil, fmt.Errorf("failed to read file: %s", stderr.String())
	}

	return stdout.Bytes(), nil
}

// FileExists checks if a file exists on the remote server
func (r *RemoteExecutor) FileExists(path string) (bool, error) {
	session, err := r.client.NewSession()
	if err != nil {
		return false, fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()

	cmd := fmt.Sprintf("test -f %s && echo 'exists'", shellQuote(path))
	output, err := session.Output(cmd)
	if err != nil {
		return false, nil // File doesn't exist
	}

	return bytes.Contains(output, []byte("exists")), nil
}

// shellQuote returns a POSIX shell-safe single-quoted string.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
