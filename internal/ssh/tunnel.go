package ssh

import (
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/aligundogdu/matrixmigrate/internal/config"
)

// Tunnel represents an SSH tunnel with port forwarding
type Tunnel struct {
	client     *ssh.Client
	localAddr  string
	remoteAddr string
	listener   net.Listener
	done       chan struct{}
	wg         sync.WaitGroup
	mu         sync.Mutex
	closed     bool
}

// TunnelConfig holds configuration for creating a tunnel
type TunnelConfig struct {
	SSHConfig   config.SSHConfig
	LocalPort   int
	RemoteHost  string
	RemotePort  int
	Passphrase  string
	Password    string // SSH password (if using password auth)
}

// NewTunnel creates a new SSH tunnel
func NewTunnel(cfg TunnelConfig) (*Tunnel, error) {
	sshConfig, err := newClientConfig(cfg.SSHConfig, cfg.Passphrase, cfg.Password, 30*time.Second)
	if err != nil {
		return nil, err
	}

	// Connect to SSH server
	client, err := ssh.Dial("tcp", dialAddress(cfg.SSHConfig), sshConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to SSH server: %w", err)
	}

	// Create local listener. LocalPort 0 lets the kernel pick a free port while holding it,
	// so nothing can take it between choosing and binding; LocalAddr reports the result.
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.LocalPort))
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("failed to create local listener: %w", err)
	}

	remoteAddr := net.JoinHostPort(cfg.RemoteHost, strconv.Itoa(cfg.RemotePort))

	tunnel := &Tunnel{
		client:     client,
		localAddr:  listener.Addr().String(),
		remoteAddr: remoteAddr,
		listener:   listener,
		done:       make(chan struct{}),
	}

	// Start accepting connections
	tunnel.wg.Add(1)
	go tunnel.acceptConnections()

	return tunnel, nil
}

// buildAuthMethods builds SSH authentication methods based on config
func buildAuthMethods(cfg config.SSHConfig, passphrase, password string) ([]ssh.AuthMethod, error) {
	var authMethods []ssh.AuthMethod

	// Try key-based auth first if key path is provided
	if cfg.KeyPath != "" {
		key, err := loadPrivateKey(cfg.KeyPath, passphrase)
		if err == nil {
			authMethods = append(authMethods, ssh.PublicKeys(key))
		}
		// If key loading fails but password is available, continue to password auth
	}

	// Add password auth if password is provided
	if password != "" {
		authMethods = append(authMethods, ssh.Password(password))
	}

	// Add keyboard-interactive auth (some servers require this for password)
	if password != "" {
		authMethods = append(authMethods, ssh.KeyboardInteractive(
			func(user, instruction string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range questions {
					answers[i] = password
				}
				return answers, nil
			},
		))
	}

	if len(authMethods) == 0 {
		return nil, fmt.Errorf("no authentication method available: provide either key_path or password")
	}

	return authMethods, nil
}

// loadPrivateKey loads an SSH private key from file
func loadPrivateKey(keyPath, passphrase string) (ssh.Signer, error) {
	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read key file: %w", err)
	}

	var key ssh.Signer
	if passphrase != "" {
		key, err = ssh.ParsePrivateKeyWithPassphrase(keyData, []byte(passphrase))
	} else {
		key, err = ssh.ParsePrivateKey(keyData)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key: %w", err)
	}

	return key, nil
}

// acceptConnections accepts incoming connections and forwards them
func (t *Tunnel) acceptConnections() {
	defer t.wg.Done()

	for {
		select {
		case <-t.done:
			return
		default:
		}

		// Set deadline to allow periodic checking of done channel
		t.listener.(*net.TCPListener).SetDeadline(time.Now().Add(1 * time.Second))

		conn, err := t.listener.Accept()
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			// Check if we're shutting down
			t.mu.Lock()
			closed := t.closed
			t.mu.Unlock()
			if closed {
				return
			}
			continue
		}

		t.wg.Add(1)
		go t.forward(conn)
	}
}

// forward forwards a connection through the SSH tunnel
func (t *Tunnel) forward(localConn net.Conn) {
	defer t.wg.Done()
	defer localConn.Close()

	// Connect to remote through SSH
	remoteConn, err := t.client.Dial("tcp", t.remoteAddr)
	if err != nil {
		return
	}
	defer remoteConn.Close()

	// Bidirectional copy
	done := make(chan struct{}, 2)

	go func() {
		io.Copy(remoteConn, localConn)
		done <- struct{}{}
	}()

	go func() {
		io.Copy(localConn, remoteConn)
		done <- struct{}{}
	}()

	// Wait for one direction to finish
	<-done
}

// LocalAddr returns the local address of the tunnel
func (t *Tunnel) LocalAddr() string {
	return t.localAddr
}

// LocalPort returns the local port the tunnel listens on
func (t *Tunnel) LocalPort() int {
	return t.listener.Addr().(*net.TCPAddr).Port
}

// RemoteAddr returns the remote address of the tunnel
func (t *Tunnel) RemoteAddr() string {
	return t.remoteAddr
}

// Close closes the tunnel and all connections
func (t *Tunnel) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()

	// Signal to stop accepting connections
	close(t.done)

	// Close listener
	if t.listener != nil {
		t.listener.Close()
	}

	// Close the SSH client before waiting: it ends the forwarded connections, which would
	// otherwise keep their goroutines alive for as long as the peers keep them open.
	if t.client != nil {
		t.client.Close()
	}

	// Wait for all goroutines to finish
	t.wg.Wait()

	return nil
}

// TestConnection tests the SSH connection without creating a tunnel
func TestConnection(cfg config.SSHConfig, passphrase string) error {
	return TestConnectionWithPassword(cfg, passphrase, "")
}

// TestConnectionWithPassword tests SSH connection with optional password
func TestConnectionWithPassword(cfg config.SSHConfig, passphrase, password string) error {
	sshConfig, err := newClientConfig(cfg, passphrase, password, 10*time.Second)
	if err != nil {
		return err
	}

	// Connect to SSH server
	client, err := ssh.Dial("tcp", dialAddress(cfg), sshConfig)
	if err != nil {
		return fmt.Errorf("SSH connection failed: %w", err)
	}
	defer client.Close()

	return nil
}

// TunnelManager manages multiple SSH tunnels
type TunnelManager struct {
	tunnels map[string]*Tunnel
	mu      sync.Mutex
}

// NewTunnelManager creates a new tunnel manager
func NewTunnelManager() *TunnelManager {
	return &TunnelManager{
		tunnels: make(map[string]*Tunnel),
	}
}

// CreateTunnel creates and registers a new tunnel
func (tm *TunnelManager) CreateTunnel(name string, cfg TunnelConfig) (*Tunnel, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	// If tunnel already exists, return it (reuse existing connection)
	if existing, exists := tm.tunnels[name]; exists {
		return existing, nil
	}

	tunnel, err := NewTunnel(cfg)
	if err != nil {
		return nil, err
	}

	tm.tunnels[name] = tunnel
	return tunnel, nil
}

// GetTunnel returns a tunnel by name
func (tm *TunnelManager) GetTunnel(name string) (*Tunnel, bool) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tunnel, exists := tm.tunnels[name]
	return tunnel, exists
}

// CloseTunnel closes and removes a tunnel by name
func (tm *TunnelManager) CloseTunnel(name string) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	tunnel, exists := tm.tunnels[name]
	if !exists {
		return fmt.Errorf("tunnel %s not found", name)
	}

	if err := tunnel.Close(); err != nil {
		return err
	}

	delete(tm.tunnels, name)
	return nil
}

// CloseAll closes all tunnels
func (tm *TunnelManager) CloseAll() error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	var lastErr error
	for name, tunnel := range tm.tunnels {
		if err := tunnel.Close(); err != nil {
			lastErr = err
		}
		delete(tm.tunnels, name)
	}

	return lastErr
}
