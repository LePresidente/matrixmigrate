package ssh

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/aligundogdu/matrixmigrate/internal/config"
)

const testHost = "mm.example.com"

// fakeAddr stands in for the TCP address of the server the callback is checking.
var fakeAddr net.Addr = &net.TCPAddr{IP: net.IPv4(192, 0, 2, 10), Port: 22}

func newTestKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("wrap key: %v", err)
	}
	return key
}

// writeKnownHosts writes a known_hosts file holding key for host:port and returns its path.
func writeKnownHosts(t *testing.T, host string, port int, key ssh.PublicKey) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	line := knownhosts.Line([]string{addr}, key) + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	return path
}

func sshConfigFor(port int) config.SSHConfig {
	return config.SSHConfig{Host: testHost, Port: port, User: "alice"}
}

func checkHostKey(t *testing.T, cfg config.SSHConfig, key ssh.PublicKey) error {
	t.Helper()
	policy, err := newHostKeyPolicy(cfg)
	if err != nil {
		t.Fatalf("newHostKeyPolicy: %v", err)
	}
	return policy.callback(dialAddress(cfg), fakeAddr, key)
}

func TestHostKeyKnownHostAccepted(t *testing.T) {
	key := newTestKey(t)
	cfg := sshConfigFor(22)
	cfg.KnownHostsPath = writeKnownHosts(t, testHost, 22, key)

	if err := checkHostKey(t, cfg, key); err != nil {
		t.Fatalf("known host rejected: %v", err)
	}
}

func TestHostKeyKnownHostOnNonDefaultPort(t *testing.T) {
	key := newTestKey(t)
	cfg := sshConfigFor(2222)
	cfg.KnownHostsPath = writeKnownHosts(t, testHost, 2222, key)

	if err := checkHostKey(t, cfg, key); err != nil {
		t.Fatalf("known host on port 2222 rejected: %v", err)
	}
}

func TestHostKeyUnknownHostRejected(t *testing.T) {
	known := newTestKey(t)
	presented := newTestKey(t)
	cfg := sshConfigFor(22)
	cfg.KnownHostsPath = writeKnownHosts(t, "other.example.com", 22, known)

	err := checkHostKey(t, cfg, presented)
	if err == nil {
		t.Fatal("unknown host accepted")
	}
	msg := err.Error()
	for _, want := range []string{
		"is not in " + cfg.KnownHostsPath,
		ssh.FingerprintSHA256(presented),
		"ssh-ed25519",
		"ssh-keyscan -p 22 " + testHost + " >> " + cfg.KnownHostsPath,
		"host_key_fingerprint",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not contain %q:\n%s", want, msg)
		}
	}
}

func TestHostKeyChangedKeyRejected(t *testing.T) {
	known := newTestKey(t)
	presented := newTestKey(t)
	cfg := sshConfigFor(22)
	cfg.KnownHostsPath = writeKnownHosts(t, testHost, 22, known)

	err := checkHostKey(t, cfg, presented)
	if err == nil {
		t.Fatal("changed host key accepted")
	}
	msg := err.Error()
	for _, want := range []string{"does NOT match", "intercept", ssh.FingerprintSHA256(presented)} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not contain %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "insecure_ignore_host_key") {
		t.Errorf("mismatch error must not suggest the insecure flag:\n%s", msg)
	}
}

func TestHostKeyFingerprintPin(t *testing.T) {
	pinned := newTestKey(t)
	other := newTestKey(t)
	cfg := sshConfigFor(22)
	cfg.HostKeyFingerprint = ssh.FingerprintSHA256(pinned)
	// The pin takes precedence: a known_hosts file is neither needed nor consulted.
	cfg.KnownHostsPath = filepath.Join(t.TempDir(), "does-not-exist")

	if err := checkHostKey(t, cfg, pinned); err != nil {
		t.Fatalf("pinned key rejected: %v", err)
	}
	err := checkHostKey(t, cfg, other)
	if err == nil {
		t.Fatal("key not matching the pin accepted")
	}
	if !strings.Contains(err.Error(), ssh.FingerprintSHA256(other)) {
		t.Errorf("error does not name the presented fingerprint:\n%s", err)
	}
}

func TestHostKeyInsecureAcceptsAnything(t *testing.T) {
	cfg := sshConfigFor(22)
	cfg.InsecureIgnoreHostKey = true
	cfg.HostKeyFingerprint = "SHA256:not-this-one"
	cfg.KnownHostsPath = filepath.Join(t.TempDir(), "does-not-exist")

	if err := checkHostKey(t, cfg, newTestKey(t)); err != nil {
		t.Fatalf("insecure mode rejected a key: %v", err)
	}
}

func TestHostKeyMissingKnownHostsFile(t *testing.T) {
	cfg := sshConfigFor(22)
	cfg.KnownHostsPath = filepath.Join(t.TempDir(), "missing_known_hosts")
	presented := newTestKey(t)

	err := checkHostKey(t, cfg, presented)
	if err == nil {
		t.Fatal("missing known_hosts file accepted the host")
	}
	msg := err.Error()
	for _, want := range []string{
		cfg.KnownHostsPath + " does not exist",
		ssh.FingerprintSHA256(presented),
		"ssh-keyscan -p 22 " + testHost + " >> " + cfg.KnownHostsPath,
		"host_key_fingerprint",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not contain %q:\n%s", want, msg)
		}
	}
}

func TestHostKeyAlgorithmsFromKnownHosts(t *testing.T) {
	key := newTestKey(t)
	cfg := sshConfigFor(22)
	cfg.KnownHostsPath = writeKnownHosts(t, testHost, 22, key)

	policy, err := newHostKeyPolicy(cfg)
	if err != nil {
		t.Fatalf("newHostKeyPolicy: %v", err)
	}
	if want := []string{ssh.KeyAlgoED25519}; !reflect.DeepEqual(policy.algorithms, want) {
		t.Errorf("algorithms = %v, want %v", policy.algorithms, want)
	}
}

func TestHostKeyAlgorithmsForRSA(t *testing.T) {
	got := algorithmsForKeyType(ssh.KeyAlgoRSA)
	want := []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("algorithmsForKeyType(ssh-rsa) = %v, want %v", got, want)
	}
}

func TestNewClientConfigUsesHostKeyPolicy(t *testing.T) {
	known := newTestKey(t)
	cfg := sshConfigFor(22)
	cfg.KnownHostsPath = writeKnownHosts(t, testHost, 22, known)

	clientCfg, err := newClientConfig(cfg, "", "placeholder-password", time.Second)
	if err != nil {
		t.Fatalf("newClientConfig: %v", err)
	}
	if clientCfg.User != "alice" {
		t.Errorf("User = %q, want alice", clientCfg.User)
	}
	if want := []string{ssh.KeyAlgoED25519}; !reflect.DeepEqual(clientCfg.HostKeyAlgorithms, want) {
		t.Errorf("HostKeyAlgorithms = %v, want %v", clientCfg.HostKeyAlgorithms, want)
	}
	if err := clientCfg.HostKeyCallback(dialAddress(cfg), fakeAddr, known); err != nil {
		t.Errorf("known key rejected: %v", err)
	}
	if err := clientCfg.HostKeyCallback(dialAddress(cfg), fakeAddr, newTestKey(t)); err == nil {
		t.Error("unknown key accepted: the client config is not using the host key policy")
	}
}

func newECDSATestKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ecdsa key: %v", err)
	}
	key, err := ssh.NewPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("wrap ecdsa key: %v", err)
	}
	return key
}

// assertEd25519Preferred checks that ssh-ed25519 is offered first, that no certificate
// algorithm is offered, and that every plain host key algorithm the library supports is
// still offered.
func assertEd25519Preferred(t *testing.T, algorithms []string) {
	t.Helper()
	if len(algorithms) == 0 || algorithms[0] != ssh.KeyAlgoED25519 {
		t.Fatalf("algorithms = %v, want ssh-ed25519 first", algorithms)
	}
	offered := make(map[string]bool)
	for _, algo := range algorithms {
		if strings.Contains(algo, "-cert-") {
			t.Errorf("certificate algorithm %s offered", algo)
		}
		offered[algo] = true
	}
	for _, algo := range ssh.SupportedAlgorithms().HostKeys {
		if !strings.Contains(algo, "-cert-") && !offered[algo] {
			t.Errorf("supported algorithm %s dropped from %v", algo, algorithms)
		}
	}
}

func TestHostKeyPinPrefersEd25519(t *testing.T) {
	cfg := sshConfigFor(22)
	cfg.HostKeyFingerprint = ssh.FingerprintSHA256(newTestKey(t))

	policy, err := newHostKeyPolicy(cfg)
	if err != nil {
		t.Fatalf("newHostKeyPolicy: %v", err)
	}
	assertEd25519Preferred(t, policy.algorithms)
}

func TestHostKeyUnknownHostPrefersEd25519(t *testing.T) {
	cfg := sshConfigFor(22)
	cfg.KnownHostsPath = writeKnownHosts(t, "other.example.com", 22, newECDSATestKey(t))

	policy, err := newHostKeyPolicy(cfg)
	if err != nil {
		t.Fatalf("newHostKeyPolicy: %v", err)
	}
	assertEd25519Preferred(t, policy.algorithms)
}

func TestHostKeyMissingKnownHostsPrefersEd25519(t *testing.T) {
	cfg := sshConfigFor(22)
	cfg.KnownHostsPath = filepath.Join(t.TempDir(), "missing_known_hosts")

	policy, err := newHostKeyPolicy(cfg)
	if err != nil {
		t.Fatalf("newHostKeyPolicy: %v", err)
	}
	assertEd25519Preferred(t, policy.algorithms)
}

func TestHostKeyKnownECDSAKeepsECDSAOnly(t *testing.T) {
	cfg := sshConfigFor(22)
	cfg.KnownHostsPath = writeKnownHosts(t, testHost, 22, newECDSATestKey(t))

	policy, err := newHostKeyPolicy(cfg)
	if err != nil {
		t.Fatalf("newHostKeyPolicy: %v", err)
	}
	if want := []string{ssh.KeyAlgoECDSA256}; !reflect.DeepEqual(policy.algorithms, want) {
		t.Errorf("algorithms = %v, want %v", policy.algorithms, want)
	}
}

func TestHostKeyPinMismatchNamesPresentedType(t *testing.T) {
	cfg := sshConfigFor(22)
	cfg.HostKeyFingerprint = ssh.FingerprintSHA256(newECDSATestKey(t))

	err := checkHostKey(t, cfg, newTestKey(t))
	if err == nil {
		t.Fatal("key not matching the pin accepted")
	}
	msg := err.Error()
	for _, want := range []string{
		"ssh-ed25519",
		"same type",
		"/etc/ssh/ssh_host_ed25519_key.pub",
		"intercept",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not contain %q:\n%s", want, msg)
		}
	}
}
