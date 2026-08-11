package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
)

// Config represents the main configuration structure
type Config struct {
	Language   string           `mapstructure:"language"`
	Debug      bool             `mapstructure:"debug"` // Enable debug-level logging to migration.log
	Mattermost MattermostConfig `mapstructure:"mattermost"`
	Matrix     MatrixConfig     `mapstructure:"matrix"`
	Data       DataConfig       `mapstructure:"data"`

	// ConfigFile records which file the settings came from. Empty means none was found and
	// the built-in defaults are in effect, which is worth surfacing rather than assuming.
	ConfigFile string `mapstructure:"-"`
}

// MattermostConfig holds Mattermost server configuration
type MattermostConfig struct {
	SSH        SSHConfig      `mapstructure:"ssh"`
	ConfigPath string         `mapstructure:"config_path"` // Path to config.json on remote server
	Database   DatabaseConfig `mapstructure:"database"`    // Optional: manual override
	Files      FilesConfig    `mapstructure:"files"`       // File/attachment settings
	// IgnoredUsers: Mattermost usernames to skip during import (e.g., bot/service accounts).
	// Matching is case-insensitive.
	IgnoredUsers []string `mapstructure:"ignored_users"`
}

// FilesConfig holds file attachment migration settings
type FilesConfig struct {
	// S3 public URL prefix for direct linking (e.g., "https://s3.example.com/bucket")
	// If set, files will be linked directly instead of uploaded to Matrix
	S3PublicURL string `mapstructure:"s3_public_url"`

	// Local data path on Mattermost server (e.g., "/opt/mattermost/data")
	// Used for local file storage mode
	LocalDataPath string `mapstructure:"local_data_path"`

	// Migration mode: "link" (keep S3 URLs), "upload" (upload to Matrix), "skip" (no files)
	Mode string `mapstructure:"mode"`

	// Maximum file size to upload in MB (default: 50)
	// Files larger than this are treated as upload failures
	MaxUploadSizeMB int `mapstructure:"max_upload_size_mb"`

	// When true, upload failures may fall back to sending S3/public links (if s3_public_url is set).
	// When false (default), upload failures are skipped and logged as errors.
	FallbackToLinkOnUploadFailure bool `mapstructure:"fallback_to_link_on_upload_failure"`
}

// MatrixConfig holds Matrix server configuration
type MatrixConfig struct {
	SSH        SSHConfig        `mapstructure:"ssh"`
	API        APIConfig        `mapstructure:"api"`
	Auth       AuthConfig       `mapstructure:"auth"` // Username/password auth for Matrix API
	Homeserver string           `mapstructure:"homeserver"`
	RateLimit  RateLimitConfig  `mapstructure:"rate_limit"` // Rate limiting configuration
	AppService AppServiceConfig `mapstructure:"appservice"` // Application Service for message import
	MAS        MASConfig        `mapstructure:"mas"`        // Matrix Authentication Service for user creation
	Import     ImportConfig     `mapstructure:"import"`     // Room/space import options (owner and alias)
}

// ImportConfig holds options for importing rooms and spaces from Mattermost
type ImportConfig struct {
	// PreserveOwnerAndAlias: set room/space owner from Mattermost creator_id and set local alias (team+name)
	PreserveOwnerAndAlias bool `mapstructure:"preserve_owner_and_alias"`
	// FallbackRoomCreator: Matrix localpart (username before :domain) when creator_id is empty.
	// If this user does not exist on the server, the admin account (auth.username) is used.
	FallbackRoomCreator string `mapstructure:"fallback_room_creator"`
	// ForceJoin: when true, add users to rooms/spaces via Synapse admin API (joined directly).
	// When false, users are invited and must accept. Enable for migrations where users are already members.
	ForceJoin bool `mapstructure:"force_join"`
	// PublicRoomJoinRules: who can join public (Mattermost) rooms in Matrix.
	// "space_members" (default): only members of the parent space/team can join (restricted join rule).
	// "public": anyone can join (leave default Matrix join rule; room remains openly joinable).
	PublicRoomJoinRules string `mapstructure:"public_room_join_rules"`
	// SpaceVisibility: default visibility for Matrix spaces created from Mattermost teams.
	// "invite_only" (default): create private/invite-only spaces.
	// "public": create public/joinable spaces.
	// "from_mattermost": derive from team.Type (O=public, I=invite-only).
	SpaceVisibility string `mapstructure:"space_visibility"`
	// ImportDirectMessages: when true, export and import Mattermost direct message channels (D type) as Matrix DMs.
	// Rooms appear under "People" for both users with is_direct set. Requires Application Service for m.direct account_data.
	ImportDirectMessages bool `mapstructure:"import_direct_messages"`
	// ImportReactions: when true (the default), emoji reactions are imported as Matrix
	// m.reaction annotations after the messages they belong to. Each reaction costs one
	// rate-limited API call, so a busy instance can spend a noticeable part of the message
	// import on them; set false to trade the reactions for the time.
	ImportReactions bool `mapstructure:"import_reactions"`
	// UserPassword: how passwords are assigned to newly created Matrix users.
	UserPassword UserPasswordConfig `mapstructure:"user_password"`
}

// User password modes for ImportConfig.UserPassword.Mode
const (
	// UserPasswordModeAuto sets no password when MAS is enabled (accounts are SSO-only),
	// and generates a random one otherwise.
	UserPasswordModeAuto = "auto"
	// UserPasswordModeRandom always generates a distinct random password per user.
	UserPasswordModeRandom = "random"
	// UserPasswordModeNone never sets a password. Users can only authenticate via SSO/MAS
	// or an admin-initiated password reset.
	UserPasswordModeNone = "none"
	// UserPasswordModeLocalOnly generates a password only for users whose Mattermost account
	// had no SSO provider (auth_service empty). Use it for a mixed workspace where most
	// people sign in through the upstream IdP but a few local accounts have no identity
	// there. Requires password login to be enabled on the homeserver or in MAS.
	UserPasswordModeLocalOnly = "local_only"
)

// UserPasswordConfig controls password assignment for imported users.
type UserPasswordConfig struct {
	// Mode: "auto" (default), "random", "local_only", or "none".
	Mode string `mapstructure:"mode"`
	// Length of generated passwords. Default 24; valid range 12-128.
	Length int `mapstructure:"length"`
	// WriteFile: when true (default), generated passwords are written to
	// <assets_dir>/user-passwords-<timestamp>.csv with mode 0600 so they can be distributed.
	// Set false to discard them, leaving SSO or an admin reset as the only way in.
	WriteFile bool `mapstructure:"write_file"`
}

// MASConfig holds Matrix Authentication Service configuration
// When enabled, users are created via MAS Admin API so they can log in via SSO/OAuth
// without "Localpart not available" errors.
type MASConfig struct {
	Enabled         bool   `mapstructure:"enabled"`
	Endpoint        string `mapstructure:"endpoint"`          // MAS API base URL (e.g. http://mas.example.com:8080)
	ClientIDEnv     string `mapstructure:"client_id_env"`     // Env var for OAuth client ID (admin client)
	ClientSecretEnv string `mapstructure:"client_secret_env"` // Env var for OAuth client secret
}

// AppServiceConfig holds Application Service configuration for message import
type AppServiceConfig struct {
	Enabled    bool   `mapstructure:"enabled"`      // Enable AS mode for message import
	ASTokenEnv string `mapstructure:"as_token_env"` // Env var for AS token
	HSTokenEnv string `mapstructure:"hs_token_env"` // Env var for HS token (optional)
}

// RateLimitConfig holds rate limiting configuration for Matrix API
type RateLimitConfig struct {
	RequestsPerSecond float64 `mapstructure:"requests_per_second"` // Max requests per second (0 = no limit)
	MaxRetries        int     `mapstructure:"max_retries"`         // Max retries on 429 error
	RetryBaseDelay    int     `mapstructure:"retry_base_delay_ms"` // Base delay in ms for exponential backoff
}

// SSHConfig holds SSH connection configuration
type SSHConfig struct {
	Host          string `mapstructure:"host"`
	Port          int    `mapstructure:"port"`
	User          string `mapstructure:"user"`
	KeyPath       string `mapstructure:"key_path"`       // Optional: path to SSH key
	PassphraseEnv string `mapstructure:"passphrase_env"` // Optional: env var for key passphrase
	PasswordEnv   string `mapstructure:"password_env"`   // Optional: env var for SSH password
}

// DatabaseConfig holds PostgreSQL connection configuration (optional manual override)
type DatabaseConfig struct {
	Host        string `mapstructure:"host"`
	Port        int    `mapstructure:"port"`
	Name        string `mapstructure:"name"`
	User        string `mapstructure:"user"`
	PasswordEnv string `mapstructure:"password_env"`
	// SSLMode is the lib/pq sslmode for the database connection: "disable", "require",
	// "verify-ca" or "verify-full". Empty selects a default from the connection's host -
	// see ResolveDBSSLMode.
	SSLMode string `mapstructure:"ssl_mode"`
}

// Postgres sslmode values lib/pq accepts. It rejects libpq's "allow" and "prefer", so
// offering them here would only produce a connection error further along.
const (
	DBSSLModeDisable    = "disable"
	DBSSLModeRequire    = "require"
	DBSSLModeVerifyCA   = "verify-ca"
	DBSSLModeVerifyFull = "verify-full"
)

// ResolveDBSSLMode decides the sslmode for a database connection.
//
// Precedence is explicit config, then whatever Mattermost's own DataSource asked for
// (matching how Mattermost itself reaches the database), then a default taken from the
// host: connections that never leave the machine get "disable", anything else gets
// "require".
//
// The host-derived default is what keeps the password off the wire. A unix socket or a
// loopback address - which is also where an SSH tunnel terminates - cannot be observed by
// anything that isn't already on the host, so TLS there buys nothing and would break the
// common case of a PostgreSQL built without it. A remote host is the opposite: without TLS
// the password and the entire message history cross the network in cleartext.
func ResolveDBSSLMode(configured, discovered, host string) string {
	if configured != "" {
		return configured
	}
	if discovered != "" {
		return discovered
	}
	if isLocalDBHost(host) {
		return DBSSLModeDisable
	}
	return DBSSLModeRequire
}

// isLocalDBHost reports whether a connection to host stays on this machine. lib/pq treats a
// host beginning with "/" as the directory holding a unix socket.
func isLocalDBHost(host string) bool {
	if strings.HasPrefix(host, "/") {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	return false
}

// APIConfig holds Matrix API configuration
type APIConfig struct {
	BaseURL       string `mapstructure:"base_url"`
	AdminTokenEnv string `mapstructure:"admin_token_env"` // Optional: if provided, use this token
	Port          int    `mapstructure:"port"`            // Synapse API port (default: 8008)
}

// AuthConfig holds Matrix authentication configuration
type AuthConfig struct {
	Username    string `mapstructure:"username"`     // Admin username
	PasswordEnv string `mapstructure:"password_env"` // Env var for password
}

// DataConfig holds data storage paths
type DataConfig struct {
	AssetsDir   string `mapstructure:"assets_dir"`
	MappingsDir string `mapstructure:"mappings_dir"`
	StateFile   string `mapstructure:"state_file"`
}

// Load loads configuration from the specified file or default locations
func Load(cfgFile string) (*Config, error) {
	v := viper.New()

	// Set defaults
	setDefaults(v)

	if cfgFile != "" {
		// Use config file from the flag
		v.SetConfigFile(cfgFile)
	} else {
		// Search for config in current directory and home directory
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		for _, p := range configSearchPaths {
			v.AddConfigPath(p)
		}
	}

	// Read the config file
	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			// "Not found" is not always the truth. Viper resolves its search paths to
			// absolute ones, so a config file sitting in the working directory becomes
			// invisible when a directory *above* it cannot be traversed by the current user
			// - running under `sudo -u` from a checkout in /root, for instance. Falling back
			// to defaults there produces a run that looks configured and is not.
			if found := findOverlookedConfigFile(configSearchPaths); found != "" {
				return nil, fmt.Errorf("found %s but the config search could not use it; "+
					"this usually means a directory above it is not readable by the current user - "+
					"move the working copy somewhere that user can enter, or pass --config with an absolute path", found)
			}
			// Config file genuinely not found, use defaults
			return loadDefaults(v)
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// Unmarshal config
	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Expand paths
	cfg.expandPaths()
	cfg.ConfigFile = v.ConfigFileUsed()

	// Validate config
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return &cfg, nil
}

// setDefaults sets default configuration values
func setDefaults(v *viper.Viper) {
	v.SetDefault("language", "en")
	v.SetDefault("debug", false)
	v.SetDefault("mattermost.ssh.port", 22)
	v.SetDefault("mattermost.config_path", "/opt/mattermost/config/config.json")
	v.SetDefault("mattermost.database.host", "localhost")
	v.SetDefault("mattermost.database.port", 5432)
	v.SetDefault("mattermost.files.fallback_to_link_on_upload_failure", false)
	v.SetDefault("matrix.ssh.port", 22)
	v.SetDefault("matrix.api.base_url", "http://localhost:8008")
	v.SetDefault("matrix.api.port", 8008) // Synapse API port for SSH tunnel
	// Rate limiting defaults - conservative values to avoid 429 errors
	v.SetDefault("matrix.rate_limit.requests_per_second", 5.0)  // 5 req/sec (200ms between requests)
	v.SetDefault("matrix.rate_limit.max_retries", 5)            // 5 retries before giving up
	v.SetDefault("matrix.rate_limit.retry_base_delay_ms", 2000) // 2 second base delay
	v.SetDefault("matrix.import.force_join", false)
	v.SetDefault("matrix.import.public_room_join_rules", "space_members")
	v.SetDefault("matrix.import.space_visibility", "invite_only")
	v.SetDefault("matrix.import.import_reactions", true)
	v.SetDefault("matrix.import.user_password.mode", UserPasswordModeAuto)
	v.SetDefault("matrix.import.user_password.length", 24)
	v.SetDefault("matrix.import.user_password.write_file", true)
	v.SetDefault("data.assets_dir", "./data/assets")
	v.SetDefault("data.mappings_dir", "./data/mappings")
	v.SetDefault("data.state_file", "./data/state.json")
}

// loadDefaults creates a config with default values
func loadDefaults(v *viper.Viper) (*Config, error) {
	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal defaults: %w", err)
	}
	cfg.expandPaths()
	cfg.ConfigFile = ""
	return &cfg, nil
}

// configSearchPaths are the directories searched for config.yaml when --config is not given.
var configSearchPaths = []string{".", "$HOME/.matrixmigrate"}

// configFileNames are the file names viper would accept in those directories.
var configFileNames = []string{"config.yaml", "config.yml", "config.json", "config.toml"}

// findOverlookedConfigFile reports a config file that is reachable from here but that viper's
// search reported as missing. It probes the paths directly, which succeeds in cases viper's
// absolute-path resolution does not - notably a relative working directory whose ancestors
// are not traversable. Returns the path, or "" when there is genuinely nothing there.
func findOverlookedConfigFile(paths []string) string {
	for _, dir := range paths {
		dir = os.ExpandEnv(dir)
		if dir == "" {
			continue
		}
		for _, name := range configFileNames {
			candidate := filepath.Join(dir, name)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			} else if os.IsPermission(err) {
				return candidate
			}
		}
	}
	return ""
}

// expandPaths expands ~ and environment variables in paths
func (c *Config) expandPaths() {
	c.Mattermost.SSH.KeyPath = expandPath(c.Mattermost.SSH.KeyPath)
	c.Matrix.SSH.KeyPath = expandPath(c.Matrix.SSH.KeyPath)
	c.Data.AssetsDir = expandPath(c.Data.AssetsDir)
	c.Data.MappingsDir = expandPath(c.Data.MappingsDir)
	c.Data.StateFile = expandPath(c.Data.StateFile)
}

// expandPath expands ~ to home directory and resolves environment variables
func expandPath(path string) string {
	if path == "" {
		return path
	}

	// Expand ~ to home directory
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			path = filepath.Join(home, path[2:])
		}
	}

	// Expand environment variables
	path = os.ExpandEnv(path)

	return path
}

// Validate validates the configuration
func (c *Config) Validate() error {
	// Validate Mattermost config if SSH host is provided
	if c.Mattermost.SSH.Host != "" {
		if c.Mattermost.SSH.User == "" {
			return fmt.Errorf("mattermost.ssh.user is required")
		}
		// Either key_path or password_env must be provided
		hasKey := c.Mattermost.SSH.KeyPath != ""
		hasPassword := c.Mattermost.SSH.PasswordEnv != ""
		if !hasKey && !hasPassword {
			return fmt.Errorf("mattermost.ssh: either key_path or password_env is required")
		}
	}

	// Validate Matrix SSH config if SSH host is provided
	if c.Matrix.SSH.Host != "" {
		if c.Matrix.SSH.User == "" {
			return fmt.Errorf("matrix.ssh.user is required")
		}
		// Either key_path or password_env must be provided
		hasKey := c.Matrix.SSH.KeyPath != ""
		hasPassword := c.Matrix.SSH.PasswordEnv != ""
		if !hasKey && !hasPassword {
			return fmt.Errorf("matrix.ssh: either key_path or password_env is required")
		}
	}

	// Validate Matrix API access whenever a homeserver is configured. This has to hold in
	// direct mode too, where matrix.ssh is empty and the API is reached over its base_url.
	if c.Matrix.Homeserver != "" {
		// Check that either auth or admin token is provided
		hasAuth := c.Matrix.Auth.Username != "" && c.Matrix.Auth.PasswordEnv != ""
		hasToken := c.Matrix.API.AdminTokenEnv != ""
		if !hasAuth && !hasToken {
			return fmt.Errorf("matrix: either auth (username/password_env) or api.admin_token_env is required")
		}
	} else if c.Matrix.SSH.Host != "" {
		return fmt.Errorf("matrix.homeserver is required")
	}

	// Validate public_room_join_rules
	switch c.Matrix.Import.PublicRoomJoinRules {
	case "", "space_members", "public":
		// valid
	default:
		return fmt.Errorf("matrix.import.public_room_join_rules must be \"space_members\" or \"public\", got %q", c.Matrix.Import.PublicRoomJoinRules)
	}
	// Validate space_visibility
	switch c.Matrix.Import.SpaceVisibility {
	case "", "invite_only", "public", "from_mattermost":
		// valid
	default:
		return fmt.Errorf("matrix.import.space_visibility must be \"invite_only\", \"public\", or \"from_mattermost\", got %q", c.Matrix.Import.SpaceVisibility)
	}
	// Validate user_password
	switch c.Matrix.Import.UserPassword.Mode {
	case "", UserPasswordModeAuto, UserPasswordModeRandom, UserPasswordModeLocalOnly, UserPasswordModeNone:
		// valid
	default:
		return fmt.Errorf("matrix.import.user_password.mode must be %q, %q, %q, or %q, got %q",
			UserPasswordModeAuto, UserPasswordModeRandom, UserPasswordModeLocalOnly,
			UserPasswordModeNone, c.Matrix.Import.UserPassword.Mode)
	}
	if l := c.Matrix.Import.UserPassword.Length; l != 0 && (l < 12 || l > 128) {
		return fmt.Errorf("matrix.import.user_password.length must be between 12 and 128, got %d", l)
	}

	// Validate file attachment config. Without this the run gets all the way to message
	// import before failing once per file, having already created users, rooms and
	// memberships.
	switch c.Mattermost.Files.Mode {
	case "", "link", "upload", "skip":
		// valid
	default:
		return fmt.Errorf("mattermost.files.mode must be \"link\", \"upload\" or \"skip\", got %q", c.Mattermost.Files.Mode)
	}
	if c.GetFileMode() == "upload" && c.Mattermost.Files.LocalDataPath == "" {
		return fmt.Errorf("mattermost.files.local_data_path is required when mattermost.files.mode is \"upload\"")
	}

	// Validate the database sslmode against what lib/pq accepts, rather than letting an
	// unsupported value surface as a connection error at the first query.
	switch c.Mattermost.Database.SSLMode {
	case "", DBSSLModeDisable, DBSSLModeRequire, DBSSLModeVerifyCA, DBSSLModeVerifyFull:
		// valid
	default:
		return fmt.Errorf("mattermost.database.ssl_mode must be %q, %q, %q or %q, got %q",
			DBSSLModeDisable, DBSSLModeRequire, DBSSLModeVerifyCA, DBSSLModeVerifyFull,
			c.Mattermost.Database.SSLMode)
	}

	// Validate MAS config when enabled
	if c.Matrix.MAS.Enabled {
		if c.Matrix.MAS.Endpoint == "" {
			return fmt.Errorf("matrix.mas.endpoint is required when mas is enabled")
		}
		if c.Matrix.MAS.ClientIDEnv == "" || c.Matrix.MAS.ClientSecretEnv == "" {
			return fmt.Errorf("matrix.mas.client_id_env and matrix.mas.client_secret_env are required when mas is enabled")
		}
	}

	return nil
}

// HasManualDatabaseConfig returns true if database config is manually specified
func (c *Config) HasManualDatabaseConfig() bool {
	return c.Mattermost.Database.Host != "" &&
		c.Mattermost.Database.Name != "" &&
		c.Mattermost.Database.User != ""
}

// GetMattermostDBPassword returns the Mattermost database password from environment
func (c *Config) GetMattermostDBPassword() string {
	if c.Mattermost.Database.PasswordEnv == "" {
		return ""
	}
	return os.Getenv(c.Mattermost.Database.PasswordEnv)
}

// GetMatrixAdminToken returns the Matrix admin token from environment
func (c *Config) GetMatrixAdminToken() string {
	if c.Matrix.API.AdminTokenEnv == "" {
		return ""
	}
	return os.Getenv(c.Matrix.API.AdminTokenEnv)
}

// GetMatrixPassword returns the Matrix password from environment
func (c *Config) GetMatrixPassword() string {
	if c.Matrix.Auth.PasswordEnv == "" {
		return ""
	}
	return os.Getenv(c.Matrix.Auth.PasswordEnv)
}

// UseTokenAuth returns true if admin token should be used instead of login
func (c *Config) UseTokenAuth() bool {
	return c.GetMatrixAdminToken() != ""
}

// GetPublicRoomJoinRules returns the effective public room join rules: "space_members" or "public".
// Empty config is treated as "space_members" (default).
func (c *Config) GetPublicRoomJoinRules() string {
	s := c.Matrix.Import.PublicRoomJoinRules
	if s == "" {
		return "space_members"
	}
	return s
}

// GetSpaceVisibility returns the default space visibility mode.
// Empty config is treated as "invite_only" (Mattermost teams are invite-only by default).
func (c *Config) GetSpaceVisibility() string {
	s := c.Matrix.Import.SpaceVisibility
	if s == "" {
		return "invite_only"
	}
	return s
}

// GetUserPasswordMode resolves matrix.import.user_password.mode to a concrete mode:
// UserPasswordModeRandom, UserPasswordModeLocalOnly, or UserPasswordModeNone.
//
// "auto" (the default) resolves to "none" when MAS is enabled, because those accounts
// authenticate through SSO and a local password would be dead weight; otherwise it resolves
// to "random". "local_only" is passed through — it is the middle ground for a workspace
// where only some users have an upstream identity.
func (c *Config) GetUserPasswordMode() string {
	switch c.Matrix.Import.UserPassword.Mode {
	case UserPasswordModeRandom:
		return UserPasswordModeRandom
	case UserPasswordModeLocalOnly:
		return UserPasswordModeLocalOnly
	case UserPasswordModeNone:
		return UserPasswordModeNone
	default: // "" or "auto"
		if c.Matrix.MAS.Enabled {
			return UserPasswordModeNone
		}
		return UserPasswordModeRandom
	}
}

// GetUserPasswordLength returns the configured generated-password length, or 24 when unset.
func (c *Config) GetUserPasswordLength() int {
	if l := c.Matrix.Import.UserPassword.Length; l != 0 {
		return l
	}
	return 24
}

// GetSSHKeyPassphrase returns the SSH key passphrase from environment
func (c *Config) GetSSHKeyPassphrase(server string) string {
	var envVar string
	switch server {
	case "mattermost":
		envVar = c.Mattermost.SSH.PassphraseEnv
	case "matrix":
		envVar = c.Matrix.SSH.PassphraseEnv
	}
	if envVar == "" {
		return ""
	}
	return os.Getenv(envVar)
}

// GetSSHPassword returns the SSH password from environment
func (c *Config) GetSSHPassword(server string) string {
	var envVar string
	switch server {
	case "mattermost":
		envVar = c.Mattermost.SSH.PasswordEnv
	case "matrix":
		envVar = c.Matrix.SSH.PasswordEnv
	}
	if envVar == "" {
		return ""
	}
	return os.Getenv(envVar)
}

// EnsureDataDirs creates data directories if they don't exist
func (c *Config) EnsureDataDirs() error {
	dirs := []string{c.Data.AssetsDir, c.Data.MappingsDir}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	// Ensure state file directory exists
	stateDir := filepath.Dir(c.Data.StateFile)
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return fmt.Errorf("failed to create state directory %s: %w", stateDir, err)
	}

	return nil
}


// MatrixAPIURL returns the full Matrix API base URL
func (c *Config) MatrixAPIURL() string {
	return strings.TrimSuffix(c.Matrix.API.BaseURL, "/")
}

// FormatUserID formats a username as a Matrix user ID
func (c *Config) FormatUserID(username string) string {
	return fmt.Sprintf("@%s:%s", username, c.Matrix.Homeserver)
}

// GetASToken returns the Application Service token from environment
func (c *Config) GetASToken() string {
	if c.Matrix.AppService.ASTokenEnv == "" {
		return ""
	}
	return os.Getenv(c.Matrix.AppService.ASTokenEnv)
}

// GetHSToken returns the Homeserver token from environment
func (c *Config) GetHSToken() string {
	if c.Matrix.AppService.HSTokenEnv == "" {
		return ""
	}
	return os.Getenv(c.Matrix.AppService.HSTokenEnv)
}

// UseAppService returns true if Application Service mode is enabled
func (c *Config) UseAppService() bool {
	return c.Matrix.AppService.Enabled && c.GetASToken() != ""
}

// UseMAS returns true if Matrix Authentication Service is enabled for user creation
func (c *Config) UseMAS() bool {
	return c.Matrix.MAS.Enabled && c.Matrix.MAS.Endpoint != "" &&
		c.GetMASClientID() != "" && c.GetMASClientSecret() != ""
}

// GetMASClientID returns the MAS OAuth client ID from environment
func (c *Config) GetMASClientID() string {
	if c.Matrix.MAS.ClientIDEnv == "" {
		return ""
	}
	return os.Getenv(c.Matrix.MAS.ClientIDEnv)
}

// GetMASClientSecret returns the MAS OAuth client secret from environment
func (c *Config) GetMASClientSecret() string {
	if c.Matrix.MAS.ClientSecretEnv == "" {
		return ""
	}
	return os.Getenv(c.Matrix.MAS.ClientSecretEnv)
}

// GetFileMode returns the file migration mode (link, upload, or skip)
func (c *Config) GetFileMode() string {
	mode := c.Mattermost.Files.Mode
	if mode == "" {
		// Default: link if S3 URL is set, otherwise skip
		if c.Mattermost.Files.S3PublicURL != "" {
			return "link"
		}
		return "skip"
	}
	return mode
}

// GetFileURL returns the public URL for a file path
// Returns empty string if no public URL is configured
func (c *Config) GetFileURL(filePath string) string {
	if c.Mattermost.Files.S3PublicURL == "" {
		return ""
	}
	baseURL := strings.TrimSuffix(c.Mattermost.Files.S3PublicURL, "/")
	return fmt.Sprintf("%s/%s", baseURL, filePath)
}

// GetMaxUploadSize returns the maximum file size for upload in bytes
func (c *Config) GetMaxUploadSize() int64 {
	maxMB := c.Mattermost.Files.MaxUploadSizeMB
	if maxMB <= 0 {
		maxMB = 50 // Default 50MB
	}
	return int64(maxMB) * 1024 * 1024
}

// ShouldUploadFile returns true if the file should be uploaded to Matrix
func (c *Config) ShouldUploadFile(fileSize int64) bool {
	mode := c.GetFileMode()
	if mode == "skip" {
		return false
	}
	if mode == "link" {
		return false
	}
	// mode == "upload"
	return fileSize <= c.GetMaxUploadSize()
}
