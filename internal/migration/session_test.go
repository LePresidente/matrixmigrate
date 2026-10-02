package migration

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/aligundogdu/matrixmigrate/internal/config"
	"github.com/aligundogdu/matrixmigrate/internal/matrix"
	"github.com/aligundogdu/matrixmigrate/internal/ssh"
)

func TestClassifyConnection(t *testing.T) {
	pingCalled := false
	if got := classifyConnection(false, func() error { pingCalled = true; return nil }); got != connAbsent {
		t.Errorf("no client: got %v, want connAbsent", got)
	}
	if pingCalled {
		t.Error("no client: ping must not be called")
	}
	if got := classifyConnection(true, func() error { return nil }); got != connLive {
		t.Errorf("ping ok: got %v, want connLive", got)
	}
	if got := classifyConnection(true, func() error { return errors.New("broken pipe") }); got != connStale {
		t.Errorf("ping failed: got %v, want connStale", got)
	}
}

// logoutRecorder is a homeserver that only answers /logout and records the tokens it was
// called with.
type logoutRecorder struct {
	mu     sync.Mutex
	tokens []string
	status int
}

func (l *logoutRecorder) handler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.URL.Path == "/_matrix/client/v3/logout" {
		l.mu.Lock()
		l.tokens = append(l.tokens, r.Header.Get("Authorization"))
		l.mu.Unlock()
		w.WriteHeader(l.status)
		_, _ = w.Write([]byte("{}"))
		return
	}
	http.NotFound(w, r)
}

func (l *logoutRecorder) calls() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.tokens...)
}

func TestCloseLogsOutLoginSession(t *testing.T) {
	rec := &logoutRecorder{status: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	o := &Orchestrator{tunnelManager: ssh.NewTunnelManager()}
	o.setLoginSession(srv.URL, "login-token")

	if err := o.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := rec.calls(); len(got) != 1 || got[0] != "Bearer login-token" {
		t.Fatalf("logout calls = %v, want one with the login token", got)
	}

	// A second Close must not log out again.
	if err := o.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := rec.calls(); len(got) != 1 {
		t.Fatalf("logout calls after second Close = %v, want still one", got)
	}
}

func TestCloseLogoutFailureIsNotReturned(t *testing.T) {
	rec := &logoutRecorder{status: http.StatusInternalServerError}
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	o := &Orchestrator{tunnelManager: ssh.NewTunnelManager()}
	o.setLoginSession(srv.URL, "login-token")

	if err := o.Close(); err != nil {
		t.Fatalf("Close returned the logout failure: %v", err)
	}
	if got := rec.calls(); len(got) != 1 {
		t.Fatalf("logout calls = %v, want one", got)
	}
}

func TestCloseWithoutLoginDoesNotLogOut(t *testing.T) {
	o := &Orchestrator{tunnelManager: ssh.NewTunnelManager()}
	// An admin token from the environment is not a session this tool opened.
	if err := o.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// A ConnectMatrix that logs in and then fails must leave no client behind and must revoke the
// session it opened, so a later Close has nothing to log out and a retry starts clean.
func TestConnectMatrixFailurePartWayLeavesNothingBehind(t *testing.T) {
	var mu sync.Mutex
	var logouts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/client/v3/login":
			_, _ = w.Write([]byte(`{"user_id":"@alice:example.com","access_token":"login-token","device_id":"matrixmigrate"}`))
		case "/_matrix/client/v3/logout":
			mu.Lock()
			logouts = append(logouts, r.Header.Get("Authorization"))
			mu.Unlock()
			_, _ = w.Write([]byte("{}"))
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN_TOKEN","error":"Invalid access token"}`))
		}
	}))
	defer srv.Close()

	t.Setenv("MM_TEST_MATRIX_PASSWORD", "secret")
	cfg := &config.Config{}
	cfg.Data.StateFile = filepath.Join(t.TempDir(), "state.json")
	cfg.Matrix.Homeserver = "example.com"
	cfg.Matrix.API.BaseURL = srv.URL
	cfg.Matrix.Auth.Username = "alice"
	cfg.Matrix.Auth.PasswordEnv = "MM_TEST_MATRIX_PASSWORD"
	cfg.Matrix.RateLimit.MaxRetries = 0

	o := &Orchestrator{config: cfg, state: NewMigrationState(), tunnelManager: ssh.NewTunnelManager()}
	if err := o.ConnectMatrix(); err == nil {
		t.Fatal("ConnectMatrix succeeded against a server that rejects the token")
	}
	if o.mxClient != nil {
		t.Error("mxClient is set after a failed connect")
	}
	if o.mxToken != "" || o.mxLoginBaseURL != "" {
		t.Errorf("login session kept after a failed connect: token=%q url=%q", o.mxToken, o.mxLoginBaseURL)
	}
	mu.Lock()
	got := append([]string(nil), logouts...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "Bearer login-token" {
		t.Errorf("logouts = %v, want the session revoked once", got)
	}
}

// A second ConnectMatrix in the same session reuses the client it already holds instead of
// opening another tunnel or logging in again.
func TestConnectMatrixReusesExistingClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("ConnectMatrix contacted the server (%s) although a client was already set", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := &config.Config{}
	cfg.Matrix.API.BaseURL = srv.URL
	existing := matrix.NewClient(srv.URL, "admin-token", "example.com")
	o := &Orchestrator{config: cfg, state: NewMigrationState(), tunnelManager: ssh.NewTunnelManager(), mxClient: existing}

	if err := o.ConnectMatrix(); err != nil {
		t.Fatalf("ConnectMatrix: %v", err)
	}
	if o.mxClient != existing {
		t.Error("ConnectMatrix replaced the existing client")
	}
}
