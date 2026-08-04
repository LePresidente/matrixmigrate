package migration

import (
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
	mxToken   string            // Matrix access token (from login or config)

	// forceMembershipReplay re-applies channel/team memberships even when the step already
	// completed, so members who joined after the first run get added on a later run.
	forceMembershipReplay bool
}

// SetForceMembershipReplay controls whether a completed membership step is re-applied on
// re-run (to pick up members added since the first run). Force-join is idempotent.
func (o *Orchestrator) SetForceMembershipReplay(force bool) {
	o.forceMembershipReplay = force
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
	}, nil
}

// Close closes all connections
func (o *Orchestrator) Close() error {
	logger.Close()
	if o.mmClient != nil {
		o.mmClient.Close()
	}
	return o.tunnelManager.CloseAll()
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

	// Membership stats
	TeamMembershipsExported    int
	ChannelMembershipsExported int
	MembersAdded               int
	MembersSkipped             int
	MembersFailed              int

	// Output file
	OutputFile string
}

// ConnectMattermost establishes connection to Mattermost
func (o *Orchestrator) ConnectMattermost() error {
	cfg := o.config.Mattermost
	passphrase := o.config.GetSSHKeyPassphrase("mattermost")
	sshPassword := o.config.GetSSHPassword("mattermost")

	// Get database credentials
	var dbHost string
	var dbPort int
	var dbUser string
	var dbPassword string
	var dbName string

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
		// Get an available local port for the tunnel
		localPort, err := ssh.GetLocalPort()
		if err != nil {
			return fmt.Errorf("failed to get local port: %w", err)
		}

		// Create SSH tunnel to database
		tunnelCfg := ssh.TunnelConfig{
			SSHConfig:  cfg.SSH,
			LocalPort:  localPort,
			RemoteHost: dbHost,
			RemotePort: dbPort,
			Passphrase: passphrase,
			Password:   sshPassword,
		}

		_, err = o.tunnelManager.CreateTunnel("mattermost", tunnelCfg)
		if err != nil {
			return fmt.Errorf("failed to create SSH tunnel: %w", err)
		}

		connHost, connPort = "127.0.0.1", localPort
	}

	dsn := buildPostgresDSN(connHost, connPort, dbUser, dbPassword, dbName)

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

// ConnectMatrix establishes connection to Matrix
func (o *Orchestrator) ConnectMatrix() error {
	cfg := o.config.Matrix

	// Direct mode: no ssh.host means the Matrix API is reachable from here, so talk to
	// matrix.api.base_url instead of forwarding a port. This is the normal case for a
	// homeserver behind an HTTPS ingress, where nothing listens on 127.0.0.1:8008.
	direct := cfg.SSH.Host == ""

	var baseURL string

	if direct {
		baseURL = o.config.MatrixAPIURL()
		logger.Info("Connecting directly to Matrix API at %s", baseURL)
	} else {
		passphrase := o.config.GetSSHKeyPassphrase("matrix")
		sshPassword := o.config.GetSSHPassword("matrix")

		// Get an available local port for the tunnel
		localPort, err := ssh.GetLocalPort()
		if err != nil {
			return fmt.Errorf("failed to get local port: %w", err)
		}

		// Get remote API port from config (default: 8008)
		remotePort := cfg.API.Port
		if remotePort == 0 {
			remotePort = 8008
		}

		// Create SSH tunnel to Matrix API
		tunnelCfg := ssh.TunnelConfig{
			SSHConfig:  cfg.SSH,
			LocalPort:  localPort,
			RemoteHost: "127.0.0.1",
			RemotePort: remotePort,
			Passphrase: passphrase,
			Password:   sshPassword,
		}

		logger.Info("Creating SSH tunnel to Matrix API (local:%d -> remote:127.0.0.1:%d)", localPort, remotePort)

		_, err = o.tunnelManager.CreateTunnel("matrix", tunnelCfg)
		if err != nil {
			return fmt.Errorf("failed to create SSH tunnel: %w", err)
		}

		// Use local tunnel URL
		baseURL = fmt.Sprintf("http://127.0.0.1:%d", localPort)

		// Wait a moment for the tunnel to be ready
		time.Sleep(500 * time.Millisecond)

		// Verify tunnel is working by attempting a simple HTTP request
		if err := o.waitForTunnel(baseURL, 5*time.Second); err != nil {
			o.tunnelManager.CloseTunnel("matrix")
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
			o.tunnelManager.CloseTunnel("matrix")
			return fmt.Errorf("Matrix password not found in environment variable %s", cfg.Auth.PasswordEnv)
		}

		loginResp, err := matrix.Login(baseURL, cfg.Auth.Username, password)
		if err != nil {
			o.tunnelManager.CloseTunnel("matrix")
			return fmt.Errorf("failed to login to Matrix: %w", err)
		}
		accessToken = loginResp.AccessToken
		o.mxToken = accessToken
	}

	// Create Matrix client with rate limiting from config
	rlConfig := matrix.RateLimitConfig{
		RequestsPerSecond: cfg.RateLimit.RequestsPerSecond,
		MaxRetries:        cfg.RateLimit.MaxRetries,
		RetryBaseDelay:    time.Duration(cfg.RateLimit.RetryBaseDelay) * time.Millisecond,
	}
	client := matrix.NewClientWithRateLimit(baseURL, accessToken, cfg.Homeserver, rlConfig)

	// Test connection
	if err := client.TestConnection(); err != nil {
		o.tunnelManager.CloseTunnel("matrix")
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
			o.tunnelManager.CloseTunnel("matrix")
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
		o.tunnelManager.CloseTunnel("matrix")
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

	// Try to load existing mapping to skip already imported items
	var existingMappings *matrix.ExistingMappings
	existingMappingFile := o.state.GetStepOutputFile(StepImportAssets)
	if existingMappingFile != "" {
		existingMapping, err := LoadMapping(existingMappingFile)
		if err == nil {
			existingMappings = &matrix.ExistingMappings{
				Users:  existingMapping.Users,
				Spaces: existingMapping.Teams,
				Rooms:  existingMapping.Channels,
			}
		}
	}

	// Also check for latest mapping file in mappings directory
	if existingMappings == nil {
		latestMapping, _ := GetLatestMappingFile(o.config.Data.MappingsDir)
		if latestMapping != "" {
			existingMapping, err := LoadMapping(latestMapping)
			if err == nil {
				existingMappings = &matrix.ExistingMappings{
					Users:  existingMapping.Users,
					Spaces: existingMapping.Teams,
					Rooms:  existingMapping.Channels,
				}
			}
		}
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
	importer := matrix.NewImporter(o.mxClient)

	// Resolve how new users get a password ("auto" -> none when MAS handles authentication).
	passwordMode := matrix.PasswordModeRandom
	if o.config.GetUserPasswordMode() == config.UserPasswordModeNone {
		passwordMode = matrix.PasswordModeNone
	}
	importer.SetPasswordPolicy(matrix.PasswordPolicy{
		Mode:   passwordMode,
		Length: o.config.GetUserPasswordLength(),
	})
	if passwordMode == matrix.PasswordModeNone {
		logger.Info("User import: creating users without a password (SSO/MAS or admin reset required)")
	} else {
		logger.Info("User import: generating a random %d-character password per user", o.config.GetUserPasswordLength())
	}

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

	// Import direct message channels as Matrix DMs when enabled
	if o.config.Matrix.Import.ImportDirectMessages && len(assets.DirectChannels) > 0 {
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

	// Create mapping
	mapping := NewMapping(o.config.Matrix.Homeserver)
	mapping.MergeUsers(importResult.UserMapping)
	mapping.MergeTeams(importResult.SpaceMapping)
	mapping.MergeChannels(importResult.RoomMapping)

	// Save mapping
	mappingFile := GenerateMappingFilename(o.config.Data.MappingsDir)
	if err := SaveMapping(mapping, mappingFile); err != nil {
		o.state.FailStep(StepImportAssets, err)
		o.SaveState()
		return nil, fmt.Errorf("failed to save mapping: %w", err)
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
	if assetFile := o.state.GetStepOutputFile(StepExportAssets); assetFile != "" {
		var assets mattermost.Assets
		if err := archive.LoadGzipJSON(assetFile, &assets); err == nil {
			channels = append(assets.Channels, assets.DirectChannels...)
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
	importer := matrix.NewImporter(o.mxClient)

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
	teamStats, spacesToLeaveAfterMembershipImport, err := importer.ApplyTeamMemberships(memberships.TeamMembers, mapping.Users, mapping.Teams, defaultRoomOwnerID, importProgress)
	if err != nil {
		o.state.FailStep(StepImportMemberships, err)
		o.SaveState()
		return nil, fmt.Errorf("failed to apply team memberships: %w", err)
	}

	// Apply channel memberships
	if progress != nil {
		progress("channel_memberships", 0, len(memberships.ChannelMembers), "")
	}
	channelStats, err := importer.ApplyChannelMemberships(channels, memberships.ChannelMembers, mapping.Users, mapping.Channels, defaultChannelOwnerID, importProgress)
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
	MappingFile      string
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
	var msgMapping *MessageMapping

	if msgMappingFile != "" {
		msgMapping, err = LoadMessageMapping(msgMappingFile)
		if err != nil {
			logger.Warn("Failed to load existing message mapping, starting fresh: %v", err)
			msgMapping = NewMessageMapping(o.config.Matrix.Homeserver)
		} else {
			logger.Info("Resuming from existing mapping with %d messages", msgMapping.Count())
		}
	} else {
		msgMapping = NewMessageMapping(o.config.Matrix.Homeserver)
	}

	// Set up AS token if configured
	if o.config.UseAppService() {
		o.mxClient.SetASToken(o.config.GetASToken())
		logger.Info("Application Service token configured - messages will have original timestamps")
	} else {
		logger.Warn("No Application Service token - messages will be imported with current timestamps")
	}

	// Create importer
	importer := matrix.NewImporter(o.mxClient)

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
			fileConfig.RemoteReadFile = remoteExecutor.ReadFile
			logger.Info("Upload mode: Mattermost SSH file reader enabled for remote local_data_path")
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

	// Import messages with files
	result, err := importer.ImportMessagesWithFiles(
		messages.Posts,
		assetMapping.Channels, // channelID -> roomID
		assetMapping.Users,    // userID -> matrixUserID
		existingMapping,       // existing message mapping
		filesByPost,           // post ID -> files
		fileConfig,            // file migration settings
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
			logger.Warn("Message error categories: no_room=%d send_error=%d reply_error=%d parent_missing=%d other=%d",
				c["no_room"], c["send_error"], c["reply_error"], c["parent_missing"], c["other"])
		}
	}

	// Update message mapping with new imports, then save to the same file the checkpoints
	// have been writing so a run leaves exactly one mapping behind.
	addMessageEntries(msgMapping, result.Mapping, postByID, assetMapping)

	if err := SaveMessageMapping(msgMapping, mappingFile); err != nil {
		logger.Warn("Failed to save message mapping: %v", err)
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
		MappingFile:      mappingFile,
	}, nil
}
