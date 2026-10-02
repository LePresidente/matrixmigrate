package config

import (
	"strings"
	"testing"
)

func directModeConfig(baseURL string) *Config {
	c := &Config{}
	c.Matrix.Homeserver = "example.com"
	c.Matrix.Auth.Username = "alice"
	c.Matrix.Auth.PasswordEnv = "MATRIX_PASSWORD"
	c.Matrix.API.BaseURL = baseURL
	return c
}

func TestValidateRejectsCleartextHTTPToRemoteHost(t *testing.T) {
	err := directModeConfig("http://example.com:8008").Validate()
	if err == nil {
		t.Fatal("expected an error for http:// to a remote host")
	}
	for _, want := range []string{"matrix.api.base_url", "unencrypted", "matrix.allow_insecure_http"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestValidateAllowsCleartextHTTPWithOption(t *testing.T) {
	c := directModeConfig("http://example.com:8008")
	c.Matrix.AllowInsecureHTTP = true
	if err := c.Validate(); err != nil {
		t.Fatalf("allow_insecure_http should disable the check: %v", err)
	}
}

func TestValidateAllowsLoopbackAndHTTPS(t *testing.T) {
	for _, u := range []string{
		"http://localhost:8008", "http://127.0.0.1:8008", "http://[::1]:8008",
		"https://example.com", "",
	} {
		if err := directModeConfig(u).Validate(); err != nil {
			t.Errorf("base_url %q should be accepted: %v", u, err)
		}
	}
}

func TestValidateDoesNotCheckBaseURLInTunnelMode(t *testing.T) {
	c := directModeConfig("http://example.com:8008")
	c.Matrix.SSH.Host = "ssh.example.com"
	c.Matrix.SSH.User = "alice"
	c.Matrix.SSH.KeyPath = "/home/alice/.ssh/id_ed25519"
	if err := c.Validate(); err != nil {
		t.Fatalf("base_url is unused over a tunnel: %v", err)
	}
}

func masConfig(endpoint string) *Config {
	c := &Config{}
	c.Matrix.MAS = MASConfig{Enabled: true, Endpoint: endpoint, ClientIDEnv: "A", ClientSecretEnv: "B"}
	return c
}

func TestValidateMASEndpointCleartext(t *testing.T) {
	err := masConfig("http://mas.example.com:8080").Validate()
	if err == nil || !strings.Contains(err.Error(), "matrix.mas.endpoint") {
		t.Fatalf("expected matrix.mas.endpoint error, got %v", err)
	}
	c := masConfig("http://mas.example.com:8080")
	c.Matrix.AllowInsecureHTTP = true
	if err := c.Validate(); err != nil {
		t.Errorf("option should allow it: %v", err)
	}
	for _, u := range []string{"http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080", "https://mas.example.com"} {
		if err := masConfig(u).Validate(); err != nil {
			t.Errorf("endpoint %q should be accepted: %v", u, err)
		}
	}
	// Checked even when matrix.ssh is set.
	c = masConfig("http://mas.example.com:8080")
	c.Matrix.SSH.Host = "ssh.example.com"
	c.Matrix.SSH.User = "alice"
	c.Matrix.SSH.KeyPath = "/k"
	c.Matrix.Homeserver = "example.com"
	c.Matrix.Auth.Username = "alice"
	c.Matrix.Auth.PasswordEnv = "P"
	if err := c.Validate(); err == nil {
		t.Error("MAS endpoint must be checked in tunnel mode too")
	}
}

func TestResolveDBSSLModeIgnoresInvalidDiscovered(t *testing.T) {
	for _, d := range []string{"prefer", "disable host=x", "allow"} {
		if got := ResolveDBSSLMode("", d, "db.internal"); got != DBSSLModeRequire {
			t.Errorf("discovered %q: got %q, want require", d, got)
		}
		if got := ResolveDBSSLMode("", d, "127.0.0.1"); got != DBSSLModeDisable {
			t.Errorf("discovered %q on loopback: got %q, want disable", d, got)
		}
	}
	if got := ResolveDBSSLMode("", DBSSLModeVerifyFull, "127.0.0.1"); got != DBSSLModeVerifyFull {
		t.Errorf("valid discovered value must be used, got %q", got)
	}
}
