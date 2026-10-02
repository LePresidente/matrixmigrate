package migration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aligundogdu/matrixmigrate/internal/config"
	"github.com/aligundogdu/matrixmigrate/internal/logger"
	"github.com/aligundogdu/matrixmigrate/internal/matrix"
	"github.com/aligundogdu/matrixmigrate/internal/mattermost"
	"github.com/aligundogdu/matrixmigrate/internal/ssh"
	"github.com/aligundogdu/matrixmigrate/pkg/archive"
)

// Orchestrator manages the migration process
type Orchestrator struct {
	config        *config.Config
	state         *MigrationState
	tunnelManager *ssh.TunnelManager

	mmClient  *mattermost.Client
	mxClient  *matrix.Client
	masClient *matrix.MASClient // set only when MAS is enabled

	// mxToken is the access token of a session this tool opened with a username/password
	// login, and mxLoginBaseURL the API address the login used. Close revokes it. Both stay
	// empty when the token is the configured admin token.
	mxToken        string
	mxLoginBaseURL string

	// forceMembershipReplay re-applies channel/team memberships even when the step already
	// completed, so members who joined after the first run get added on a later run.
	forceMembershipReplay bool

	// ctx is cancelled when the user interrupts the run. See SetContext.
	ctx context.Context
}

// ErrInterrupted marks a step that stopped early because the run was interrupted. The work
// done up to that point has been saved, so running the same step again resumes it.
var ErrInterrupted = errors.New("interrupted")

// SetContext installs the context whose cancellation interrupts the running step. Import
// steps stop after the item in flight, save their progress, and return an error wrapping
// ErrInterrupted. Without a call the context is context.Background() and nothing interrupts.
func (o *Orchestrator) SetContext(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	o.ctx = ctx
	if o.mxClient != nil {
		o.mxClient.SetContext(ctx)
	}
}

// interrupted reports whether the context installed with SetContext has been cancelled.
func (o *Orchestrator) interrupted() bool {
	return o.ctx != nil && o.ctx.Err() != nil
}

// failInterrupted records step as failed with err, which must wrap ErrInterrupted, and returns
// err for the caller to hand back.
func (o *Orchestrator) failInterrupted(step StepName, err error) error {
	logger.Warn("%v", err)
	o.state.FailStep(step, err)
	if serr := o.SaveState(); serr != nil {
		logger.Error("Failed to save state after interrupt: %v", serr)
	}
	return err
}

// NewOrchestrator creates a new migration orchestrator
func NewOrchestrator(cfg *config.Config) (*Orchestrator, error) {
	// Initialize logger
	if err := logger.Init(cfg.Data.AssetsDir); err != nil {
		// Non-fatal, continue without logging
	}
	logger.SetDebug(cfg.Debug)
	if cfg.Debug {
		logger.Info("Debug logging enabled")
	}

	// Load or create state
	state, err := LoadState(cfg.Data.StateFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load state: %w", err)
	}

	return &Orchestrator{
		config:        cfg,
		state:         state,
		tunnelManager: ssh.NewTunnelManager(),
		ctx:           context.Background(),
	}, nil
}

// Close ends the session: it logs out a Matrix session this tool opened with a password
// login, then closes the database connection and every SSH tunnel. Calling it again is safe.
func (o *Orchestrator) Close() error {
	// The logout has to go out before the tunnel it may travel through is closed.
	o.endLoginSession()
	if o.mmClient != nil {
		o.mmClient.Close()
		o.mmClient = nil
	}
	o.mxClient = nil
	err := o.tunnelManager.CloseAll()
	logger.Close()
	return err
}

// SetForceMembershipReplay controls whether a completed membership step is re-applied on
// re-run (to pick up members added since the first run). Force-join is idempotent.
func (o *Orchestrator) SetForceMembershipReplay(force bool) {
	o.forceMembershipReplay = force
}

// setLoginSession records a session opened by a password login so Close can revoke it.
func (o *Orchestrator) setLoginSession(baseURL, token string) {
	o.mxLoginBaseURL = baseURL
	o.mxToken = token
}

// endLoginSession revokes the session recorded by setLoginSession, if any. A failure is
// logged, not returned: the session expires on its own and nothing else depends on it.
func (o *Orchestrator) endLoginSession() {
	if o.mxToken == "" {
		return
	}
	if err := matrix.Logout(o.mxLoginBaseURL, o.mxToken); err != nil {
		logger.Warn("Could not log out the Matrix session: %v", err)
	} else {
		logger.Info("Logged out the Matrix session")
	}
	o.mxToken = ""
	o.mxLoginBaseURL = ""
}

// connectionState is what a Connect call finds already in place.
type connectionState int

const (
	connAbsent connectionState = iota // no client: connect from scratch
	connLive                          // a client that still answers: reuse it
	connStale                         // a client that no longer answers: discard and reconnect
)

// classifyConnection decides what to do with an existing client. ping is called only when
// there is a client.
func classifyConnection(hasClient bool, ping func() error) connectionState {
	if !hasClient {
		return connAbsent
	}
	if err := ping(); err != nil {
		return connStale
	}
	return connLive
}

// waitForTunnel waits for the SSH tunnel to be ready by making HTTP requests
func (o *Orchestrator) waitForTunnel(baseURL string, timeout time.Duration) error {
	client := &http.Client{
		Timeout: 2 * time.Second,
	}

	deadline := time.Now().Add(timeout)
	var lastErr error

	for time.Now().Before(deadline) {
		// Try to connect to the Matrix server's version endpoint
		resp, err := client.Get(baseURL + "/_matrix/client/versions")
		if err == nil {
			resp.Body.Close()
			logger.Info("SSH tunnel to Matrix API is ready")
			return nil
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("timeout waiting for tunnel: %w", lastErr)
}

// GetState returns the current migration state
func (o *Orchestrator) GetState() *MigrationState {
	return o.state
}

// SaveState saves the current state
func (o *Orchestrator) SaveState() error {
	return SaveState(o.state, o.config.Data.StateFile)
}

// ProgressCallback is called to report progress during operations
type ProgressCallback func(stage string, current, total int, item string)

// OperationResult holds the result of an operation with statistics
type OperationResult struct {
	// Export stats
	UsersExported    int
	TeamsExported    int
	ChannelsExported int

	// Import stats
	UsersCreated  int
	UsersSkipped  int
	UsersFailed   int
	SpacesCreated int
	SpacesSkipped int
	SpacesFailed  int
	RoomsCreated  int
	RoomsSkipped  int
	RoomsFailed   int
	RoomsLinked   int

	// RoomsLinkFailed counts rooms that could not be added to their parent space.
	RoomsLinkFailed int

	// Membership stats
	TeamMembershipsExported    int
	ChannelMembershipsExported int
	MembersAdded               int
	MembersSkipped             int
	MembersFailed              int

	// Leave-rooms stats
	RoomsLeft        int
	RoomsLeaveSkip   int
	RoomsLeaveFailed int

	// Leave-rooms removal sweeps: deactivated accounts, then the migration's own AS bot
	DeactivatedAccounts    int
	DeactivatedRoomsLeft   int
	DeactivatedRoomsKept   int
	DeactivatedRoomsFailed int
	BotRoomsLeft           int
	BotRoomsKept           int
	BotRoomsFailed         int

	// Output file
	OutputFile string
}

// newImporter builds an Importer already carrying the settings every step shares, so a new
// step cannot quietly get the defaults instead of the configuration.
func (o *Orchestrator) newImporter() *matrix.Importer {
	importer := matrix.NewImporter(o.mxClient)
	importer.SetDeletedUserMode(o.config.GetDeletedUserMode())
	importer.SetContext(o.ctx)
	return importer
}

// ConnectMattermost establishes connection to Mattermost. It is idempotent: a connection
// that still answers is kept, and one that does not is closed, with its tunnel, and replaced.
func (o *Orchestrator) ConnectMattermost() error {
	switch classifyConnection(o.mmClient != nil, func() error { return o.mmClient.Ping() }) {
	case connLive:
		return nil
	case connStale:
		logger.Warn("Mattermost database connection no longer answers; reconnecting")
		o.mmClient.Close()
		o.mmClient = nil
		o.tunnelManager.CloseTunnel("mattermost")
	}

	cfg := o.config.Mattermost
	passphrase := o.config.GetSSHKeyPassphrase("mattermost")
	sshPassword := o.config.GetSSHPassword("mattermost")

	// Get database credentials
	var dbHost string
	var dbPort int
	var dbUser string
	var dbPassword string
	var dbName string
	// sslmode as discovered from Mattermost's own DataSource; "" means it said nothing.
	var dbSSLMode string

	// Direct mode: no ssh.host means the database is reachable from here, so the
	// credentials cannot come from a config.json read over SSH.
	direct := cfg.SSH.Host == ""

	if o.config.HasManualDatabaseConfig() {
		// Use manual config
		dbHost = cfg.Database.Host
		dbPort = cfg.Database.Port
		dbUser = cfg.Database.User
		dbPassword = o.config.GetMattermostDBPassword()
		dbName = cfg.Database.Name
	} else if direct {
		// Read from a config.json on this machine
		creds, err := mattermost.GetDatabaseCredentialsLocal(cfg.ConfigPath)
		if err != nil {
			return fmt.Errorf("failed to read database credentials from local Mattermost config: %w", err)
		}
		dbHost = creds.Host
		dbPort = creds.Port
		dbUser = creds.User
		dbPassword = creds.Password
		dbName = creds.Database
		// Direct mode is the only mode whose connection goes where Mattermost's own goes,
		// so it is the only one where inheriting Mattermost's sslmode is meaningful. Over a
		// tunnel the connection terminates at 127.0.0.1, where "verify-full" could not match
		// the certificate anyway.
		dbSSLMode = creds.SSLMode
	} else {
		// Read from Mattermost config.json via SSH
		creds, err := mattermost.GetDatabaseCredentials(cfg.SSH, passphrase, sshPassword, cfg.ConfigPath)
		if err != nil {
			return fmt.Errorf("failed to read database credentials from Mattermost config: %w", err)
		}
		dbHost = creds.Host
		dbPort = creds.Port
		dbUser = creds.User
		dbPassword = creds.Password
		dbName = creds.Database
	}

	// In direct mode the DSN points at the database itself; otherwise at the local end
	// of an SSH tunnel.
	connHost, connPort := dbHost, dbPort

	if direct {
		logger.Info("Connecting directly to Mattermost database at %s:%d", dbHost, dbPort)
	} else {
		// Create SSH tunnel to database. LocalPort 0 lets the tunnel bind a free port itself;
		// the address comes from the tunnel returned, which may be an existing one.
		tunnelCfg := ssh.TunnelConfig{
			SSHConfig:  cfg.SSH,
			LocalPort:  0,
			RemoteHost: dbHost,
			RemotePort: dbPort,
			Passphrase: passphrase,
			Password:   sshPassword,
		}

		tunnel, err := o.tunnelManager.CreateTunnel("mattermost", tunnelCfg)
		if err != nil {
			return fmt.Errorf("failed to create SSH tunnel: %w", err)
		}

		connHost, connPort = "127.0.0.1", tunnel.LocalPort()
	}

	sslMode := config.ResolveDBSSLMode(cfg.Database.SSLMode, dbSSLMode, connHost)
	if sslMode != config.DBSSLModeDisable {
		logger.Info("Connecting to the Mattermost database with sslmode=%s", sslMode)
	}

	dsn := buildPostgresDSN(connHost, connPort, dbUser, dbPassword, dbName, sslMode)

	// Connect to database
	client, err := mattermost.NewClient(dsn)
	if err != nil {
		if !direct {
			o.tunnelManager.CloseTunnel("mattermost")
		}
		return fmt.Errorf("failed to connect to database: %w", err)
	}

	o.mmClient = client
	if direct {
		o.state.MattermostHost = fmt.Sprintf("%s:%d", dbHost, dbPort)
	} else {
		o.state.MattermostHost = cfg.SSH.Host
	}
	return nil
}

// ConnectMatrix establishes connection to Matrix. It is idempotent: once a client is set,
// later calls return at once rather than open another tunnel, log in again or re-verify. A
// call that fails part-way closes the tunnel it opened and revokes a session it logged in.
func (o *Orchestrator) ConnectMatrix() (err error) {
	if o.mxClient != nil {
		return nil
	}

	cfg := o.config.Matrix

	// Direct mode: no ssh.host means the Matrix API is reachable from here, so talk to
	// matrix.api.base_url instead of forwarding a port. This is the normal case for a
	// homeserver behind an HTTPS ingress, where nothing listens on 127.0.0.1:8008.
	direct := cfg.SSH.Host == ""

	var baseURL string

	// A connect that fails part-way leaves nothing behind: the session it logged in is
	// revoked while the tunnel it travels through is still open, then the tunnel is closed.
	defer func() {
		if err == nil {
			return
		}
		o.endLoginSession()
		o.masClient = nil
		if !direct {
			o.tunnelManager.CloseTunnel("matrix")
		}
	}()

	if direct {
		baseURL = o.config.MatrixAPIURL()
		logger.Info("Connecting directly to Matrix API at %s", baseURL)
	} else {
		passphrase := o.config.GetSSHKeyPassphrase("matrix")
		sshPassword := o.config.GetSSHPassword("matrix")

		// Get remote API port from config (default: 8008)
		remotePort := cfg.API.Port
		if remotePort == 0 {
			remotePort = 8008
		}

		// Create SSH tunnel to Matrix API. LocalPort 0 lets the tunnel bind a free port
		// itself; the address comes from the tunnel returned, which may be an existing one.
		tunnelCfg := ssh.TunnelConfig{
			SSHConfig:  cfg.SSH,
			LocalPort:  0,
			RemoteHost: "127.0.0.1",
			RemotePort: remotePort,
			Passphrase: passphrase,
			Password:   sshPassword,
		}

		tunnel, err := o.tunnelManager.CreateTunnel("matrix", tunnelCfg)
		if err != nil {
			return fmt.Errorf("failed to create SSH tunnel: %w", err)
		}
		logger.Info("SSH tunnel to Matrix API: %s -> remote:127.0.0.1:%d", tunnel.LocalAddr(), remotePort)

		// Use local tunnel URL
		baseURL = "http://" + tunnel.LocalAddr()

		// Wait a moment for the tunnel to be ready
		time.Sleep(500 * time.Millisecond)

		// Verify tunnel is working by attempting a simple HTTP request
		if err := o.waitForTunnel(baseURL, 5*time.Second); err != nil {
			return fmt.Errorf("SSH tunnel to Matrix API is not responding on port %d: %w (is Synapse running and listening on port %d?)", remotePort, err, remotePort)
		}
	}

	// Get access token (either from config or via login)
	var accessToken string

	if o.config.UseTokenAuth() {
		// Use provided admin token
		accessToken = o.config.GetMatrixAdminToken()
	} else {
		// Login with username/password
		password := o.config.GetMatrixPassword()
		if password == "" {
			return fmt.Errorf("Matrix password not found in environment variable %s", cfg.Auth.PasswordEnv)
		}

		loginResp, err := matrix.Login(baseURL, cfg.Auth.Username, password)
		if err != nil {
			return fmt.Errorf("failed to login to Matrix: %w", err)
		}
		accessToken = loginResp.AccessToken
		o.setLoginSession(baseURL, accessToken)
	}

	// Create Matrix client with rate limiting from config
	rlConfig := matrix.RateLimitConfig{
		RequestsPerSecond: cfg.RateLimit.RequestsPerSecond,
		MaxRetries:        cfg.RateLimit.MaxRetries,
		RetryBaseDelay:    time.Duration(cfg.RateLimit.RetryBaseDelay) * time.Millisecond,
	}
	client := matrix.NewClientWithRateLimit(baseURL, accessToken, cfg.Homeserver, rlConfig)
	client.SetContext(o.ctx)

	// Test connection
	if err := client.TestConnection(); err != nil {
		return fmt.Errorf("failed to connect to Matrix API: %w", err)
	}

	// Auto-detect homeserver from authenticated user
	detectedHomeserver, err := client.DetectHomeserver()
	if err != nil {
		logger.Warn("Could not auto-detect homeserver: %v, using configured value: %s", err, cfg.Homeserver)
	} else if detectedHomeserver != cfg.Homeserver {
		logger.Info("Auto-detected homeserver '%s' differs from configured '%s', using detected value",
			detectedHomeserver, cfg.Homeserver)
		client.SetHomeserver(detectedHomeserver)
	}

	// When MAS is enabled, use it for user creation so users can log in via SSO/OAuth
	if o.config.Matrix.MAS.Enabled {
		clientID := o.config.GetMASClientID()
		clientSecret := o.config.GetMASClientSecret()
		if clientID == "" || clientSecret == "" {
			return fmt.Errorf("matrix.mas is enabled but %s and/or %s are not set",
				o.config.Matrix.MAS.ClientIDEnv, o.config.Matrix.MAS.ClientSecretEnv)
		}
		homeserver := client.GetHomeserver()
		masClient := matrix.NewMASClient(
			o.config.Matrix.MAS.Endpoint,
			clientID,
			clientSecret,
			homeserver,
		)
		client.SetMASClient(masClient)
		o.masClient = masClient
		logger.Info("Matrix Authentication Service enabled for user creation")
	}

	// Set AS token early so room/space creation can use it to create as the actual owner (creator)
	if o.config.UseAppService() {
		client.SetASToken(o.config.GetASToken())
		logger.Info("Application Service token set for room creator and message import")
	}

	// Verify every configured credential before any step can write to the homeserver.
	// A credential that is present but not accepted does not fail cleanly later: room
	// creation degrades to the admin user, and a room's creator cannot be changed
	// afterwards, so a partial run leaves permanently mis-owned rooms behind.
	if err := o.verifyCredentials(client); err != nil {
		return err
	}

	// Force-join: add users to rooms/spaces via Synapse admin API (no invite to accept)
	client.SetForceJoin(o.config.Matrix.Import.ForceJoin)
	if o.config.Matrix.Import.ForceJoin {
		logger.Info("Force-join enabled: users will be added to rooms/spaces directly (no invite acceptance required)")
	}

	o.mxClient = client
	if direct {
		o.state.MatrixHost = baseURL
	} else {
		o.state.MatrixHost = cfg.SSH.Host
	}
	return nil
}

// verifyCredentials checks each configured Matrix credential against the live server and
// reports every failure at once, so a run stops before it writes anything rather than
// degrading part-way through. The admin token is already covered by TestConnection.
func (o *Orchestrator) verifyCredentials(client *matrix.Client) error {
	var problems []string

	if o.config.UseAppService() {
		userID, err := client.VerifyASToken()
		if err != nil {
			problems = append(problems, fmt.Sprintf(
				"application service token (%s) rejected by homeserver: %v — check as_token in the registration file matches, and that Synapse loaded it (app_service_config_files)",
				o.config.Matrix.AppService.ASTokenEnv, err))
		} else {
			logger.Info("Preflight: application service token OK (authenticates as %s)", userID)
		}
	}

	if o.masClient != nil {
		if err := o.masClient.VerifyCredentials(); err != nil {
			problems = append(problems, fmt.Sprintf(
				"MAS client credentials (%s/%s) rejected by %s: %v",
				o.config.Matrix.MAS.ClientIDEnv, o.config.Matrix.MAS.ClientSecretEnv,
				o.config.Matrix.MAS.Endpoint, err))
		} else {
			logger.Info("Preflight: MAS client credentials OK")
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("credential preflight failed:\n  - %s", strings.Join(problems, "\n  - "))
	}

	logger.Info("Preflight: all configured Matrix credentials verified")
	return nil
}

// ExportAssets exports assets from Mattermost
func (o *Orchestrator) ExportAssets(progress ProgressCallback) (*OperationResult, error) {
	result := &OperationResult{}

	if o.mmClient == nil {
		return nil, fmt.Errorf("not connected to Mattermost")
	}

	// Check if we can run this step
	canRun, reason := o.state.CanRunStep(StepExportAssets)
	if !canRun {
		return nil, fmt.Errorf("cannot run step: %s", reason)
	}

	// Start step
	o.state.StartStep(StepExportAssets)
	if err := o.SaveState(); err != nil {
		return nil, err
	}

	// Create exporter
	exporter := mattermost.NewExporter(o.mmClient)

	// Export callback
	var exportProgress mattermost.ExportProgressCallback
	if progress != nil {
		exportProgress = func(stage string, current, total int) {
			progress(stage, current, total, "")
			o.state.UpdateStepProgress(StepExportAssets, current, total)
		}
	}

	// Export assets (include direct message channels when import_direct_messages is enabled)
	includeDirectMessages := o.config.Matrix.Import.ImportDirectMessages
	if includeDirectMessages {
		logger.Info("Export assets: import_direct_messages is enabled; will export D type channels for DM import")
	}
	assets, err := exporter.ExportAssets(exportProgress, includeDirectMessages)
	if err != nil {
		o.state.FailStep(StepExportAssets, err)
		o.SaveState()
		return nil, fmt.Errorf("export failed: %w", err)
	}

	// Filter to active assets only
	assets = mattermost.FilterActiveAssets(assets)

	// Optionally skip configured Mattermost users (e.g., bot/service accounts)
	if len(o.config.Mattermost.IgnoredUsers) > 0 {
		before := len(assets.Users)
		assets = mattermost.FilterIgnoredUsersFromAssets(assets, o.config.Mattermost.IgnoredUsers)
		ignored := before - len(assets.Users)
		if ignored > 0 {
			logger.Info("Export assets: ignored %d users via mattermost.ignored_users", ignored)
		}
	}

	// Count exported items
	result.UsersExported = len(assets.Users)
	result.TeamsExported = len(assets.Teams)
	result.ChannelsExported = len(assets.Channels) + len(assets.DirectChannels)

	// Generate filename
	timestamp := time.Now().Format("20060102-150405")
	filename := fmt.Sprintf("mattermost-assets-%s.json.gz", timestamp)
	filepath := o.config.Data.AssetsDir + "/" + filename

	// Save to gzipped JSON
	if err := archive.SaveGzipJSON(filepath, assets); err != nil {
		o.state.FailStep(StepExportAssets, err)
		o.SaveState()
		return nil, fmt.Errorf("failed to save assets: %w", err)
	}

	// Complete step
	o.state.CompleteStep(StepExportAssets, filepath)
	result.OutputFile = filepath
	return result, o.SaveState()
}

// loadExistingAssetMappings returns the union of the mapping file recorded in state (may be
// empty) and the newest asset-mapping file in dir, the newer one winning on conflict. It
// returns nil when neither can be loaded.
func loadExistingAssetMappings(recordedFile, dir string) *matrix.ExistingMappings {
	var sources []*Mapping
	recorded := ""
	if recordedFile != "" {
		if m, err := LoadMapping(recordedFile); err == nil {
			sources = append(sources, m)
			recorded = recordedFile
		}
	}
	if latest, _ := GetLatestMappingFile(dir); latest != "" && latest != recorded {
		if m, err := LoadMapping(latest); err == nil {
			sources = append(sources, m)
		}
	}
	if len(sources) == 0 {
		return nil
	}
	out := &matrix.ExistingMappings{
		Users:  make(map[string]string),
		Spaces: make(map[string]string),
		Rooms:  make(map[string]string),
	}
	for _, m := range sources {
		for k, v := range m.Users {
			out.Users[k] = v
		}
		for k, v := range m.Teams {
			out.Spaces[k] = v
		}
		for k, v := range m.Channels {
			out.Rooms[k] = v
		}
	}
	return out
}

// ImportAssets imports assets to Matrix
func (o *Orchestrator) ImportAssets(progress ProgressCallback) (*OperationResult, error) {
	result := &OperationResult{}

	if o.mxClient == nil {
		return nil, fmt.Errorf("not connected to Matrix")
	}

	// Check if we can run this step
	canRun, reason := o.state.CanRunStep(StepImportAssets)
	if !canRun {
		return nil, fmt.Errorf("cannot run step: %s", reason)
	}

	// Get the asset file from previous step
	assetFile := o.state.GetStepOutputFile(StepExportAssets)
	if assetFile == "" {
		return nil, fmt.Errorf("no asset file found from export step")
	}

	// Start step
	o.state.StartStep(StepImportAssets)
	if err := o.SaveState(); err != nil {
		return nil, err
	}

	// Load assets
	var assets mattermost.Assets
	if err := archive.LoadGzipJSON(assetFile, &assets); err != nil {
		o.state.FailStep(StepImportAssets, err)
		o.SaveState()
		return nil, fmt.Errorf("failed to load assets: %w", err)
	}

	// Optionally skip configured Mattermost users before import.
	if len(o.config.Mattermost.IgnoredUsers) > 0 {
		before := len(assets.Users)
		filtered := mattermost.FilterIgnoredUsersFromAssets(&assets, o.config.Mattermost.IgnoredUsers)
		assets = *filtered
		ignored := before - len(assets.Users)
		if ignored > 0 {
			logger.Info("Import assets: ignored %d users via mattermost.ignored_users", ignored)
		}
	}

	// Existing mappings let a re-run skip what was already created: the union of the file the
	// last completed run recorded and the newest mapping on disk, which may be a checkpoint
	// from an interrupted run that never reached the state file.
	existingMappings := loadExistingAssetMappings(o.state.GetStepOutputFile(StepImportAssets), o.config.Data.MappingsDir)

	// One file name for the whole run: the checkpoints and the final save all write it, so a
	// crash leaves the latest state in the newest asset-mapping file.
	mappingFile := GenerateMappingFilename(o.config.Data.MappingsDir)
	homeserver := o.mxClient.GetHomeserver()
	saveAssetMapping := func(users, spaces, rooms map[string]string) error {
		m := NewMapping(homeserver)
		m.MergeUsers(users)
		m.MergeTeams(spaces)
		m.MergeChannels(rooms)
		return SaveMapping(m, mappingFile)
	}

	// Build room import options from config.
	// Space visibility is always applied; owner/alias options are applied when preserve_owner_and_alias is enabled.
	roomOpts := &matrix.RoomImportOptions{
		SpaceVisibility: o.config.GetSpaceVisibility(),
	}
	if o.config.Matrix.Import.PreserveOwnerAndAlias {
		logger.Info("Room/space import: preserve_owner_and_alias is enabled; will set alias (team+name) and owner from creator_id or fallback")
		adminUserID := o.config.FormatUserID(o.config.Matrix.Auth.Username)
		if adminUserID == "" || o.config.Matrix.Auth.Username == "" {
			if who, err := o.mxClient.WhoAmI(); err == nil && who != nil {
				adminUserID = who.UserID
				logger.Info("Room/space import: admin user ID from WhoAmI: %s", adminUserID)
			}
			if adminUserID == "" {
				adminUserID = o.config.FormatUserID("admin")
				logger.Warn("Room/space import: could not get admin user ID, using fallback @admin:%s", o.config.Matrix.Homeserver)
			}
		}
		roomOpts.PreserveOwnerAndAlias = true
		roomOpts.FallbackCreator = o.config.Matrix.Import.FallbackRoomCreator
		roomOpts.AdminUserID = adminUserID
		if roomOpts.FallbackCreator == "" {
			roomOpts.FallbackCreator = o.config.Matrix.Auth.Username
		}
		logger.Info("Room/space import: fallback_room_creator=%q, admin_user_id=%s", roomOpts.FallbackCreator, roomOpts.AdminUserID)
		if o.mxClient.HasASToken() {
			logger.Info("Room/space import: Application Service token is set; rooms/spaces will be created with creator = owner (actual user)")
		} else {
			logger.Warn("Room/space import: no Application Service token; rooms will be created by admin then owner will be invited and granted power level 100 (creator will remain admin)")
		}
	}

	// Create importer
	importer := o.newImporter()

	// Resolve how new users get a password ("auto" -> none when MAS handles authentication).
	passwordMode := matrix.PasswordModeRandom
	switch o.config.GetUserPasswordMode() {
	case config.UserPasswordModeNone:
		passwordMode = matrix.PasswordModeNone
	case config.UserPasswordModeLocalOnly:
		passwordMode = matrix.PasswordModeLocalOnly
	}
	importer.SetPasswordPolicy(matrix.PasswordPolicy{
		Mode:   passwordMode,
		Length: o.config.GetUserPasswordLength(),
	})
	switch passwordMode {
	case matrix.PasswordModeNone:
		logger.Info("User import: creating users without a password (SSO/MAS or admin reset required)")
	case matrix.PasswordModeLocalOnly:
		logger.Info("User import: generating a random %d-character password only for users without SSO in Mattermost; everyone else signs in through the upstream provider",
			o.config.GetUserPasswordLength())
		if o.config.Matrix.MAS.Enabled {
			logger.Warn("User import: user_password.mode is %q with MAS enabled - password login must be enabled in MAS (passwords.enabled: true), otherwise set-password returns 403 and those accounts end up unreachable",
				config.UserPasswordModeLocalOnly)
		}
	default:
		logger.Info("User import: generating a random %d-character password per user", o.config.GetUserPasswordLength())
	}

	importer.SetAssetCheckpoint(func(users, spaces, rooms map[string]string) {
		if err := saveAssetMapping(users, spaces, rooms); err != nil {
			logger.Warn("Could not checkpoint the asset mapping to %s: %v", mappingFile, err)
		}
	})

	// Import callback
	var importProgress matrix.ImportProgressCallback
	if progress != nil {
		importProgress = func(stage string, current, total int, item string) {
			progress(stage, current, total, item)
			o.state.UpdateStepProgress(StepImportAssets, current, total)
		}
	}

	// Import assets (passing existing mappings to skip duplicates, and optional room owner/alias options)
	importResult, err := importer.ImportAssets(&assets, existingMappings, roomOpts, importProgress)
	if err != nil {
		o.state.FailStep(StepImportAssets, err)
		o.SaveState()
		return nil, fmt.Errorf("import failed: %w", err)
	}

	// Persist generated passwords before anything else can fail, otherwise they are lost and
	// the accounts become unreachable without an admin reset.
	if creds := importer.GeneratedCredentials(); len(creds) > 0 {
		if !o.config.Matrix.Import.UserPassword.WriteFile {
			logger.Warn("Generated %d user passwords but user_password.write_file is false; they are discarded (SSO or admin reset required)", len(creds))
		} else if path, werr := WriteUserPasswords(o.config.Data.AssetsDir, creds); werr != nil {
			logger.Error("Failed to write generated user passwords to %s: %v", o.config.Data.AssetsDir, werr)
		} else {
			logger.Warn("Wrote %d generated user passwords to %s (mode 0600) - distribute and delete this file", len(creds), path)
		}
	}

	// Import direct message channels as Matrix DMs when enabled. An interrupted run goes
	// straight to saving what it has.
	if o.interrupted() {
		logger.Warn("Import assets interrupted: skipping direct message import and room linking")
	} else if o.config.Matrix.Import.ImportDirectMessages && len(assets.DirectChannels) > 0 {
		logger.Info("Import direct messages: processing %d direct channels as DMs", len(assets.DirectChannels))
		existingRoomMapping := make(map[string]string)
		if existingMappings != nil {
			existingRoomMapping = existingMappings.Rooms
		}
		dmMapping, dmStats, err := importer.ImportDirectChannelsAsDMs(assets.DirectChannels, assets.Users, importResult.UserMapping, existingRoomMapping, importProgress)
		if err != nil {
			logger.Error("Import direct messages failed: %v", err)
			o.state.FailStep(StepImportAssets, err)
			o.SaveState()
			return nil, fmt.Errorf("import direct messages failed: %w", err)
		}
		for k, v := range dmMapping {
			importResult.RoomMapping[k] = v
		}
		importResult.Stats.RoomsCreated += dmStats.RoomsCreated
		importResult.Stats.RoomsSkipped += dmStats.RoomsSkipped
		importResult.Stats.RoomsFailed += dmStats.RoomsFailed
		logger.Info("Import direct messages: created=%d, skipped=%d, failed=%d", dmStats.RoomsCreated, dmStats.RoomsSkipped, dmStats.RoomsFailed)
	}

	// Fill result stats
	result.UsersCreated = importResult.Stats.UsersCreated
	result.UsersSkipped = importResult.Stats.UsersSkipped
	result.UsersFailed = importResult.Stats.UsersFailed
	result.SpacesCreated = importResult.Stats.SpacesCreated
	result.SpacesSkipped = importResult.Stats.SpacesSkipped
	result.SpacesFailed = importResult.Stats.SpacesFailed
	result.RoomsCreated = importResult.Stats.RoomsCreated
	result.RoomsSkipped = importResult.Stats.RoomsSkipped
	result.RoomsFailed = importResult.Stats.RoomsFailed

	// Save mapping (the same file the checkpoints wrote). It records the homeserver the client
	// actually talked to, which may differ from the configured one after detection.
	if err := saveAssetMapping(importResult.UserMapping, importResult.SpaceMapping, importResult.RoomMapping); err != nil {
		if o.interrupted() {
			return nil, o.failInterrupted(StepImportAssets,
				fmt.Errorf("asset import %w, and saving what was created so far failed: %v", ErrInterrupted, err))
		}
		o.state.FailStep(StepImportAssets, err)
		o.SaveState()
		return nil, fmt.Errorf("failed to save mapping: %w", err)
	}

	// Checked again after linking, which an interrupt can also cut short.
	interruptedAfterSave := func() error {
		return o.failInterrupted(StepImportAssets,
			fmt.Errorf("asset import %w: everything created so far is recorded in %s; run the same command again to resume", ErrInterrupted, mappingFile))
	}
	if o.interrupted() {
		return nil, interruptedAfterSave()
	}

	// Link rooms to spaces (pass userMapping and defaultSpaceOwnerID so admin can be invited into spaces/rooms before linking)
	defaultSpaceOwnerID := ""
	if roomOpts != nil {
		// Teams have no creator_id, so spaces are created using fallback_room_creator when available.
		// Use the same fallback owner here so owner-invite flow targets the actual space owner.
		if roomOpts.FallbackCreator != "" {
			defaultSpaceOwnerID = o.config.FormatUserID(roomOpts.FallbackCreator)
		}
		if defaultSpaceOwnerID == "" {
			defaultSpaceOwnerID = roomOpts.AdminUserID
		}
	}
	if progress != nil {
		progress("linking", 0, len(assets.Channels), "")
	}
	linkResult, err := importer.LinkRoomsToSpaces(assets.Channels, importResult.SpaceMapping, importResult.RoomMapping, importResult.UserMapping, defaultSpaceOwnerID, o.config.GetPublicRoomJoinRules(), importProgress)
	if err == nil && linkResult != nil {
		result.RoomsLinked = linkResult.RoomsLinked
		result.RoomsLinkFailed = linkResult.RoomsLinkFailed
		if linkResult.RoomsLinkFailed > 0 {
			logger.Warn("Import assets: %d rooms could not be linked to their space; re-run import assets to retry them", linkResult.RoomsLinkFailed)
		}
	}
	if o.interrupted() {
		return nil, interruptedAfterSave()
	}

	// Complete step
	o.state.CompleteStep(StepImportAssets, mappingFile)
	result.OutputFile = mappingFile
	return result, o.SaveState()
}

// ExportMemberships exports memberships from Mattermost
func (o *Orchestrator) ExportMemberships(progress ProgressCallback) (*OperationResult, error) {
	result := &OperationResult{}

	if o.mmClient == nil {
		return nil, fmt.Errorf("not connected to Mattermost")
	}

	// Check if we can run this step
	canRun, reason := o.state.CanRunStep(StepExportMemberships)
	if !canRun {
		return nil, fmt.Errorf("cannot run step: %s", reason)
	}

	// Start step
	o.state.StartStep(StepExportMemberships)
	if err := o.SaveState(); err != nil {
		return nil, err
	}

	// Create exporter
	exporter := mattermost.NewExporter(o.mmClient)

	// Export callback
	var exportProgress mattermost.ExportProgressCallback
	if progress != nil {
		exportProgress = func(stage string, current, total int) {
			progress(stage, current, total, "")
			o.state.UpdateStepProgress(StepExportMemberships, current, total)
		}
	}

	// Export memberships
	memberships, err := exporter.ExportMemberships(exportProgress)
	if err != nil {
		o.state.FailStep(StepExportMemberships, err)
		o.SaveState()
		return nil, fmt.Errorf("export failed: %w", err)
	}

	// Filter to active memberships
	memberships = mattermost.FilterActiveMemberships(memberships)

	// Count exported memberships
	result.TeamMembershipsExported = len(memberships.TeamMembers)
	result.ChannelMembershipsExported = len(memberships.ChannelMembers)

	// Generate filename
	timestamp := time.Now().Format("20060102-150405")
	filename := fmt.Sprintf("mattermost-memberships-%s.json.gz", timestamp)
	filepath := o.config.Data.AssetsDir + "/" + filename

	// Save to gzipped JSON
	if err := archive.SaveGzipJSON(filepath, memberships); err != nil {
		o.state.FailStep(StepExportMemberships, err)
		o.SaveState()
		return nil, fmt.Errorf("failed to save memberships: %w", err)
	}

	// Complete step
	o.state.CompleteStep(StepExportMemberships, filepath)
	result.OutputFile = filepath
	return result, o.SaveState()
}

// ImportMemberships imports memberships to Matrix
func (o *Orchestrator) ImportMemberships(progress ProgressCallback) (*OperationResult, error) {
	result := &OperationResult{}

	logger.Info("=== ImportMemberships Started ===")

	if o.mxClient == nil {
		logger.Error("Not connected to Matrix")
		return nil, fmt.Errorf("not connected to Matrix")
	}

	// Check if we can run this step
	canRun, reason := o.state.CanRunStep(StepImportMemberships)
	if !canRun {
		logger.Error("Cannot run step: %s", reason)
		return nil, fmt.Errorf("cannot run step: %s", reason)
	}
	// If memberships were already imported successfully, skip expensive replays by default.
	// This keeps reruns fast and avoids reissuing force-join operations. When replay is
	// forced, re-apply so members who joined after the first run get added (idempotent).
	if step := o.state.GetStep(StepImportMemberships); step.Status == StatusCompleted && !o.forceMembershipReplay {
		logger.Info("ImportMemberships: step already completed, skipping membership replay")
		return result, nil
	}

	// Get the membership file and mapping file from previous steps
	membershipFile := o.state.GetStepOutputFile(StepExportMemberships)
	if membershipFile == "" {
		logger.Error("No membership file found from export step")
		return nil, fmt.Errorf("no membership file found from export step")
	}
	logger.Info("Using membership file: %s", membershipFile)

	mappingFile := o.state.GetStepOutputFile(StepImportAssets)
	if mappingFile == "" {
		logger.Error("No mapping file found from import assets step")
		return nil, fmt.Errorf("no mapping file found from import assets step")
	}
	logger.Info("Using mapping file: %s", mappingFile)

	// Start step
	o.state.StartStep(StepImportMemberships)
	if err := o.SaveState(); err != nil {
		return nil, err
	}

	// Load memberships
	logger.Info("Loading memberships from file...")
	var memberships mattermost.Memberships
	if err := archive.LoadGzipJSON(membershipFile, &memberships); err != nil {
		logger.Error("Failed to load memberships: %v", err)
		o.state.FailStep(StepImportMemberships, err)
		o.SaveState()
		return nil, fmt.Errorf("failed to load memberships: %w", err)
	}
	logger.Info("Loaded %d team memberships, %d channel memberships",
		len(memberships.TeamMembers), len(memberships.ChannelMembers))

	// Load mapping
	logger.Info("Loading mapping from file...")
	mapping, err := LoadMapping(mappingFile)
	if err != nil {
		logger.Error("Failed to load mapping: %v", err)
		o.state.FailStep(StepImportMemberships, err)
		o.SaveState()
		return nil, fmt.Errorf("failed to load mapping: %w", err)
	}
	logger.Info("Loaded mapping: %d users, %d teams, %d channels",
		len(mapping.Users), len(mapping.Teams), len(mapping.Channels))

	// Load assets to get channel list (needed for group channel equal power levels and DM memberships)
	// and to resolve ignored usernames -> user IDs for membership filtering.
	var channels []mattermost.Channel
	var users []mattermost.User
	if assetFile := o.state.GetStepOutputFile(StepExportAssets); assetFile != "" {
		var assets mattermost.Assets
		if err := archive.LoadGzipJSON(assetFile, &assets); err == nil {
			channels = append(assets.Channels, assets.DirectChannels...)
			// Needed to explain why a channel referenced by a membership has no room.
			users = assets.Users
			logger.Info("Loaded %d channels (%d regular + %d direct) from assets for membership import", len(channels), len(assets.Channels), len(assets.DirectChannels))

			if len(o.config.Mattermost.IgnoredUsers) > 0 {
				ignoredUserIDs := mattermost.GetIgnoredUserIDs(assets.Users, o.config.Mattermost.IgnoredUsers)
				beforeTeam := len(memberships.TeamMembers)
				beforeChannel := len(memberships.ChannelMembers)
				memberships = *mattermost.FilterMembershipsByIgnoredUserIDs(&memberships, ignoredUserIDs)
				ignoredMemberships := (beforeTeam - len(memberships.TeamMembers)) + (beforeChannel - len(memberships.ChannelMembers))
				if ignoredMemberships > 0 {
					logger.Info("Import memberships: ignored %d memberships for configured users", ignoredMemberships)
				}
			}
		}
	}

	// Create importer
	importer := o.newImporter()

	// Import callback
	var importProgress matrix.ImportProgressCallback
	if progress != nil {
		importProgress = func(stage string, current, total int, item string) {
			progress(stage, current, total, item)
			o.state.UpdateStepProgress(StepImportMemberships, current, total)
		}
	}

	// Default owner for spaces/rooms when creator_id is empty.
	// Prefer fallback_room_creator because room/space import uses that as owner when creator_id is missing.
	defaultRoomOwnerID := ""
	if o.config.Matrix.Import.FallbackRoomCreator != "" {
		defaultRoomOwnerID = o.config.FormatUserID(o.config.Matrix.Import.FallbackRoomCreator)
	}
	if defaultRoomOwnerID == "" {
		defaultRoomOwnerID = o.config.FormatUserID(o.config.Matrix.Auth.Username)
	}
	if defaultRoomOwnerID == "" {
		if who, err := o.mxClient.WhoAmI(); err == nil && who != nil {
			defaultRoomOwnerID = who.UserID
		}
	}
	if defaultRoomOwnerID == "" {
		defaultRoomOwnerID = o.config.FormatUserID("admin")
	}
	defaultChannelOwnerID := defaultRoomOwnerID
	if who, err := o.mxClient.WhoAmI(); err == nil && who != nil && who.UserID != "" {
		defaultChannelOwnerID = who.UserID
	}

	// Apply team memberships
	if progress != nil {
		progress("team_memberships", 0, len(memberships.TeamMembers), "")
	}
	teamStats, spacesToLeaveAfterMembershipImport, err := importer.ApplyTeamMemberships(memberships.TeamMembers, users, mapping.Users, mapping.Teams, defaultRoomOwnerID, importProgress)
	if err != nil {
		o.state.FailStep(StepImportMemberships, err)
		o.SaveState()
		return nil, fmt.Errorf("failed to apply team memberships: %w", err)
	}

	// Apply channel memberships
	if progress != nil {
		progress("channel_memberships", 0, len(memberships.ChannelMembers), "")
	}
	channelStats, err := importer.ApplyChannelMemberships(channels, users, memberships.ChannelMembers, mapping.Users, mapping.Channels, defaultChannelOwnerID, importProgress)
	if err != nil {
		o.state.FailStep(StepImportMemberships, err)
		o.SaveState()
		return nil, fmt.Errorf("failed to apply channel memberships: %w", err)
	}

	// Final cleanup for membership import:
	// remove admin from spaces that were joined for force-join/bootstrap.
	leftSpaces := 0
	failedLeaveSpaces := 0
	for _, spaceID := range spacesToLeaveAfterMembershipImport {
		if err := o.mxClient.LeaveRoom(spaceID); err != nil {
			logger.Warn("Import memberships cleanup: admin leave space %s failed: %v", spaceID, err)
			failedLeaveSpaces++
		} else {
			leftSpaces++
		}
	}
	logger.Info("Import memberships cleanup: admin left spaces=%d leave_failures=%d attempted=%d",
		leftSpaces, failedLeaveSpaces, len(spacesToLeaveAfterMembershipImport))

	if o.interrupted() {
		return nil, o.failInterrupted(StepImportMemberships,
			fmt.Errorf("membership import %w; memberships are safe to re-apply, run the same command again to finish", ErrInterrupted))
	}

	// Fill result stats
	result.MembersAdded = teamStats.MembersAdded + channelStats.MembersAdded
	result.MembersSkipped = teamStats.MembersSkipped + channelStats.MembersSkipped
	result.MembersFailed = teamStats.MembersFailed + channelStats.MembersFailed

	logger.Info("=== ImportMemberships Completed ===")
	logger.Info("Total: added=%d, skipped=%d, failed=%d",
		result.MembersAdded, result.MembersSkipped, result.MembersFailed)
	logger.Success("Membership import completed successfully")

	// Complete step
	o.state.CompleteStep(StepImportMemberships, "")
	return result, o.SaveState()
}

// LeaveRooms makes the migration admin leave every room and space from the asset mapping.
//
// This is a cleanup sweep, not part of the import chain: the import steps already leave
// rooms inline after force-joining members, but they only log a warning when that fails,
// which leaves the admin account inside private rooms and other people's DMs. Running this
// at the end of a migration clears those leftovers, and it is safe to repeat. It also
// withdraws the history joins a message import recorded in its journal but never undid.
func (o *Orchestrator) LeaveRooms(progress ProgressCallback) (*OperationResult, error) {
	result := &OperationResult{}

	logger.Info("=== LeaveRooms Started ===")

	if o.mxClient == nil {
		logger.Error("Not connected to Matrix")
		return nil, fmt.Errorf("not connected to Matrix")
	}

	canRun, reason := o.state.CanRunStep(StepLeaveRooms)
	if !canRun {
		logger.Error("Cannot run step: %s", reason)
		return nil, fmt.Errorf("cannot run step: %s", reason)
	}

	mappingFile := o.state.GetStepOutputFile(StepImportAssets)
	if mappingFile == "" {
		logger.Error("No mapping file found from import assets step")
		return nil, fmt.Errorf("no mapping file found from import assets step")
	}
	logger.Info("Using mapping file: %s", mappingFile)

	o.state.StartStep(StepLeaveRooms)
	if err := o.SaveState(); err != nil {
		return nil, err
	}

	mapping, err := LoadMapping(mappingFile)
	if err != nil {
		logger.Error("Failed to load mapping: %v", err)
		o.state.FailStep(StepLeaveRooms, err)
		o.SaveState()
		return nil, fmt.Errorf("failed to load mapping: %w", err)
	}

	historyJournal, err := LoadHistoryJoinJournal(HistoryJoinJournalPath(o.config.Data.MappingsDir))
	if err != nil {
		logger.Error("%v", err)
		o.state.FailStep(StepLeaveRooms, err)
		o.SaveState()
		return nil, err
	}

	// Rooms first, then spaces: a space is only left once its rooms are done, so a run
	// interrupted midway does not leave the admin outside a space it still needs.
	roomIDs := make([]string, 0, len(mapping.Channels)+len(mapping.Teams))
	for _, roomID := range mapping.Channels {
		roomIDs = append(roomIDs, roomID)
	}
	for _, spaceID := range mapping.Teams {
		roomIDs = append(roomIDs, spaceID)
	}
	logger.Info("Leaving %d rooms and %d spaces", len(mapping.Channels), len(mapping.Teams))

	importer := o.newImporter()

	var importProgress matrix.ImportProgressCallback
	if progress != nil {
		importProgress = func(stage string, current, total int, item string) {
			progress(stage, current, total, item)
			o.state.UpdateStepProgress(StepLeaveRooms, current, total)
		}
	}

	// Both sweeps run before the admin leaves, not after: removing somebody from an
	// invite-only room can require putting the admin into it first, which would undo the
	// leave that had just been done.

	// Deactivated accounts, whose memberships an earlier run may have created and which
	// Synapse will not clean up on its own once the account is already deactivated.
	if users := o.loadExportedUsers(); len(users) > 0 {
		removal := importer.RemoveDeletedUsersFromRooms(users, mapping.Users, importProgress)
		result.DeactivatedAccounts = removal.Accounts
		result.DeactivatedRoomsLeft = removal.Left
		result.DeactivatedRoomsKept = removal.Kept
		result.DeactivatedRoomsFailed = removal.Failed
	} else {
		logger.Warn("No exported assets found: cannot tell which accounts are deactivated, skipping their room removal")
	}

	// The migration's own application service bot, which joins rooms only so it can post on
	// behalf of authors who no longer have an account.
	botRemoval := importer.RemoveASBotFromRooms(importProgress)
	result.BotRoomsLeft = botRemoval.Left
	result.BotRoomsKept = botRemoval.Kept
	result.BotRoomsFailed = botRemoval.Failed

	// Past authors joined only to replay history by a message import that did not get as far
	// as its own cleanup - an interrupted or failed run - recorded in the history-join journal.
	attachHistoryJoinJournal(importer, historyJournal)
	historyCleanup := withdrawHistoryJoins(importer, historyJournal)

	stats, err := importer.LeaveMigratedRooms(roomIDs, importProgress)
	if err != nil {
		logger.Error("Failed to leave rooms: %v", err)
		o.state.FailStep(StepLeaveRooms, err)
		o.SaveState()
		return nil, fmt.Errorf("failed to leave rooms: %w", err)
	}

	if o.interrupted() {
		return nil, o.failInterrupted(StepLeaveRooms,
			fmt.Errorf("leaving rooms %w; run the same command again to finish", ErrInterrupted))
	}

	result.RoomsLeft = stats.RoomsLeft
	result.RoomsLeaveSkip = stats.RoomsLeaveSkip
	result.RoomsLeaveFailed = stats.RoomsLeaveFail

	logger.Info("=== LeaveRooms Completed ===")
	logger.Info("Total: left=%d, already-out=%d, failed=%d",
		result.RoomsLeft, result.RoomsLeaveSkip, result.RoomsLeaveFailed)
	logger.Info("Deactivated accounts: checked=%d, memberships_removed=%d, kept_as_owner=%d, failed=%d",
		result.DeactivatedAccounts, result.DeactivatedRoomsLeft, result.DeactivatedRoomsKept, result.DeactivatedRoomsFailed)
	logger.Info("Migration bot: rooms_left=%d, kept_as_owner=%d, failed=%d",
		result.BotRoomsLeft, result.BotRoomsKept, result.BotRoomsFailed)
	logger.Info("History joins: left=%d, kept_owners=%d, failed=%d, still_recorded=%d",
		historyCleanup.Left, historyCleanup.Kept, historyCleanup.Failed, len(historyCleanup.Remaining))
	if result.RoomsLeaveFailed > 0 {
		logger.Warn("The migration admin is still in %d room(s); re-run 'import leave-rooms' or remove it manually", result.RoomsLeaveFailed)
	} else {
		logger.Success("Migration admin left all migrated rooms and spaces")
	}

	o.state.CompleteStep(StepLeaveRooms, "")
	return result, o.SaveState()
}

// loadExportedUsers reads the user list from the newest asset export, or returns nil when
// there is none. Deletion is a Mattermost fact, so the export is the only record of which
// migrated accounts are supposed to be closed.
func (o *Orchestrator) loadExportedUsers() []mattermost.User {
	assetFile := o.state.GetStepOutputFile(StepExportAssets)
	if assetFile == "" {
		return nil
	}
	var assets mattermost.Assets
	if err := archive.LoadGzipJSON(assetFile, &assets); err != nil {
		logger.Warn("Could not read exported assets from %s: %v", assetFile, err)
		return nil
	}
	return assets.Users
}

// EnableEmailNotifications registers an email pusher for every migrated user with an address,
// so email notifications are on from the start rather than waiting for each person to find
// the setting.
//
// Deliberately a separate step at the end of a migration: a new pusher starts from the current
// stream position, so running this before the messages are imported would mail the entire
// migration to everyone.
func (o *Orchestrator) EnableEmailNotifications(progress ProgressCallback) (*OperationResult, error) {
	result := &OperationResult{}

	logger.Info("=== EnableEmailNotifications Started ===")

	if o.mxClient == nil {
		logger.Error("Not connected to Matrix")
		return nil, fmt.Errorf("not connected to Matrix")
	}

	canRun, reason := o.state.CanRunStep(StepEnableNotifications)
	if !canRun {
		logger.Error("Cannot run step: %s", reason)
		return nil, fmt.Errorf("cannot run step: %s", reason)
	}

	if !o.config.UseAppService() {
		err := fmt.Errorf("email notifications need the Application Service: a pusher is registered as the user, which requires ?user_id=. Set matrix.appservice.enabled and the as_token_env variable")
		logger.Error("%v", err)
		return nil, err
	}

	// Addresses come from the assets export; the mapping only holds IDs.
	assetsFile := o.state.GetStepOutputFile(StepExportAssets)
	if assetsFile == "" {
		return nil, fmt.Errorf("no assets export file found")
	}
	var assets mattermost.Assets
	if err := archive.LoadGzipJSON(assetsFile, &assets); err != nil {
		return nil, fmt.Errorf("failed to load assets: %w", err)
	}

	mappingFile := o.state.GetStepOutputFile(StepImportAssets)
	if mappingFile == "" {
		return nil, fmt.Errorf("no mapping file found from import assets step")
	}
	mapping, err := LoadMapping(mappingFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load mapping: %w", err)
	}
	logger.Info("Loaded %d users from %s and %d mappings from %s",
		len(assets.Users), assetsFile, len(mapping.Users), mappingFile)

	o.state.StartStep(StepEnableNotifications)
	if err := o.SaveState(); err != nil {
		return nil, err
	}

	o.mxClient.SetASToken(o.config.GetASToken())

	importer := o.newImporter()

	var importProgress matrix.ImportProgressCallback
	if progress != nil {
		importProgress = func(stage string, current, total int, item string) {
			progress(stage, current, total, item)
			o.state.UpdateStepProgress(StepEnableNotifications, current, total)
		}
	}

	stats, err := importer.EnableEmailNotifications(assets.Users, mapping.Users, importProgress)
	if err != nil {
		logger.Error("Failed to enable email notifications: %v", err)
		o.state.FailStep(StepEnableNotifications, err)
		o.SaveState()
		return nil, err
	}

	if o.interrupted() {
		return nil, o.failInterrupted(StepEnableNotifications,
			fmt.Errorf("enabling email notifications %w; run the same command again to finish", ErrInterrupted))
	}

	result.UsersCreated = stats.UsersCreated
	result.UsersSkipped = stats.UsersSkipped
	result.UsersFailed = stats.UsersFailed

	logger.Info("=== EnableEmailNotifications Completed ===")
	logger.Info("Total: enabled=%d, skipped=%d, failed=%d",
		result.UsersCreated, result.UsersSkipped, result.UsersFailed)
	if result.UsersFailed > 0 {
		logger.Warn("%d user(s) did not get email notifications; see the reasons above and re-run this step once fixed", result.UsersFailed)
	} else {
		logger.Success("Email notifications enabled")
	}

	o.state.CompleteStep(StepEnableNotifications, "")
	return result, o.SaveState()
}

// TestMattermostConnection tests the Mattermost connection
func (o *Orchestrator) TestMattermostConnection() error {
	cfg := o.config.Mattermost
	passphrase := o.config.GetSSHKeyPassphrase("mattermost")
	sshPassword := o.config.GetSSHPassword("mattermost")

	// Test SSH connection first
	if err := ssh.TestConnectionWithPassword(cfg.SSH, passphrase, sshPassword); err != nil {
		return fmt.Errorf("SSH connection failed: %w", err)
	}

	// If not using manual config, test reading config.json
	if !o.config.HasManualDatabaseConfig() {
		_, err := mattermost.GetDatabaseCredentials(cfg.SSH, passphrase, sshPassword, cfg.ConfigPath)
		if err != nil {
			return fmt.Errorf("failed to read Mattermost config: %w", err)
		}
	}

	// Connect and test database
	if err := o.ConnectMattermost(); err != nil {
		return err
	}

	// Test database query
	if err := o.mmClient.Ping(); err != nil {
		return fmt.Errorf("database ping failed: %w", err)
	}

	return nil
}

// TestMatrixConnection tests the Matrix connection
func (o *Orchestrator) TestMatrixConnection() error {
	cfg := o.config.Matrix
	passphrase := o.config.GetSSHKeyPassphrase("matrix")
	sshPassword := o.config.GetSSHPassword("matrix")

	// Test SSH connection first
	if err := ssh.TestConnectionWithPassword(cfg.SSH, passphrase, sshPassword); err != nil {
		return fmt.Errorf("SSH connection failed: %w", err)
	}

	// Connect and test API
	if err := o.ConnectMatrix(); err != nil {
		return err
	}

	return nil
}

// ExportMessagesResult contains the result of message export
type ExportMessagesResult struct {
	OutputFile       string
	MessagesExported int
	FilesExported    int
}

// ExportMessages exports all messages from Mattermost
func (o *Orchestrator) ExportMessages(progress matrix.ImportProgressCallback) (*ExportMessagesResult, error) {
	// Start step
	o.state.StartStep(StepExportMessages)
	if err := o.SaveState(); err != nil {
		return nil, err
	}

	logger.Info("=== ExportMessages Started ===")

	// Create exporter
	exporter := mattermost.NewExporter(o.mmClient)

	// Export messages
	exportProgress := func(stage string, current, total int) {
		if progress != nil {
			progress(stage, current, total, "")
		}
		o.state.UpdateStepProgress(StepExportMessages, current, total)
	}

	messages, err := exporter.ExportMessages(exportProgress)
	if err != nil {
		o.state.FailStep(StepExportMessages, err)
		o.SaveState()
		return nil, fmt.Errorf("failed to export messages: %w", err)
	}

	logger.Info("Exported %d messages", len(messages.Posts))

	// Save to compressed file
	timestamp := time.Now().Format("20060102-150405")
	filename := fmt.Sprintf("%s/mattermost-messages-%s.json.gz", o.config.Data.AssetsDir, timestamp)

	if err := archive.SaveGzipJSON(filename, messages); err != nil {
		o.state.FailStep(StepExportMessages, err)
		o.SaveState()
		return nil, fmt.Errorf("failed to save messages: %w", err)
	}

	logger.Success("Messages saved to %s", filename)

	// Complete step
	o.state.CompleteStep(StepExportMessages, filename)
	if err := o.SaveState(); err != nil {
		return nil, err
	}

	return &ExportMessagesResult{
		OutputFile:       filename,
		MessagesExported: len(messages.Posts),
		FilesExported:    len(messages.Files),
	}, nil
}

// ImportMessagesResult contains the result of message import
type ImportMessagesResult struct {
	MessagesImported int
	MessagesSkipped  int
	MessagesFailed   int
	RepliesImported  int
	RepliesFailed    int
	FilesLinked      int
	FilesUploaded    int
	FilesSkipped     int
	FilesTooLarge    int

	ReactionsImported    int
	ReactionsSkipped     int
	ReactionsFailed      int
	ReactionsCustomEmoji int

	PinnedRoomsUpdated   int
	PinnedRoomsUnchanged int
	PinnedEventsAdded    int
	PinsSkipped          int
	PinsFailed           int

	MappingFile string
}

// messageCheckpointInterval is how many imported messages pass between mapping checkpoints.
// Small enough that an interrupted multi-day import loses little work, large enough that
// rewriting the mapping file stays a rounding error next to the import itself.
const messageCheckpointInterval = 500

// addMessageEntries adds mapping entries for posts not already recorded in m.
// postByID indexes the export so this stays O(n) rather than rescanning every post.
func addMessageEntries(m *MessageMapping, mapping map[string]string, postByID map[string]*mattermost.Post, assetMapping *Mapping) {
	for mmID, mxEventID := range mapping {
		if _, exists := m.Messages[mmID]; exists {
			continue
		}
		post, ok := postByID[mmID]
		if !ok {
			continue
		}
		m.AddMessage(&MessageMapEntry{
			MattermostID:  mmID,
			MatrixEventID: mxEventID,
			ChannelID:     post.ChannelID,
			RoomID:        assetMapping.Channels[post.ChannelID],
			UserID:        post.UserID,
			MatrixUserID:  assetMapping.Users[post.UserID],
			Timestamp:     post.CreateAt,
			IsReply:       post.IsReply(),
			RootID:        post.RootID,
		})
	}
}

// ImportMessages imports messages to Matrix
func (o *Orchestrator) ImportMessages(progress matrix.MessageImportCallback) (*ImportMessagesResult, error) {
	// Start step
	o.state.StartStep(StepImportMessages)
	if err := o.SaveState(); err != nil {
		return nil, err
	}

	logger.Info("=== ImportMessages Started ===")

	// Load exported messages
	messagesFile := o.state.GetStepOutputFile(StepExportMessages)
	if messagesFile == "" {
		err := fmt.Errorf("no messages export file found")
		o.state.FailStep(StepImportMessages, err)
		o.SaveState()
		return nil, err
	}

	var messages mattermost.Messages
	if err := archive.LoadGzipJSON(messagesFile, &messages); err != nil {
		o.state.FailStep(StepImportMessages, err)
		o.SaveState()
		return nil, fmt.Errorf("failed to load messages: %w", err)
	}

	logger.Info("Loaded %d messages and %d files from %s", len(messages.Posts), len(messages.Files), messagesFile)

	// Build files by post map
	filesByPost := make(map[string][]mattermost.FileInfo)
	for _, file := range messages.Files {
		if file.PostID != "" {
			filesByPost[file.PostID] = append(filesByPost[file.PostID], file)
		}
	}
	logger.Info("Built file mapping: %d posts have files", len(filesByPost))

	// Load asset mapping for room and user mappings
	assetMappingFile := o.state.GetStepOutputFile(StepImportAssets)
	if assetMappingFile == "" {
		err := fmt.Errorf("no asset mapping file found")
		o.state.FailStep(StepImportMessages, err)
		o.SaveState()
		return nil, err
	}

	assetMapping, err := LoadMapping(assetMappingFile)
	if err != nil {
		o.state.FailStep(StepImportMessages, err)
		o.SaveState()
		return nil, fmt.Errorf("failed to load asset mapping: %w", err)
	}

	logger.Info("Loaded asset mapping: %d rooms, %d users", len(assetMapping.Channels), len(assetMapping.Users))

	// Load or create message mapping for resume support
	msgMappingFile, _ := GetLatestMessageMappingFile(o.config.Data.MappingsDir)
	msgMapping, err := loadOrCreateMessageMapping(msgMappingFile, o.config.Matrix.Homeserver)
	if err != nil {
		o.state.FailStep(StepImportMessages, err)
		o.SaveState()
		return nil, err
	}
	if msgMappingFile != "" {
		logger.Info("Resuming from existing mapping with %d messages", msgMapping.Count())
	}

	// Memberships made only to replay history are journalled before they are made, so an
	// interrupted run's are still withdrawn - by this run's cleanup or by 'import leave-rooms'.
	historyJournal, err := LoadHistoryJoinJournal(HistoryJoinJournalPath(o.config.Data.MappingsDir))
	if err != nil {
		o.state.FailStep(StepImportMessages, err)
		o.SaveState()
		return nil, err
	}

	// Set up AS token if configured
	if o.config.UseAppService() {
		o.mxClient.SetASToken(o.config.GetASToken())
		logger.Info("Application Service token configured - messages will have original timestamps")
	} else {
		logger.Warn("No Application Service token - messages will be imported with current timestamps")
	}

	// Create importer
	importer := o.newImporter()
	attachHistoryJoinJournal(importer, historyJournal)

	// Convert existing mapping to simple map
	existingMapping := make(map[string]string)
	for mmID, entry := range msgMapping.Messages {
		existingMapping[mmID] = entry.MatrixEventID
	}

	// Build file config
	fileConfig := &matrix.FileConfig{
		Mode:                 o.config.GetFileMode(),
		S3PublicURL:          o.config.Mattermost.Files.S3PublicURL,
		LocalDataPath:        o.config.Mattermost.Files.LocalDataPath,
		UploadFallbackToLink: o.config.Mattermost.Files.FallbackToLinkOnUploadFailure,
		MaxUploadSize:        o.config.GetMaxUploadSize(),
	}
	if fileConfig.Mode == "upload" && fileConfig.LocalDataPath != "" && o.config.Mattermost.SSH.Host != "" {
		passphrase := o.config.GetSSHKeyPassphrase("mattermost")
		sshPassword := o.config.GetSSHPassword("mattermost")
		remoteExecutor, remoteErr := ssh.NewRemoteExecutorWithPassword(o.config.Mattermost.SSH, passphrase, sshPassword)
		if remoteErr != nil {
			logger.Warn("Upload mode: could not initialize Mattermost SSH file reader (will use local path only): %v", remoteErr)
		} else {
			defer func() {
				if closeErr := remoteExecutor.Close(); closeErr != nil {
					logger.Warn("Upload mode: failed to close Mattermost SSH file reader: %v", closeErr)
				}
			}()
			readWithSudo := o.config.Mattermost.Files.ReadWithSudo
			fileConfig.RemoteReadFile = attachmentReader(remoteExecutor.ReadFileAsUser, remoteExecutor.ReadFile, readWithSudo)
			logger.Info("Upload mode: Mattermost SSH file reader enabled for remote local_data_path (read_with_sudo: %t)", readWithSudo)
		}
	}
	logger.Info("File mode: %s, S3 URL: %s", fileConfig.Mode, fileConfig.S3PublicURL)

	// Index posts by ID once; both the checkpoint callback and the final mapping update need it.
	postByID := make(map[string]*mattermost.Post, len(messages.Posts))
	for idx := range messages.Posts {
		postByID[messages.Posts[idx].ID] = &messages.Posts[idx]
	}

	// Checkpoint the mapping periodically. A large instance takes days to import, and without
	// this the mapping only lands when the whole run finishes: any interruption would leave
	// every sent message unrecorded, so a restart would import them a second time.
	mappingFile := GenerateMessageMappingFilename(o.config.Data.MappingsDir)
	importer.SetMessageCheckpoint(messageCheckpointInterval, func(partial map[string]string) {
		addMessageEntries(msgMapping, partial, postByID, assetMapping)
		if err := SaveMessageMapping(msgMapping, mappingFile); err != nil {
			logger.Warn("Checkpoint: failed to save message mapping: %v", err)
			return
		}
		logger.Info("Checkpoint: message mapping saved with %d entries to %s", len(msgMapping.Messages), mappingFile)
	})

	// Reactions ride along with the message import: they need the event IDs it produces.
	var reactionImport *matrix.ReactionImport
	switch {
	case !o.config.Matrix.Import.ImportReactions:
		logger.Info("Reaction import disabled (matrix.import.import_reactions: false)")
	case len(messages.Reactions) == 0:
		// An export taken before reactions were supported has no such field, and looks exactly
		// like an instance where nobody ever reacted. Say so, rather than silently doing nothing.
		logger.Info("No reactions in the message export - re-run 'export messages' if the instance has any")
	default:
		reactionImport = &matrix.ReactionImport{
			Reactions:       messages.Reactions,
			AlreadyImported: msgMapping.ReactionKeys(),
		}
		logger.Info("Reaction import enabled: %d reactions in export, %d already sent by earlier runs",
			len(messages.Reactions), len(reactionImport.AlreadyImported))

		importer.SetReactionCheckpoint(messageCheckpointInterval, func(partial map[string]string) {
			for key, eventID := range partial {
				msgMapping.AddReaction(key, eventID)
			}
			if err := SaveMessageMapping(msgMapping, mappingFile); err != nil {
				logger.Warn("Checkpoint: failed to save reaction mapping: %v", err)
				return
			}
			logger.Info("Checkpoint: reaction mapping saved with %d entries to %s", msgMapping.ReactionCount(), mappingFile)
		})
	}

	// Pins ride along with the message import too: the state event names event IDs, which only
	// exist once the messages have been sent.
	var pinImport *matrix.PinImport
	switch {
	case !o.config.Matrix.Import.ImportPinnedMessages:
		logger.Info("Pinned message import disabled (matrix.import.import_pinned_messages: false)")
	default:
		pinnedPosts := 0
		for idx := range messages.Posts {
			if messages.Posts[idx].IsPinned {
				pinnedPosts++
			}
		}
		if pinnedPosts == 0 {
			// An export taken before pins were supported has the flag false on every post, and
			// looks exactly like an instance where nobody ever pinned anything. Say so.
			logger.Info("No pinned posts in the message export - re-run 'export messages' if the instance has any")
		} else {
			pinImport = &matrix.PinImport{}
			logger.Info("Pinned message import enabled: %d pinned post(s) in export", pinnedPosts)
		}
	}

	// Import messages with files
	result, err := importer.ImportMessagesWithFiles(
		messages.Posts,
		assetMapping.Channels, // channelID -> roomID
		assetMapping.Users,    // userID -> matrixUserID
		existingMapping,       // existing message mapping
		filesByPost,           // post ID -> files
		fileConfig,            // file migration settings
		reactionImport,        // reactions, or nil to skip them
		pinImport,             // pinned messages, or nil to skip them
		progress,
	)
	if err != nil {
		o.state.FailStep(StepImportMessages, err)
		o.SaveState()
		return nil, fmt.Errorf("failed to import messages: %w", err)
	}

	// Persist per-message failure reasons so they're diagnosable (the aggregate counts alone
	// hide why ~10% of posts fail — usually no_room for skipped DMs / archived channels).
	if len(result.Errors) > 0 {
		if path, werr := WriteMessageErrors(o.config.Data.AssetsDir, result.Errors); werr != nil {
			logger.Warn("Failed to write message error log: %v", werr)
		} else {
			c := CategorizeMessageErrors(result.Errors)
			logger.Warn("Message import had %d errors; details written to %s", len(result.Errors), path)
			logger.Warn("Message error categories: no_room=%d send_error=%d reply_error=%d reaction_error=%d pin_error=%d parent_missing=%d other=%d",
				c["no_room"], c["send_error"], c["reply_error"], c["reaction_error"], c["pin_error"], c["parent_missing"], c["other"])
		}
	}

	// Update message mapping with new imports, then save to the same file the checkpoints
	// have been writing so a run leaves exactly one mapping behind.
	addMessageEntries(msgMapping, result.Mapping, postByID, assetMapping)
	for key, eventID := range result.ReactionMapping {
		msgMapping.AddReaction(key, eventID)
	}

	mappingErr := SaveMessageMapping(msgMapping, mappingFile)
	if mappingErr != nil {
		logger.Warn("Failed to save message mapping: %v", mappingErr)
	} else {
		logger.Info("Message mapping saved to %s", mappingFile)
	}

	logger.Info("=== ImportMessages Completed ===")
	logger.Info("Messages: imported=%d, skipped=%d, failed=%d",
		result.Stats.MessagesImported, result.Stats.MessagesSkipped, result.Stats.MessagesFailed)
	logger.Info("Replies: imported=%d, failed=%d",
		result.Stats.RepliesImported, result.Stats.RepliesFailed)
	logger.Info("Files: linked=%d, uploaded=%d, skipped=%d, too_large=%d",
		result.Stats.FilesLinked, result.Stats.FilesUploaded, result.Stats.FilesSkipped, result.Stats.FilesTooLarge)
	if reactionImport != nil {
		logger.Info("Reactions: imported=%d, skipped=%d, failed=%d, custom_emoji=%d",
			result.Stats.ReactionsImported, result.Stats.ReactionsSkipped,
			result.Stats.ReactionsFailed, result.Stats.ReactionsCustomEmoji)
	}
	// Withdraw the memberships the import created for itself. Owners installed in place of a
	// locked or missing creator are kept - see LeaveHistoryMemberships. Whatever cannot be
	// withdrawn stays in the journal for 'import leave-rooms'.
	if cleanup := withdrawHistoryJoins(importer, historyJournal); cleanup.Left > 0 || cleanup.Kept > 0 || cleanup.Failed > 0 {
		logger.Info("Membership cleanup: left=%d, kept_owners=%d, failed=%d",
			cleanup.Left, cleanup.Kept, cleanup.Failed)
	}

	if o.interrupted() {
		if mappingErr != nil {
			return nil, o.failInterrupted(StepImportMessages,
				fmt.Errorf("message import %w, and saving the message mapping failed (%v); a re-run resumes from the last checkpoint in %s, if one was written", ErrInterrupted, mappingErr, mappingFile))
		}
		return nil, o.failInterrupted(StepImportMessages,
			fmt.Errorf("message import %w: progress saved to %s (%d messages); run the same command again to resume", ErrInterrupted, mappingFile, len(msgMapping.Messages)))
	}

	logger.Success("Message import completed successfully")

	// Complete step
	o.state.CompleteStep(StepImportMessages, mappingFile)
	if err := o.SaveState(); err != nil {
		return nil, err
	}

	return &ImportMessagesResult{
		MessagesImported: result.Stats.MessagesImported,
		MessagesSkipped:  result.Stats.MessagesSkipped,
		MessagesFailed:   result.Stats.MessagesFailed,
		RepliesImported:  result.Stats.RepliesImported,
		RepliesFailed:    result.Stats.RepliesFailed,
		FilesLinked:      result.Stats.FilesLinked,
		FilesUploaded:    result.Stats.FilesUploaded,
		FilesSkipped:     result.Stats.FilesSkipped,
		FilesTooLarge:    result.Stats.FilesTooLarge,

		ReactionsImported:    result.Stats.ReactionsImported,
		ReactionsSkipped:     result.Stats.ReactionsSkipped,
		ReactionsFailed:      result.Stats.ReactionsFailed,
		ReactionsCustomEmoji: result.Stats.ReactionsCustomEmoji,

		PinnedRoomsUpdated:   result.Stats.PinnedRoomsUpdated,
		PinnedRoomsUnchanged: result.Stats.PinnedRoomsUnchanged,
		PinnedEventsAdded:    result.Stats.PinnedEventsAdded,
		PinsSkipped:          result.Stats.PinsSkipped,
		PinsFailed:           result.Stats.PinsFailed,

		MappingFile: mappingFile,
	}, nil
}

// attachmentReader picks how attachments are read over SSH: as the SSH user by default, or
// with the `sudo cat` fallback when mattermost.files.read_with_sudo is set. A failed plain
// read names that option, since a permission error is the usual cause.
func attachmentReader(asUser, withSudo func(string) ([]byte, error), useSudo bool) func(string) ([]byte, error) {
	if useSudo {
		return withSudo
	}
	return func(path string) ([]byte, error) {
		data, err := asUser(path)
		if err != nil {
			return nil, fmt.Errorf("%w (if the SSH user lacks permission, set mattermost.files.read_with_sudo: true to retry with sudo)", err)
		}
		return data, nil
	}
}
