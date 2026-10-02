package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/aligundogdu/matrixmigrate/internal/config"
	"github.com/aligundogdu/matrixmigrate/internal/logger"
)

// newClientConfig builds the SSH client configuration used by every connection this package
// makes: authentication from buildAuthMethods and host key verification from
// newHostKeyPolicy.
func newClientConfig(cfg config.SSHConfig, passphrase, password string, timeout time.Duration) (*ssh.ClientConfig, error) {
	authMethods, err := buildAuthMethods(cfg, passphrase, password)
	if err != nil {
		return nil, fmt.Errorf("failed to build auth methods: %w", err)
	}

	policy, err := newHostKeyPolicy(cfg)
	if err != nil {
		return nil, err
	}

	return &ssh.ClientConfig{
		User:              cfg.User,
		Auth:              authMethods,
		HostKeyCallback:   policy.callback,
		HostKeyAlgorithms: policy.algorithms,
		Timeout:           timeout,
	}, nil
}

// dialAddress is the host:port string dialled for cfg. ssh.Dial passes the same string to
// the host key callback as the hostname.
func dialAddress(cfg config.SSHConfig) string {
	return net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
}

// hostKeyPolicy is the host key check for one SSH server.
type hostKeyPolicy struct {
	callback ssh.HostKeyCallback
	// algorithms restricts the host key types offered during the handshake to those
	// known_hosts holds for the server. Nil leaves the library default.
	algorithms []string
}

// newHostKeyPolicy decides how the host key of cfg's server is verified, in order:
// insecure_ignore_host_key accepts any key, host_key_fingerprint pins one key, otherwise
// the key must be recorded in the known_hosts file.
func newHostKeyPolicy(cfg config.SSHConfig) (*hostKeyPolicy, error) {
	if cfg.InsecureIgnoreHostKey {
		return &hostKeyPolicy{callback: insecureCallback}, nil
	}

	if pin := strings.TrimSpace(cfg.HostKeyFingerprint); pin != "" {
		return &hostKeyPolicy{callback: fingerprintCallback(pin), algorithms: preferredHostKeyAlgorithms()}, nil
	}

	path, err := knownHostsPath(cfg)
	if err != nil {
		return nil, err
	}
	check, err := knownhosts.New(path)
	if errors.Is(err, os.ErrNotExist) {
		return &hostKeyPolicy{callback: missingKnownHostsCallback(cfg, path), algorithms: preferredHostKeyAlgorithms()}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read SSH known_hosts file %s: %w", path, err)
	}

	algorithms := knownHostAlgorithms(check, dialAddress(cfg))
	if algorithms == nil {
		// Unknown host: the key it presents is the one the user is told to check and add.
		algorithms = preferredHostKeyAlgorithms()
	}
	return &hostKeyPolicy{
		callback:   knownHostsCallback(cfg, path, check),
		algorithms: algorithms,
	}, nil
}

// knownHostsPath is the configured known_hosts file, or ~/.ssh/known_hosts when unset.
func knownHostsPath(cfg config.SSHConfig) (string, error) {
	if cfg.KnownHostsPath != "" {
		return cfg.KnownHostsPath, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate ~/.ssh/known_hosts (set known_hosts_path): %w", err)
	}
	return filepath.Join(home, ".ssh", "known_hosts"), nil
}

func insecureCallback(hostname string, _ net.Addr, key ssh.PublicKey) error {
	logger.Warn("SSH host key verification is DISABLED for %s (insecure_ignore_host_key: true); accepting %s key %s without checking it",
		hostname, key.Type(), ssh.FingerprintSHA256(key))
	return nil
}

func fingerprintCallback(pin string) ssh.HostKeyCallback {
	return func(hostname string, _ net.Addr, key ssh.PublicKey) error {
		got := ssh.FingerprintSHA256(key)
		if got == pin {
			return nil
		}
		return fmt.Errorf("SSH host key for %s does NOT match host_key_fingerprint: the server presented %s key %s, expected %s. "+
			"host_key_fingerprint must be the fingerprint of the server's host key of the same type (%s): "+
			"if the pin was taken from a key of another type, replace it with the output of `ssh-keygen -lf %s` run on the server. "+
			"If it was taken from that %s key, this can mean someone is intercepting the connection; "+
			"confirm the server's key with its administrator before changing host_key_fingerprint",
			hostname, key.Type(), got, pin, key.Type(), hostKeyFile(key.Type()), key.Type())
	}
}

func missingKnownHostsCallback(cfg config.SSHConfig, path string) ssh.HostKeyCallback {
	return func(hostname string, _ net.Addr, key ssh.PublicKey) error {
		return fmt.Errorf("cannot verify the SSH host key of %s: the known_hosts file %s does not exist. "+
			"The server presented %s key %s. %s",
			hostname, path, key.Type(), ssh.FingerprintSHA256(key), trustInstructions(cfg, path, key))
	}
}

func knownHostsCallback(cfg config.SSHConfig, path string, check ssh.HostKeyCallback) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := check(hostname, remote, key)
		if err == nil {
			return nil
		}

		presented := fmt.Sprintf("%s key %s", key.Type(), ssh.FingerprintSHA256(key))

		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) {
			if len(keyErr.Want) == 0 {
				return fmt.Errorf("SSH host %s is not in %s; the server presented %s. %s",
					hostname, path, presented, trustInstructions(cfg, path, key))
			}
			known := make([]string, 0, len(keyErr.Want))
			for _, want := range keyErr.Want {
				known = append(known, fmt.Sprintf("%s key %s (%s:%d)",
					want.Key.Type(), ssh.FingerprintSHA256(want.Key), want.Filename, want.Line))
			}
			return fmt.Errorf("SSH host key for %s does NOT match the key recorded in %s. "+
				"This can mean someone is intercepting the connection (man-in-the-middle), or the server's host key was replaced. "+
				"The server presented %s; known: %s. Do not connect until the server's administrator has confirmed the new key; "+
				"only then replace the old entry in %s",
				hostname, path, presented, strings.Join(known, ", "), path)
		}

		var revokedErr *knownhosts.RevokedError
		if errors.As(err, &revokedErr) {
			return fmt.Errorf("SSH host key for %s is marked as revoked in %s (%s:%d); the server presented %s",
				hostname, path, revokedErr.Revoked.Filename, revokedErr.Revoked.Line, presented)
		}

		return fmt.Errorf("SSH host key check for %s against %s failed: %w", hostname, path, err)
	}
}

// trustInstructions names the two ways to make an unknown host trusted, after checking the
// presented key against the server's own copy of it.
func trustInstructions(cfg config.SSHConfig, path string, key ssh.PublicKey) string {
	return fmt.Sprintf("Check that fingerprint against `ssh-keygen -lf %s` run on the server (or ask its administrator), then either "+
		"add the host with `ssh-keyscan -p %d %s >> %s`, or set host_key_fingerprint to that fingerprint in the ssh config",
		hostKeyFile(key.Type()), cfg.Port, cfg.Host, path)
}

// hostKeyFile is where a stock OpenSSH server keeps its public host key of keyType.
func hostKeyFile(keyType string) string {
	name := strings.TrimPrefix(keyType, "ssh-")
	if strings.HasPrefix(keyType, "ecdsa-") {
		name = "ecdsa"
	}
	return "/etc/ssh/ssh_host_" + name + "_key.pub"
}

// preferredHostKeyAlgorithms is the handshake preference when known_hosts does not dictate
// the key type: ssh-ed25519 first, then the other plain host key algorithms the library
// supports, in its order. The library default puts Ed25519 last, so a stock OpenSSH server
// would present its ECDSA key, while `ssh` itself, ssh-keyscan's first line and the pin the
// docs suggest all use Ed25519. Certificate algorithms are left out: certificates are not
// verified here, and a certificate's fingerprint is not the one the user is told to compare.
// ssh-rsa (SHA-1) is appended last because the library default still offers it, for servers
// with only an RSA key and no rsa-sha2 support.
func preferredHostKeyAlgorithms() []string {
	algorithms := []string{ssh.KeyAlgoED25519}
	for _, algo := range ssh.SupportedAlgorithms().HostKeys {
		if algo != ssh.KeyAlgoED25519 && !isCertAlgorithm(algo) {
			algorithms = append(algorithms, algo)
		}
	}
	return append(algorithms, ssh.KeyAlgoRSA)
}

func isCertAlgorithm(algo string) bool {
	return strings.Contains(algo, "-cert-")
}

// knownHostAlgorithms returns the host key algorithms matching the key types known_hosts
// holds for address, or nil when it holds none. Without this restriction a server that
// prefers a different key type than the recorded one would present that key, and the
// check would report a mismatch instead of a match.
func knownHostAlgorithms(check ssh.HostKeyCallback, address string) []string {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil
	}
	probe, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil
	}

	// A throwaway key never matches, so the error lists every recorded key for address.
	// The hostname takes precedence over the remote address in the lookup.
	var keyErr *knownhosts.KeyError
	if err := check(address, &net.TCPAddr{IP: net.IPv4zero}, probe); !errors.As(err, &keyErr) {
		return nil
	}

	var algorithms []string
	seen := make(map[string]bool)
	for _, want := range keyErr.Want {
		for _, algo := range algorithmsForKeyType(want.Key.Type()) {
			if !seen[algo] {
				seen[algo] = true
				algorithms = append(algorithms, algo)
			}
		}
	}
	return algorithms
}

// algorithmsForKeyType maps a public key type to the handshake algorithms that use it. An
// RSA key can be presented with any of the three RSA signature algorithms.
func algorithmsForKeyType(keyType string) []string {
	if keyType == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	}
	return []string{keyType}
}
