package matrix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aligundogdu/matrixmigrate/internal/logger"
)

// RateLimitConfig holds rate limiting settings
type RateLimitConfig struct {
	RequestsPerSecond float64       // Max requests per second (0 = no limit)
	MaxRetries        int           // Max retries on 429 error
	RetryBaseDelay    time.Duration // Base delay for exponential backoff
}

// DefaultRateLimitConfig returns default rate limiting settings
func DefaultRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		RequestsPerSecond: 5.0,             // 5 req/sec
		MaxRetries:        5,               // 5 retries
		RetryBaseDelay:    2 * time.Second, // 2 second base delay
	}
}

// Client represents a Matrix API client
type Client struct {
	baseURL    string
	adminToken string
	httpClient *http.Client
	homeserver string

	// Matrix Authentication Service - when set, user creation uses MAS instead of Synapse
	masClient *MASClient

	// Application Service support
	asToken string // AS token for message import with timestamps

	// Rate limiting
	lastRequest    time.Time
	rateLimit      time.Duration
	maxRetries     int
	retryBaseDelay time.Duration
	mu             sync.Mutex

	// Transaction ID counter for messages
	txnCounter int64

	// ForceJoin: when true, add users via Synapse admin API (joined directly) instead of invite
	forceJoin bool

	// joinedRooms: room IDs the admin has already joined (Synapse admin join API requires admin in room)
	joinedRooms   map[string]struct{}
	joinedRoomsMu sync.Mutex

	// roomCreators caches room ID -> creator user ID, looked up when writing power levels
	roomCreators   map[string]string
	roomCreatorsMu sync.Mutex
}

// roomCreator returns the user that created roomID, or "" when it cannot be determined.
// Results are cached because power levels are written repeatedly for the same rooms.
func (c *Client) roomCreator(roomID string) string {
	c.roomCreatorsMu.Lock()
	if creator, ok := c.roomCreators[roomID]; ok {
		c.roomCreatorsMu.Unlock()
		return creator
	}
	c.roomCreatorsMu.Unlock()

	creator := ""
	endpoint := "/_synapse/admin/v1/rooms/" + url.PathEscape(roomID)
	body, statusCode, err := c.doRequest("GET", endpoint, nil)
	if err == nil && statusCode == http.StatusOK {
		var resp struct {
			Creator string `json:"creator"`
		}
		if json.Unmarshal(body, &resp) == nil {
			creator = resp.Creator
		}
	} else if err != nil {
		logger.Debug("roomCreator: could not look up creator of %s: %v", roomID, err)
	}

	c.roomCreatorsMu.Lock()
	if c.roomCreators == nil {
		c.roomCreators = make(map[string]string)
	}
	c.roomCreators[roomID] = creator
	c.roomCreatorsMu.Unlock()
	return creator
}

// NewClient creates a new Matrix API client with default rate limiting
func NewClient(baseURL, adminToken, homeserver string) *Client {
	return NewClientWithRateLimit(baseURL, adminToken, homeserver, DefaultRateLimitConfig())
}

// NewClientWithRateLimit creates a new Matrix API client with custom rate limiting
func NewClientWithRateLimit(baseURL, adminToken, homeserver string, rlConfig RateLimitConfig) *Client {
	var rateLimit time.Duration
	if rlConfig.RequestsPerSecond > 0 {
		rateLimit = time.Duration(float64(time.Second) / rlConfig.RequestsPerSecond)
	}

	maxRetries := rlConfig.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 5
	}

	retryBaseDelay := rlConfig.RetryBaseDelay
	if retryBaseDelay <= 0 {
		retryBaseDelay = 2 * time.Second
	}

	return &Client{
		baseURL:    baseURL,
		adminToken: adminToken,
		homeserver: homeserver,
		httpClient: &http.Client{
			// Synapse can take well over 30s to answer createRoom while an import is
			// hammering it, and a client-side timeout there is expensive: the server
			// finishes the create regardless, so the room exists but we never see its ID.
			Timeout: 120 * time.Second,
		},
		rateLimit:      rateLimit,
		maxRetries:     maxRetries,
		retryBaseDelay: retryBaseDelay,
	}
}

// SetHomeserver updates the homeserver domain
func (c *Client) SetHomeserver(homeserver string) {
	c.homeserver = homeserver
	if c.masClient != nil {
		c.masClient.SetHomeserver(homeserver)
	}
}

// SetMASClient sets the Matrix Authentication Service client for user creation.
// When set, CreateUser and UserExists use MAS instead of Synapse Admin API.
func (c *Client) SetMASClient(mas *MASClient) {
	c.masClient = mas
	if mas != nil {
		mas.SetHomeserver(c.homeserver)
	}
}

// GetHomeserver returns the current homeserver domain
func (c *Client) GetHomeserver() string {
	return c.homeserver
}

// SetForceJoin sets whether to add users via Synapse admin API (force-join) instead of invite.
func (c *Client) SetForceJoin(forceJoin bool) {
	c.forceJoin = forceJoin
}

// ForceJoinEnabled returns true if force-join mode is enabled.
func (c *Client) ForceJoinEnabled() bool {
	return c.forceJoin
}

// DetectHomeserver detects the homeserver from the authenticated user ID
// Returns the detected homeserver or error
func (c *Client) DetectHomeserver() (string, error) {
	resp, err := c.WhoAmI()
	if err != nil {
		return "", fmt.Errorf("failed to get current user: %w", err)
	}

	// Parse homeserver from user ID (format: @user:homeserver)
	userID := resp.UserID
	if userID == "" {
		return "", fmt.Errorf("no user_id in response")
	}

	// Find the : separator
	idx := strings.Index(userID, ":")
	if idx == -1 {
		return "", fmt.Errorf("invalid user_id format: %s", userID)
	}

	homeserver := userID[idx+1:]
	if homeserver == "" {
		return "", fmt.Errorf("empty homeserver in user_id: %s", userID)
	}

	logger.Info("Detected homeserver from user ID '%s': %s", userID, homeserver)
	return homeserver, nil
}

// doRequest performs an HTTP request to the Matrix API with rate limiting
func (c *Client) doRequest(method, endpoint string, body interface{}) ([]byte, int, error) {
	return c.doRequestWithRetry(method, endpoint, body, 0)
}

// doRequestWithRetry performs an HTTP request with retry logic for rate limiting
func (c *Client) doRequestWithRetry(method, endpoint string, body interface{}, retryCount int) ([]byte, int, error) {
	// Rate limiting: ensure minimum time between requests
	c.mu.Lock()
	if c.rateLimit > 0 {
		elapsed := time.Since(c.lastRequest)
		if elapsed < c.rateLimit {
			sleepTime := c.rateLimit - elapsed
			time.Sleep(sleepTime)
		}
	}
	c.lastRequest = time.Now()
	c.mu.Unlock()

	var reqBody io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(jsonBody)
	}

	reqURL := c.baseURL + endpoint
	req, err := http.NewRequest(method, reqURL, reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.adminToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if shouldRetryRequestErr(err) && isNonIdempotent(method, endpoint) {
			logger.Warn("Request transport error on %s; not retrying (room may have been created): %v", endpoint, err)
			return nil, 0, fmt.Errorf("%w: %v", ErrCreateRoomTimeout, err)
		}
		if shouldRetryRequestErr(err) && retryCount < c.maxRetries {
			retryAfter := retryDelay(c.retryBaseDelay, retryCount)
			logger.Warn("Request transport error, retrying in %v (%d/%d): %v", retryAfter, retryCount+1, c.maxRetries, err)
			time.Sleep(retryAfter)
			return c.doRequestWithRetry(method, endpoint, body, retryCount+1)
		}
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("failed to read response body: %w", err)
	}

	// Handle rate limiting (429) with exponential backoff
	if resp.StatusCode == http.StatusTooManyRequests {
		if retryCount >= c.maxRetries {
			return nil, resp.StatusCode, fmt.Errorf("rate limit exceeded after %d retries", c.maxRetries)
		}

		// Try to use Retry-After header if present
		var retryAfter time.Duration
		if retryAfterStr := resp.Header.Get("Retry-After"); retryAfterStr != "" {
			// Retry-After can be in seconds (integer) or HTTP-date format
			if seconds, err := strconv.Atoi(retryAfterStr); err == nil {
				retryAfter = time.Duration(seconds) * time.Second
			}
		}

		// If no Retry-After header, use exponential backoff
		if retryAfter == 0 {
			// Exponential backoff: base * 2^retryCount (e.g., 2s, 4s, 8s, 16s, 32s)
			retryAfter = c.retryBaseDelay * time.Duration(1<<uint(retryCount))
		}

		// Cap the delay at 60 seconds
		if retryAfter > 60*time.Second {
			retryAfter = 60 * time.Second
		}

		logger.Warn("Rate limit hit (429), waiting %v before retry %d/%d", retryAfter, retryCount+1, c.maxRetries)
		time.Sleep(retryAfter)

		// Retry
		return c.doRequestWithRetry(method, endpoint, body, retryCount+1)
	}

	return respBody, resp.StatusCode, nil
}

// WhoAmI returns the current user ID for the admin token
func (c *Client) WhoAmI() (*WhoAmIResponse, error) {
	body, statusCode, err := c.doRequest("GET", "/_matrix/client/v3/account/whoami", nil)
	if err != nil {
		return nil, err
	}

	var resp WhoAmIResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("API error: %s - %s", resp.Errcode, resp.Error)
	}

	return &resp, nil
}

// TestConnection tests the API connection
func (c *Client) TestConnection() error {
	_, err := c.WhoAmI()
	return err
}

// VerifyASToken checks that the homeserver accepts the Application Service token and
// returns the user ID it authenticates as. A token that is merely present but not
// registered on the homeserver fails every AS-path call (room creation, message import)
// with M_UNKNOWN_TOKEN, so this is checked up front rather than per item.
func (c *Client) VerifyASToken() (string, error) {
	if c.asToken == "" {
		return "", fmt.Errorf("no application service token configured")
	}

	body, statusCode, err := c.doRequestWithToken("GET", "/_matrix/client/v3/account/whoami", nil, c.asToken)
	if err != nil {
		return "", err
	}

	var resp WhoAmIResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	if statusCode != http.StatusOK {
		return "", fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}

	return resp.UserID, nil
}

// CreateUser creates or updates a user via the Admin API (or MAS when configured)
func (c *Client) CreateUser(username string, req *CreateUserRequest) (*UserResponse, error) {
	if c.masClient != nil {
		resp, err := c.masClient.CreateUser(username, req)
		if err != nil {
			return nil, err
		}
		// MAS only stores the user; set display name and email on Synapse
		if resp != nil && (req != nil && (req.DisplayName != "" || req.Email != "")) {
			if err := c.updateUserProfile(resp.UserID, req.DisplayName, req.Email); err != nil {
				logger.Warn("Failed to set display name/email for %s: %v", resp.UserID, err)
			}
		}
		if resp != nil && req != nil && req.Deactivated {
			if err := c.SetUserDeactivated(resp.UserID, true); err != nil {
				logger.Warn("Failed to set deactivated (locked) for %s: %v", resp.UserID, err)
			}
		}
		return resp, nil
	}
	userID := fmt.Sprintf("@%s:%s", username, c.homeserver)
	endpoint := fmt.Sprintf("/_synapse/admin/v2/users/%s", url.PathEscape(userID))

	logger.Info("Creating user: %s (endpoint: %s)", username, endpoint)

	body, statusCode, err := c.doRequest("PUT", endpoint, req)
	if err != nil {
		logger.Error("HTTP request failed for user '%s': %v", username, err)
		return nil, err
	}

	logger.Info("CreateUser response for '%s': status=%d", username, statusCode)

	var resp UserResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		logger.Error("Failed to parse response for user '%s': %v (body: %s)", username, err, string(body))
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if statusCode != http.StatusOK && statusCode != http.StatusCreated {
		// Check if user already exists (some Matrix servers return different codes)
		if resp.Errcode == "M_USER_IN_USE" || strings.Contains(resp.Error, "already exists") {
			logger.Info("User '%s' already exists (status=%d), treating as success", username, statusCode)
			resp.UserID = userID
			return &resp, nil
		}
		logger.Error("API error for user '%s': status=%d, errcode=%s, error=%s", username, statusCode, resp.Errcode, resp.Error)
		return nil, fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}

	resp.UserID = userID
	return &resp, nil
}

// GetUser gets user info via the Admin API
func (c *Client) GetUser(userID string) (*UserResponse, error) {
	endpoint := fmt.Sprintf("/_synapse/admin/v2/users/%s", url.PathEscape(userID))

	body, statusCode, err := c.doRequest("GET", endpoint, nil)
	if err != nil {
		return nil, err
	}

	var resp UserResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if statusCode == http.StatusNotFound {
		return nil, nil // User doesn't exist
	}

	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}

	return &resp, nil
}

// SetUserDeactivated sets or clears the deactivated (locked) flag on Synapse for a user.
// Deactivated users appear as locked in tools like synapse-admin and cannot log in.
func (c *Client) SetUserDeactivated(userID string, deactivated bool) error {
	endpoint := fmt.Sprintf("/_synapse/admin/v2/users/%s", url.PathEscape(userID))
	body := map[string]interface{}{"deactivated": deactivated}
	_, statusCode, err := c.doRequest("PUT", endpoint, body)
	if err != nil {
		return err
	}
	if statusCode != http.StatusOK && statusCode != http.StatusCreated {
		return fmt.Errorf("SetUserDeactivated returned status %d", statusCode)
	}
	logger.Info("SetUserDeactivated: %s -> %v", userID, deactivated)
	return nil
}

// updateUserProfile sets display name and email (threepids) on Synapse for a user.
// Used after MAS user creation so the profile is stored on the homeserver.
func (c *Client) updateUserProfile(userID, displayName, email string) error {
	if displayName == "" && email == "" {
		return nil
	}
	endpoint := fmt.Sprintf("/_synapse/admin/v2/users/%s", url.PathEscape(userID))
	body := make(map[string]interface{})
	if displayName != "" {
		body["displayname"] = displayName
	}
	if email != "" {
		body["threepids"] = []map[string]string{
			{"medium": "email", "address": email},
		}
	}
	_, statusCode, err := c.doRequest("PUT", endpoint, body)
	if err != nil {
		return err
	}
	if statusCode != http.StatusOK && statusCode != http.StatusCreated {
		return fmt.Errorf("profile update returned status %d", statusCode)
	}
	return nil
}

// UserExists checks if a user exists
func (c *Client) UserExists(username string) (bool, error) {
	if c.masClient != nil {
		return c.masClient.UserExists(username)
	}
	userID := fmt.Sprintf("@%s:%s", username, c.homeserver)
	logger.Info("Checking if user exists: %s", userID)
	user, err := c.GetUser(userID)
	if err != nil {
		logger.Error("UserExists check failed for '%s': %v", username, err)
		return false, err
	}
	exists := user != nil
	logger.Info("User '%s' exists: %v", username, exists)
	return exists, nil
}

// CreateRoom creates a new room (as the authenticated user, i.e. admin or AS sender).
// To create the room as a specific user (so they are the creator), use CreateRoomAsUser.
func (c *Client) CreateRoom(req *CreateRoomRequest) (*CreateRoomResponse, error) {
	return c.CreateRoomAsUser(req, "")
}

// CreateRoomAsUser creates a new room. When creatorUserID is non-empty and an AS token is set,
// the request is made with the AS token and ?user_id=creatorUserID so the room creator is that user.
// Otherwise the room is created as the current authenticated user (admin).
func (c *Client) CreateRoomAsUser(req *CreateRoomRequest, creatorUserID string) (*CreateRoomResponse, error) {
	endpoint := "/_matrix/client/v3/createRoom"
	token := c.adminToken
	if creatorUserID != "" && c.asToken != "" {
		token = c.asToken
		params := url.Values{}
		params.Set("user_id", creatorUserID)
		endpoint = endpoint + "?" + params.Encode()
		logger.Info("CreateRoom: using AS token with user_id=%s (room creator will be this user)", creatorUserID)
	} else if creatorUserID != "" {
		logger.Info("CreateRoom: no AS token; creating as admin then will invite and set power level for owner %s", creatorUserID)
	}
	body, statusCode, err := c.doRequestWithToken("POST", endpoint, req, token)
	if err != nil {
		return nil, err
	}

	var resp CreateRoomResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}

	return &resp, nil
}

// ResolveRoomAlias returns the room ID behind a full room alias (e.g. #general:example.com).
func (c *Client) ResolveRoomAlias(alias string) (string, error) {
	endpoint := "/_matrix/client/v3/directory/room/" + url.PathEscape(alias)
	body, statusCode, err := c.doRequest("GET", endpoint, nil)
	if err != nil {
		return "", err
	}

	var resp struct {
		RoomID  string `json:"room_id"`
		Errcode string `json:"errcode"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}
	if statusCode != http.StatusOK || resp.RoomID == "" {
		return "", fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}
	return resp.RoomID, nil
}

// recoverRoomByAlias finds the room behind roomAliasLocalPart after a create attempt failed in a
// way that means the room may already exist. Returns nil when there is no alias to resolve or the
// alias does not resolve, so the caller can surface the original error.
func (c *Client) recoverRoomByAlias(roomAliasLocalPart string, cause error) *CreateRoomResponse {
	if roomAliasLocalPart == "" || c.homeserver == "" {
		return nil
	}
	alias := fmt.Sprintf("#%s:%s", roomAliasLocalPart, c.homeserver)
	roomID, err := c.ResolveRoomAlias(alias)
	if err != nil {
		logger.Warn("recoverRoomByAlias: %s did not resolve after %v: %v", alias, cause, err)
		return nil
	}
	logger.Info("recoverRoomByAlias: reusing existing room %s for alias %s (create reported: %v)", roomID, alias, cause)
	return &CreateRoomResponse{RoomID: roomID}
}

// joinedRoomIDs lists the rooms userID has joined, asked as that user via the AS token.
func (c *Client) joinedRoomIDs(userID string) ([]string, error) {
	params := url.Values{}
	params.Set("user_id", userID)
	endpoint := "/_matrix/client/v3/joined_rooms?" + params.Encode()

	body, statusCode, err := c.doRequestWithToken("GET", endpoint, nil, c.asToken)
	if err != nil {
		return nil, err
	}
	var resp struct {
		JoinedRooms []string `json:"joined_rooms"`
		Errcode     string   `json:"errcode"`
		Error       string   `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}
	return resp.JoinedRooms, nil
}

// joinedMemberIDs lists the joined members of roomID, asked as userID via the AS token.
func (c *Client) joinedMemberIDs(roomID, userID string) ([]string, error) {
	params := url.Values{}
	params.Set("user_id", userID)
	endpoint := fmt.Sprintf("/_matrix/client/v3/rooms/%s/joined_members?%s", url.PathEscape(roomID), params.Encode())

	body, statusCode, err := c.doRequestWithToken("GET", endpoint, nil, c.asToken)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Joined  map[string]interface{} `json:"joined"`
		Errcode string                 `json:"errcode"`
		Error   string                 `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}
	members := make([]string, 0, len(resp.Joined))
	for id := range resp.Joined {
		members = append(members, id)
	}
	return members, nil
}

// findDirectRoomByMembers looks for an existing two-person room holding exactly creatorUserID and
// otherUserID. A DM carries no alias, so this membership scan is the only way to find a room that
// a timed-out create left behind. Returns "" when there is no such room or no AS token to ask with.
func (c *Client) findDirectRoomByMembers(creatorUserID, otherUserID string) string {
	if c.asToken == "" {
		return ""
	}
	rooms, err := c.joinedRoomIDs(creatorUserID)
	if err != nil {
		logger.Warn("findDirectRoomByMembers: could not list rooms for %s: %v", creatorUserID, err)
		return ""
	}
	for _, roomID := range rooms {
		members, err := c.joinedMemberIDs(roomID, creatorUserID)
		if err != nil || len(members) != 2 {
			continue
		}
		if (members[0] == creatorUserID && members[1] == otherUserID) ||
			(members[1] == creatorUserID && members[0] == otherUserID) {
			return roomID
		}
	}
	return ""
}

// CreateSpace creates a new space (a room with m.space type).
// roomAliasLocalPart is optional; when set, the room gets #roomAliasLocalPart:homeserver.
// ownerUserID is optional; when set, that user is the room creator (using AS token with user_id when available).
// When appservice is enabled, the alias is sent; AS registration must include alias_namespaces for your domain.
func (c *Client) CreateSpace(name, topic string, public bool, roomAliasLocalPart, ownerUserID string) (*CreateRoomResponse, error) {
	visibility := VisibilityPrivate
	preset := PresetPrivateChat
	if public {
		visibility = VisibilityPublic
		preset = PresetPublicChat
	}

	req := &CreateRoomRequest{
		Name:       name,
		Topic:      topic,
		Visibility: string(visibility),
		Preset:     string(preset),
		CreationContent: map[string]interface{}{
			"type": SpaceType,
		},
	}
	if roomAliasLocalPart != "" {
		req.RoomAliasName = roomAliasLocalPart
	}

	// When AS token is set and ownerUserID is set, create as that user so they are the actual creator.
	// Otherwise create as admin and then invite/set power levels.
	if ownerUserID != "" {
		logger.Info("CreateSpace: name=%q alias=%q owner=%s", name, req.RoomAliasName, ownerUserID)
	}
	resp, err := c.CreateRoomAsUser(req, ownerUserID)
	if err != nil && strings.Contains(err.Error(), "M_EXCLUSIVE") && req.RoomAliasName != "" {
		logger.Warn("CreateSpace: M_EXCLUSIVE for alias %q (AS alias namespace may need exclusive: true or regex fix); retrying without alias", req.RoomAliasName)
		reqNoAlias := *req
		reqNoAlias.RoomAliasName = ""
		resp, err = c.CreateRoomAsUser(&reqNoAlias, ownerUserID)
	}
	if roomMayExist(err) {
		// Fall through rather than return: a recovered space still needs the owner and admin
		// power levels below, which linking rooms into it later depends on.
		if existing := c.recoverRoomByAlias(req.RoomAliasName, err); existing != nil {
			resp, err = existing, nil
		}
	}
	if err != nil {
		return nil, err
	}
	if ownerUserID != "" && !c.createdAsUser(ownerUserID) {
		logger.Info("CreateSpace: room %s created as admin; inviting %s and setting power level 100", resp.RoomID, ownerUserID)
		if err := c.ensureRoomOwner(resp.RoomID, ownerUserID); err != nil {
			logger.Warn("CreateSpace: could not set room owner %s: %v", ownerUserID, err)
		} else {
			logger.Info("CreateSpace: owner %s invited and set to power level 100", ownerUserID)
		}
	}
	if ownerUserID != "" && c.createdAsUser(ownerUserID) && c.asToken != "" {
		// Space was created as owner (AS); add admin with PL 50 so linking rooms to space works later.
		if err := c.ensureAdminInSpaceWithPower(resp.RoomID, ownerUserID, 50); err != nil {
			logger.Warn("CreateSpace: could not add admin to space for linking: %v", err)
		}
	}
	return resp, nil
}

// CreateRegularRoom creates a regular room (not a space).
// roomAliasLocalPart is optional; when set, the room gets #roomAliasLocalPart:homeserver.
// ownerUserID is optional; when set, that user is the room creator (using AS token with user_id when available).
// When appservice is enabled, the alias is sent; AS registration must include alias_namespaces for your domain.
func (c *Client) CreateRegularRoom(name, topic string, public bool, roomAliasLocalPart, ownerUserID string) (*CreateRoomResponse, error) {
	visibility := VisibilityPrivate
	preset := PresetPrivateChat
	if public {
		visibility = VisibilityPublic
		preset = PresetPublicChat
	}

	req := &CreateRoomRequest{
		Name:       name,
		Topic:      topic,
		Visibility: string(visibility),
		Preset:     string(preset),
	}
	if roomAliasLocalPart != "" {
		req.RoomAliasName = roomAliasLocalPart
	}

	// When AS token is set and ownerUserID is set, create as that user so they are the actual creator.
	if ownerUserID != "" {
		logger.Info("CreateRegularRoom: name=%q alias=%q owner=%s", name, req.RoomAliasName, ownerUserID)
	}
	resp, err := c.CreateRoomAsUser(req, ownerUserID)
	if err != nil && strings.Contains(err.Error(), "M_EXCLUSIVE") && req.RoomAliasName != "" {
		logger.Warn("CreateRegularRoom: M_EXCLUSIVE for alias %q (AS alias namespace may need exclusive: true or regex fix); retrying without alias", req.RoomAliasName)
		reqNoAlias := *req
		reqNoAlias.RoomAliasName = ""
		resp, err = c.CreateRoomAsUser(&reqNoAlias, ownerUserID)
	}
	if roomMayExist(err) {
		if existing := c.recoverRoomByAlias(req.RoomAliasName, err); existing != nil {
			resp, err = existing, nil
		}
	}
	if err != nil {
		return nil, err
	}
	if ownerUserID != "" && !c.createdAsUser(ownerUserID) {
		logger.Info("CreateRegularRoom: room %s created as admin; inviting %s and setting power level 100", resp.RoomID, ownerUserID)
		if err := c.ensureRoomOwner(resp.RoomID, ownerUserID); err != nil {
			logger.Warn("CreateRegularRoom: could not set room owner %s: %v", ownerUserID, err)
		} else {
			logger.Info("CreateRegularRoom: owner %s invited and set to power level 100", ownerUserID)
		}
	}
	return resp, nil
}

// CreateDirectRoom creates a DM room between creatorUserID and otherUserID.
// Room is created as creatorUserID (using AS when available) so it shows under "People" for both.
// Sets m.direct account_data for both users so clients show the room as a DM.
// When force_join is true, the admin is temporarily invited (as creator), joins to force-join the other user, then leaves so only the two users remain.
func (c *Client) CreateDirectRoom(creatorUserID, otherUserID string) (*CreateRoomResponse, error) {
	req := &CreateRoomRequest{
		Preset:     string(PresetTrustedPrivateChat),
		Visibility: string(VisibilityPrivate),
		IsDirect:   true,
		Invite:     []string{otherUserID},
	}
	logger.Info("CreateDirectRoom: creator=%s other=%s (is_direct=true)", creatorUserID, otherUserID)
	resp, err := c.CreateRoomAsUser(req, creatorUserID)
	if errors.Is(err, ErrCreateRoomTimeout) {
		// A DM has no alias to resolve, so look for the room by its membership before trying
		// again — creating a second one would leave a duplicate DM with no way to tell which
		// is which.
		if roomID := c.findDirectRoomByMembers(creatorUserID, otherUserID); roomID != "" {
			logger.Warn("CreateDirectRoom: create timed out but DM %s already exists; reusing it", roomID)
			resp, err = &CreateRoomResponse{RoomID: roomID}, nil
		} else {
			logger.Warn("CreateDirectRoom: create timed out and no DM found for %s/%s; creating again", creatorUserID, otherUserID)
			resp, err = c.CreateRoomAsUser(req, creatorUserID)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("create DM room: %w", err)
	}
	// Ensure other user is in the room. When force_join: room is invite-only so admin cannot join directly.
	// Have creator invite admin, admin joins, force-join other, then admin leaves.
	if c.forceJoin {
		me, whoErr := c.WhoAmI()
		if whoErr != nil || me == nil {
			logger.Warn("CreateDirectRoom: cannot force-join without knowing admin user: %v", whoErr)
		} else if err := c.InviteUserAsUser(resp.RoomID, me.UserID, creatorUserID); err != nil {
			logger.Warn("CreateDirectRoom: creator invite admin failed: %v", err)
		} else if err := c.JoinRoom(resp.RoomID); err != nil {
			logger.Warn("CreateDirectRoom: admin join failed: %v", err)
		} else {
			c.joinedRoomsMu.Lock()
			if c.joinedRooms == nil {
				c.joinedRooms = make(map[string]struct{})
			}
			c.joinedRooms[resp.RoomID] = struct{}{}
			c.joinedRoomsMu.Unlock()
			if err := c.ForceJoinUser(resp.RoomID, otherUserID); err != nil {
				logger.Warn("CreateDirectRoom: force-join %s failed: %v", otherUserID, err)
			} else {
				logger.Info("CreateDirectRoom: force-joined %s to DM %s", otherUserID, resp.RoomID)
			}
			if leaveErr := c.LeaveRoom(resp.RoomID); leaveErr != nil {
				logger.Warn("CreateDirectRoom: admin leave after force-join failed: %v", leaveErr)
			} else {
				logger.Info("CreateDirectRoom: admin left DM %s", resp.RoomID)
			}
		}
	} else {
		// otherUserID was already invited in the create request; no need to invite again as admin
		logger.Info("CreateDirectRoom: %s was invited in create request", otherUserID)
	}
	// Set m.direct for both users so the room appears under "People" for both (requires AS)
	if err := c.setDirectRoomForUser(creatorUserID, otherUserID, resp.RoomID); err != nil {
		logger.Warn("CreateDirectRoom: failed to set m.direct for %s: %v", creatorUserID, err)
	}
	if err := c.setDirectRoomForUser(otherUserID, creatorUserID, resp.RoomID); err != nil {
		logger.Warn("CreateDirectRoom: failed to set m.direct for %s: %v", otherUserID, err)
	}
	return resp, nil
}

// setDirectRoomForUser updates userID's m.direct account_data to include roomID under otherUserID.
// Requires Application Service token (sets account_data on behalf of userID).
func (c *Client) setDirectRoomForUser(userID, otherUserID, roomID string) error {
	if c.asToken == "" {
		return fmt.Errorf("setting m.direct requires Application Service token")
	}
	existing, _ := c.GetAccountData(userID, "m.direct")
	direct, _ := existing.(map[string]interface{})
	if direct == nil {
		direct = make(map[string]interface{})
	}
	var list []interface{}
	if l, ok := direct[otherUserID].([]interface{}); ok {
		list = l
	}
	for _, r := range list {
		if s, _ := r.(string); s == roomID {
			logger.Info("setDirectRoomForUser: %s already has room %s for %s", userID, roomID, otherUserID)
			return nil
		}
	}
	list = append(list, roomID)
	direct[otherUserID] = list
	return c.SetAccountData(userID, "m.direct", direct)
}

// GetAccountData retrieves account_data for a user. Requires AS token and user_id query param.
func (c *Client) GetAccountData(userID, key string) (interface{}, error) {
	if c.asToken == "" {
		return nil, fmt.Errorf("getting account_data for user requires Application Service token")
	}
	endpoint := fmt.Sprintf("/_matrix/client/v3/user/%s/account_data/%s", url.PathEscape(userID), url.PathEscape(key))
	params := url.Values{}
	params.Set("user_id", userID)
	endpoint += "?" + params.Encode()
	body, statusCode, err := c.doRequestWithToken("GET", endpoint, nil, c.asToken)
	if err != nil {
		return nil, err
	}
	if statusCode == http.StatusNotFound {
		return nil, nil
	}
	if statusCode != http.StatusOK {
		var resp GenericResponse
		json.Unmarshal(body, &resp)
		return nil, fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}
	var content interface{}
	if err := json.Unmarshal(body, &content); err != nil {
		return nil, err
	}
	return content, nil
}

// SetAccountData sets account_data for a user. Requires AS token and user_id query param.
func (c *Client) SetAccountData(userID, key string, content interface{}) error {
	if c.asToken == "" {
		return fmt.Errorf("setting account_data for user requires Application Service token")
	}
	endpoint := fmt.Sprintf("/_matrix/client/v3/user/%s/account_data/%s", url.PathEscape(userID), url.PathEscape(key))
	params := url.Values{}
	params.Set("user_id", userID)
	endpoint += "?" + params.Encode()
	body, statusCode, err := c.doRequestWithToken("PUT", endpoint, content, c.asToken)
	if err != nil {
		return err
	}
	if statusCode != http.StatusOK {
		var resp GenericResponse
		json.Unmarshal(body, &resp)
		return fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}
	logger.Info("SetAccountData: set %s for user %s", key, userID)
	return nil
}

// createdAsUser returns true if we would have created the room as the given user (AS token + user_id).
func (c *Client) createdAsUser(ownerUserID string) bool {
	return ownerUserID != "" && c.asToken != ""
}

// ensureRoomOwner invites ownerUserID to the room (if not already the sender) and sets their power level to 100.
func (c *Client) ensureRoomOwner(roomID, ownerUserID string) error {
	me, err := c.WhoAmI()
	if err != nil || me == nil {
		return fmt.Errorf("whoami: %w", err)
	}
	if ownerUserID == me.UserID {
		logger.Info("ensureRoomOwner: owner %s is current user, no invite needed", ownerUserID)
		return nil
	}
	if c.forceJoin {
		logger.Info("ensureRoomOwner: force-joining %s to room %s", ownerUserID, roomID)
	} else {
		logger.Info("ensureRoomOwner: inviting %s to room %s", ownerUserID, roomID)
	}
	if err := c.AddUserToRoom(roomID, ownerUserID); err != nil {
		return fmt.Errorf("add owner to room: %w", err)
	}
	logger.Info("ensureRoomOwner: setting power level 100 for %s in room %s", ownerUserID, roomID)
	return c.SetPowerLevels(roomID, ownerUserID, 100)
}

// SetPowerLevels sets a user's power level in a room (e.g. 100 for owner).
// Updates are merged with existing m.room.power_levels content to avoid dropping
// other operators/users from the state event.
func (c *Client) SetPowerLevels(roomID, userID string, level int) error {
	me, err := c.WhoAmI()
	if err != nil || me == nil {
		return fmt.Errorf("whoami: %w", err)
	}

	content, err := c.getPowerLevels(roomID)
	if err != nil {
		return fmt.Errorf("get power levels: %w", err)
	}
	if content.Users == nil {
		content.Users = make(map[string]int)
	}
	// Keep admin at PL 100 for state updates where possible, while preserving all existing entries.
	if current, ok := content.Users[me.UserID]; !ok || current < 100 {
		content.Users[me.UserID] = 100
	}
	content.Users[userID] = level

	if err := c.setPowerLevelsWithToken(roomID, content, c.adminToken); err != nil {
		return err
	}
	logger.Info("SetPowerLevels: set %s to level %d in room %s", userID, level, roomID)
	return nil
}

// SetPowerLevelsBulk sets power levels for multiple users in a room (e.g. all to the same level for equal membership).
// Updates are merged with existing m.room.power_levels content so existing operators are preserved.
func (c *Client) SetPowerLevelsBulk(roomID string, userLevels map[string]int) error {
	me, err := c.WhoAmI()
	if err != nil || me == nil {
		return fmt.Errorf("whoami: %w", err)
	}

	content, err := c.getPowerLevels(roomID)
	if err != nil {
		return fmt.Errorf("get power levels: %w", err)
	}
	if content.Users == nil {
		content.Users = make(map[string]int)
	}

	if current, ok := content.Users[me.UserID]; !ok || current < 100 {
		content.Users[me.UserID] = 100
	}
	for u, level := range userLevels {
		content.Users[u] = level
	}

	if err := c.setPowerLevelsWithToken(roomID, content, c.adminToken); err != nil {
		return err
	}
	logger.Info("SetPowerLevelsBulk: set %d users in room %s", len(userLevels), roomID)
	return nil
}

// InviteUser invites a user to a room (as the current user, typically admin)
func (c *Client) InviteUser(roomID, userID string) error {
	endpoint := fmt.Sprintf("/_matrix/client/v3/rooms/%s/invite", url.PathEscape(roomID))
	req := &InviteRequest{UserID: userID}
	body, statusCode, err := c.doRequest("POST", endpoint, req)
	if err != nil {
		return err
	}
	if statusCode == http.StatusForbidden {
		var resp GenericResponse
		json.Unmarshal(body, &resp)
		if resp.Errcode == "M_FORBIDDEN" {
			return nil // Already a member, not an error
		}
	}
	if statusCode != http.StatusOK {
		var resp GenericResponse
		json.Unmarshal(body, &resp)
		return fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}
	return nil
}

// InviteUserAsUser invites inviteeUserID to a room as senderUserID. Requires AS token.
// Used so the room creator (senderUserID) can invite the admin, allowing admin to join and then force-join others.
func (c *Client) InviteUserAsUser(roomID, inviteeUserID, senderUserID string) error {
	logger.Debug("InviteUserAsUser: room=%s invitee=%s sender=%s", roomID, inviteeUserID, senderUserID)
	if c.asToken == "" || senderUserID == "" {
		return fmt.Errorf("InviteUserAsUser requires AS token and senderUserID")
	}
	endpoint := fmt.Sprintf("/_matrix/client/v3/rooms/%s/invite", url.PathEscape(roomID))
	params := url.Values{}
	params.Set("user_id", senderUserID)
	endpoint += "?" + params.Encode()
	req := &InviteRequest{UserID: inviteeUserID}
	body, statusCode, err := c.doRequestWithToken("POST", endpoint, req, c.asToken)
	if err != nil {
		return err
	}
	if statusCode == http.StatusOK {
		logger.Debug("InviteUserAsUser: success room=%s", roomID)
		return nil
	}
	var resp GenericResponse
	json.Unmarshal(body, &resp)
	if statusCode == http.StatusForbidden && strings.Contains(resp.Error, "already in the room") {
		logger.Debug("InviteUserAsUser: invitee already in room=%s, treating as success", roomID)
		return nil // Invitee is already in the room; no need to invite again
	}
	logger.Debug("InviteUserAsUser: failed room=%s status=%d err=%s", roomID, statusCode, resp.Error)
	return fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
}

// ForceJoinUser adds a user to a room or space via Synapse admin API (user is joined directly, no invite to accept).
// Only works for local users. Requires admin token. The server admin must be in the room (call ensureAdminInRoom first).
// Treats "already in the room" (403) as success so idempotent retries don't fail.
func (c *Client) ForceJoinUser(roomID, userID string) error {
	logger.Debug("ForceJoinUser: room=%s user=%s", roomID, userID)
	endpoint := fmt.Sprintf("/_synapse/admin/v1/join/%s", url.PathEscape(roomID))
	req := &InviteRequest{UserID: userID}
	body, statusCode, err := c.doRequest("POST", endpoint, req)
	if err != nil {
		return err
	}
	if statusCode != http.StatusOK {
		var resp GenericResponse
		json.Unmarshal(body, &resp)
		if statusCode == http.StatusForbidden && strings.Contains(resp.Error, "already in the room") {
			logger.Debug("ForceJoinUser: user already in room=%s, treating as success", roomID)
			return nil // Idempotent: user is already a member
		}
		logger.Debug("ForceJoinUser: failed room=%s user=%s status=%d err=%s", roomID, userID, statusCode, resp.Error)
		return fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}
	logger.Debug("ForceJoinUser: success room=%s user=%s", roomID, userID)
	return nil
}

// ensureAdminInRoom ensures the admin user has joined the room. Required before ForceJoinUser (Synapse requires admin in room).
// Joins are cached per room so each room is only joined once.
func (c *Client) ensureAdminInRoom(roomID string) error {
	logger.Debug("ensureAdminInRoom: room=%s", roomID)
	c.joinedRoomsMu.Lock()
	if c.joinedRooms == nil {
		c.joinedRooms = make(map[string]struct{})
	}
	_, alreadyJoined := c.joinedRooms[roomID]
	c.joinedRoomsMu.Unlock()

	if alreadyJoined {
		return nil
	}
	err := c.JoinRoom(roomID)
	if err == nil {
		c.joinedRoomsMu.Lock()
		c.joinedRooms[roomID] = struct{}{}
		c.joinedRoomsMu.Unlock()
		return nil
	}
	// Invite-only rooms: client join fails with "not invited". Try Synapse admin API to add ourselves (may work on some setups).
	me, whoErr := c.WhoAmI()
	if whoErr == nil && me != nil {
		if joinErr := c.ForceJoinUser(roomID, me.UserID); joinErr == nil {
			c.joinedRoomsMu.Lock()
			c.joinedRooms[roomID] = struct{}{}
			c.joinedRoomsMu.Unlock()
			return nil
		}
	}
	if strings.Contains(err.Error(), "not invited") || strings.Contains(err.Error(), "M_FORBIDDEN") {
		return fmt.Errorf("room is invite-only or restricted; admin cannot join to link it: %w", err)
	}
	return fmt.Errorf("admin join room: %w", err)
}

// AddUserToRoom adds a user to a room: force-join via Synapse admin API if ForceJoin is enabled, otherwise invite.
// When force-join is used, the admin is joined to the room first (Synapse requirement).
func (c *Client) AddUserToRoom(roomID, userID string) error {
	logger.Debug("AddUserToRoom: room=%s user=%s forceJoin=%v", roomID, userID, c.forceJoin)
	if c.forceJoin {
		if err := c.ensureAdminInRoom(roomID); err != nil {
			return err
		}
		return c.ForceJoinUser(roomID, userID)
	}
	return c.InviteUser(roomID, userID)
}

// JoinRoom makes the admin user join a room (needed before inviting others in some cases)
func (c *Client) JoinRoom(roomID string) error {
	logger.Debug("JoinRoom: room=%s", roomID)
	endpoint := fmt.Sprintf("/_matrix/client/v3/rooms/%s/join", url.PathEscape(roomID))

	body, statusCode, err := c.doRequest("POST", endpoint, &JoinRequest{})
	if err != nil {
		return err
	}

	if statusCode != http.StatusOK {
		var resp GenericResponse
		json.Unmarshal(body, &resp)
		logger.Debug("JoinRoom: failed room=%s status=%d err=%s", roomID, statusCode, resp.Error)
		return fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}

	logger.Debug("JoinRoom: success room=%s", roomID)
	return nil
}

// LeaveRoom makes the current user (admin) leave a room. Used after force-joining others in a DM so the admin is not left in the room.
func (c *Client) LeaveRoom(roomID string) error {
	logger.Debug("LeaveRoom: room=%s", roomID)
	endpoint := fmt.Sprintf("/_matrix/client/v3/rooms/%s/leave", url.PathEscape(roomID))
	body, statusCode, err := c.doRequest("POST", endpoint, &JoinRequest{})
	if err != nil {
		return err
	}
	if statusCode != http.StatusOK {
		var resp GenericResponse
		json.Unmarshal(body, &resp)
		return fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}
	c.joinedRoomsMu.Lock()
	delete(c.joinedRooms, roomID)
	c.joinedRoomsMu.Unlock()
	return nil
}

// ensureAdminInSpaceWithPower adds the admin to a space (created as owner via AS) with the given power level
// so that the admin can later link rooms to the space. ownerUserID is the space creator; they invite the admin (via AS).
func (c *Client) ensureAdminInSpaceWithPower(spaceID, ownerUserID string, powerLevel int) error {
	logger.Debug("ensureAdminInSpaceWithPower: space=%s owner=%s powerLevel=%d", spaceID, ownerUserID, powerLevel)
	me, err := c.WhoAmI()
	if err != nil || me == nil {
		return fmt.Errorf("whoami: %w", err)
	}
	if c.asToken == "" || ownerUserID == "" {
		return fmt.Errorf("AS token and ownerUserID required to add admin to space created as owner")
	}
	if ownerUserID != me.UserID {
		// Owner invites admin (so admin can join invite-only space)
		if err := c.InviteUserAsUser(spaceID, me.UserID, ownerUserID); err != nil {
			return fmt.Errorf("invite admin to space: %w", err)
		}
	} else {
		logger.Debug("ensureAdminInSpaceWithPower: owner is admin, skipping invite space=%s", spaceID)
	}
	if err := c.JoinRoom(spaceID); err != nil {
		return fmt.Errorf("admin join space: %w", err)
	}
	c.joinedRoomsMu.Lock()
	if c.joinedRooms == nil {
		c.joinedRooms = make(map[string]struct{})
	}
	c.joinedRooms[spaceID] = struct{}{}
	c.joinedRoomsMu.Unlock()
	pl, err := c.getPowerLevels(spaceID)
	if err != nil {
		return fmt.Errorf("get power levels: %w", err)
	}
	currentLevel := powerLevelForUser(pl, me.UserID)
	if currentLevel >= powerLevel {
		logger.Debug("ensureAdminInSpaceWithPower: admin already has level %d in space=%s (needed=%d)", currentLevel, spaceID, powerLevel)
		return nil
	}
	if pl.Users == nil {
		pl.Users = make(map[string]int)
	}
	pl.Users[me.UserID] = powerLevel
	if err := c.setPowerLevelsAsUser(spaceID, pl, ownerUserID); err != nil {
		return fmt.Errorf("set admin power level in space: %w", err)
	}
	logger.Debug("ensureAdminInSpaceWithPower: success space=%s", spaceID)
	return nil
}

// ensureAdminInRoomWithPower adds the admin to a room (created as owner via AS) with the given power level
// so that the admin can link the room to a space. ownerUserID is the primary room owner used for PL updates.
// inviteSenderCandidates are optional fallback inviters to try when ownerUserID cannot invite admin.
func (c *Client) ensureAdminInRoomWithPower(roomID, ownerUserID string, powerLevel int, inviteSenderCandidates ...string) error {
	logger.Debug("ensureAdminInRoomWithPower: room=%s owner=%s powerLevel=%d", roomID, ownerUserID, powerLevel)
	me, err := c.WhoAmI()
	if err != nil || me == nil {
		return fmt.Errorf("whoami: %w", err)
	}
	if c.asToken == "" || ownerUserID == "" {
		return fmt.Errorf("AS token and ownerUserID required to add admin to room created as owner")
	}
	inviteSenders := make([]string, 0, 1+len(inviteSenderCandidates))
	seenSenders := make(map[string]struct{}, 1+len(inviteSenderCandidates))
	addSender := func(sender string) {
		if sender == "" || sender == me.UserID {
			return
		}
		if _, exists := seenSenders[sender]; exists {
			return
		}
		seenSenders[sender] = struct{}{}
		inviteSenders = append(inviteSenders, sender)
	}
	addSender(ownerUserID)
	for _, candidate := range inviteSenderCandidates {
		addSender(candidate)
	}
	inviteAttempted := false
	lastInviteSender := ""
	var lastInviteErr error
	for _, sender := range inviteSenders {
		inviteAttempted = true
		lastInviteSender = sender
		logger.Debug("ensureAdminInRoomWithPower: invite admin room=%s inviter=%s", roomID, sender)
		if err := c.InviteUserAsUser(roomID, me.UserID, sender); err == nil {
			lastInviteErr = nil
			break
		} else {
			lastInviteErr = err
			logger.Debug("ensureAdminInRoomWithPower: invite failed room=%s inviter=%s err=%v", roomID, sender, err)
		}
	}
	if !inviteAttempted {
		logger.Debug("ensureAdminInRoomWithPower: no inviter candidates for room=%s owner=%s", roomID, ownerUserID)
	}
	if err := c.JoinRoom(roomID); err != nil {
		// Private/invite-only room: direct join can fail if admin was previously cleaned up.
		// Fall back to Synapse admin join so force-join membership import can proceed.
		if joinErr := c.ForceJoinUser(roomID, me.UserID); joinErr != nil {
			inviterForLog := lastInviteSender
			if inviterForLog == "" {
				inviterForLog = "<none>"
			}
			inviteErrForLog := "<nil>"
			if !inviteAttempted {
				inviteErrForLog = "<not attempted>"
			} else if lastInviteErr != nil {
				inviteErrForLog = lastInviteErr.Error()
			}
			return fmt.Errorf(
				"admin join room failed room=%s owner=%s invite_attempted=%t inviter=%s invite_err=%s join_err=%w force_join_err=%v",
				roomID, ownerUserID, inviteAttempted, inviterForLog, inviteErrForLog, err, joinErr,
			)
		}
	}
	c.joinedRoomsMu.Lock()
	if c.joinedRooms == nil {
		c.joinedRooms = make(map[string]struct{})
	}
	c.joinedRooms[roomID] = struct{}{}
	c.joinedRoomsMu.Unlock()
	pl, err := c.getPowerLevels(roomID)
	if err != nil {
		return fmt.Errorf("get power levels: %w", err)
	}
	currentLevel := powerLevelForUser(pl, me.UserID)
	if currentLevel >= powerLevel {
		logger.Debug("ensureAdminInRoomWithPower: admin already has level %d in room=%s (needed=%d)", currentLevel, roomID, powerLevel)
		return nil
	}
	if pl.Users == nil {
		pl.Users = make(map[string]int)
	}
	pl.Users[me.UserID] = powerLevel
	attemptedSetters := make([]string, 0, len(inviteSenders))
	var lastSetErr error
	for _, setter := range inviteSenders {
		attemptedSetters = append(attemptedSetters, setter)
		if err := c.setPowerLevelsAsUser(roomID, pl, setter); err != nil {
			lastSetErr = err
			logger.Debug("ensureAdminInRoomWithPower: set PL failed room=%s asUser=%s err=%v", roomID, setter, err)
			continue
		}
		logger.Debug("ensureAdminInRoomWithPower: set PL success room=%s asUser=%s", roomID, setter)
		logger.Debug("ensureAdminInRoomWithPower: success room=%s", roomID)
		return nil
	}
	if lastSetErr == nil {
		lastSetErr = fmt.Errorf("no eligible sender available to set power levels")
	}
	return &adminPowerEscalationError{
		roomID:           roomID,
		requiredLevel:    powerLevel,
		attemptedSetters: attemptedSetters,
		cause:            lastSetErr,
	}
}

type adminPowerEscalationError struct {
	roomID           string
	requiredLevel    int
	attemptedSetters []string
	cause            error
}

func (e *adminPowerEscalationError) Error() string {
	return fmt.Sprintf(
		"set admin power level in room: room=%s required_level=%d attempted_setters=%v err=%v",
		e.roomID, e.requiredLevel, e.attemptedSetters, e.cause,
	)
}

func (e *adminPowerEscalationError) Unwrap() error {
	return e.cause
}

func isAdminPowerEscalationError(err error) bool {
	var target *adminPowerEscalationError
	return errors.As(err, &target)
}

// setPowerLevelsAsUser sets power levels in a room as ownerUserID (requires AS token).
func (c *Client) setPowerLevelsAsUser(roomID string, content *PowerLevelsContent, ownerUserID string) error {
	logger.Debug("setPowerLevelsAsUser: room=%s asUser=%s", roomID, ownerUserID)
	if c.asToken == "" || ownerUserID == "" {
		return fmt.Errorf("AS token and ownerUserID required")
	}
	endpoint := fmt.Sprintf("/_matrix/client/v3/rooms/%s/state/m.room.power_levels/", url.PathEscape(roomID))
	params := url.Values{}
	params.Set("user_id", ownerUserID)
	endpoint += "?" + params.Encode()
	body, statusCode, err := c.doRequestWithToken("PUT", endpoint, content, c.asToken)
	if err != nil {
		return err
	}
	if statusCode != http.StatusOK {
		var resp GenericResponse
		json.Unmarshal(body, &resp)
		return fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}
	return nil
}

func (c *Client) getPowerLevels(roomID string) (*PowerLevelsContent, error) {
	logger.Debug("getPowerLevels: room=%s", roomID)
	endpoint := fmt.Sprintf("/_matrix/client/v3/rooms/%s/state/m.room.power_levels/", url.PathEscape(roomID))
	body, statusCode, err := c.doRequest("GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("GET power_levels: %d", statusCode)
	}
	var content PowerLevelsContent
	if err := json.Unmarshal(body, &content); err != nil {
		return nil, err
	}
	return &content, nil
}

func powerLevelForUser(content *PowerLevelsContent, userID string) int {
	if content == nil {
		return 0
	}
	if content.Users != nil {
		if level, ok := content.Users[userID]; ok {
			return level
		}
	}
	return content.UsersDefault
}

func (c *Client) setPowerLevelsWithToken(roomID string, content *PowerLevelsContent, token string) error {
	// From room version 12 the creator holds implicit infinite power and the server rejects the
	// whole event if the creator is listed in content.users ("Creator user ... must not appear in
	// content.users"). Rooms here are created as their owner, so the owner is usually the creator
	// and every power-level update would fail. Dropping the entry is safe: it cannot lower or
	// raise a creator's power either way.
	if creator := c.roomCreator(roomID); creator != "" && content != nil && content.Users != nil {
		if _, listed := content.Users[creator]; listed {
			stripped := make(map[string]int, len(content.Users))
			for u, l := range content.Users {
				if u != creator {
					stripped[u] = l
				}
			}
			trimmed := *content
			trimmed.Users = stripped
			content = &trimmed
			logger.Debug("setPowerLevelsWithToken: dropped creator %s from content.users for room %s", creator, roomID)
		}
	}

	endpoint := fmt.Sprintf("/_matrix/client/v3/rooms/%s/state/m.room.power_levels/", url.PathEscape(roomID))
	body, statusCode, err := c.doRequestWithToken("PUT", endpoint, content, token)
	if err != nil {
		return err
	}
	if statusCode != http.StatusOK {
		var resp GenericResponse
		json.Unmarshal(body, &resp)
		return fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}
	return nil
}

// AddRoomToSpace adds a room as a child of a space.
// The sender must be in both the space and the room (Synapse returns M_FORBIDDEN otherwise).
func (c *Client) AddRoomToSpace(spaceID, roomID string, suggested bool) error {
	logger.Debug("AddRoomToSpace: space=%s room=%s suggested=%v", spaceID, roomID, suggested)
	if err := c.ensureAdminInRoom(spaceID); err != nil {
		return fmt.Errorf("admin join space: %w", err)
	}
	if err := c.ensureAdminInRoom(roomID); err != nil {
		return fmt.Errorf("admin join room: %w", err)
	}
	endpoint := fmt.Sprintf("/_matrix/client/v3/rooms/%s/state/%s/%s",
		url.PathEscape(spaceID),
		EventTypeSpaceChild,
		url.PathEscape(roomID))

	content := &SpaceChildContent{
		Via:       []string{c.homeserver},
		Suggested: suggested,
	}

	body, statusCode, err := c.doRequest("PUT", endpoint, content)
	if err != nil {
		return err
	}

	if statusCode != http.StatusOK {
		var resp GenericResponse
		json.Unmarshal(body, &resp)
		logger.Debug("AddRoomToSpace: failed space=%s room=%s status=%d err=%s", spaceID, roomID, statusCode, resp.Error)
		return fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}

	logger.Debug("AddRoomToSpace: success space=%s room=%s", spaceID, roomID)
	return nil
}

// SetRoomParent sets the parent space for a room
func (c *Client) SetRoomParent(roomID, spaceID string, canonical bool) error {
	logger.Debug("SetRoomParent: room=%s space=%s canonical=%v", roomID, spaceID, canonical)
	endpoint := fmt.Sprintf("/_matrix/client/v3/rooms/%s/state/%s/%s",
		url.PathEscape(roomID),
		EventTypeSpaceParent,
		url.PathEscape(spaceID))

	content := &SpaceParentContent{
		Via:       []string{c.homeserver},
		Canonical: canonical,
	}

	body, statusCode, err := c.doRequest("PUT", endpoint, content)
	if err != nil {
		return err
	}

	if statusCode != http.StatusOK {
		var resp GenericResponse
		json.Unmarshal(body, &resp)
		logger.Debug("SetRoomParent: failed room=%s space=%s status=%d err=%s", roomID, spaceID, statusCode, resp.Error)
		return fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}

	logger.Debug("SetRoomParent: success room=%s space=%s", roomID, spaceID)
	return nil
}

// SetJoinRulesRestricted sets the room's join_rules to "restricted" so only members of the given space can join.
// Requires room version 8+. Used for public rooms so access is "Space members" of the parent team/space.
func (c *Client) SetJoinRulesRestricted(roomID, spaceID string) error {
	logger.Debug("SetJoinRulesRestricted: room=%s space=%s", roomID, spaceID)
	// Empty state_key: PUT /rooms/{roomId}/state/{eventType} (no stateKey segment)
	endpoint := fmt.Sprintf("/_matrix/client/v3/rooms/%s/state/%s",
		url.PathEscape(roomID),
		EventTypeJoinRules)

	content := &JoinRulesContent{
		JoinRule: "restricted",
		Allow:    []JoinRulesAllowEntry{{Type: "m.space", RoomID: spaceID}},
	}

	body, statusCode, err := c.doRequest("PUT", endpoint, content)
	if err != nil {
		return err
	}

	if statusCode != http.StatusOK {
		var resp GenericResponse
		json.Unmarshal(body, &resp)
		return fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}

	logger.Info("SetJoinRulesRestricted: room %s now restricted to space %s", roomID, spaceID)
	return nil
}

// SetRoomName sets m.room.name for a room as the current authenticated user (admin token).
func (c *Client) SetRoomName(roomID, name string) error {
	endpoint := fmt.Sprintf("/_matrix/client/v3/rooms/%s/state/m.room.name", url.PathEscape(roomID))
	content := map[string]string{"name": name}
	body, statusCode, err := c.doRequest("PUT", endpoint, content)
	if err != nil {
		return err
	}
	if statusCode != http.StatusOK {
		var resp GenericResponse
		json.Unmarshal(body, &resp)
		return fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}
	return nil
}

// TryRecoverUnjoinableRoom performs best-effort recovery for rooms where admin cannot join for force-join.
// Strategy:
//  1) Have fallback owner invite admin and give admin power (requires fallback owner to be in room).
//  2) Promote fallback owner to PL 100 (owner-equivalent).
//  3) Rename room to "[Unjoinable] <oldName>" for visibility.
func (c *Client) TryRecoverUnjoinableRoom(roomID, oldName, fallbackOwnerID string) error {
	if fallbackOwnerID == "" {
		return fmt.Errorf("fallback owner is empty")
	}
	if err := c.ensureAdminInRoomWithPower(roomID, fallbackOwnerID, 100); err != nil {
		return err
	}
	if err := c.SetPowerLevels(roomID, fallbackOwnerID, 100); err != nil {
		return fmt.Errorf("set fallback owner power level: %w", err)
	}
	newName := strings.TrimSpace(oldName)
	if newName == "" {
		newName = roomID
	}
	if !strings.HasPrefix(newName, "[Unjoinable] ") {
		newName = "[Unjoinable] " + newName
	}
	if err := c.SetRoomName(roomID, newName); err != nil {
		return fmt.Errorf("rename unjoinable room: %w", err)
	}
	return nil
}

// FormatUserID formats a username as a full Matrix user ID
func (c *Client) FormatUserID(username string) string {
	if c.masClient != nil {
		return c.masClient.FormatUserID(username)
	}
	return fmt.Sprintf("@%s:%s", username, c.homeserver)
}

// SetASToken sets the Application Service token for message import
func (c *Client) SetASToken(token string) {
	c.asToken = token
}

// HasASToken returns true if an AS token is configured
func (c *Client) HasASToken() bool {
	return c.asToken != ""
}

// getNextTxnID generates a unique transaction ID for messages
func (c *Client) getNextTxnID() string {
	c.mu.Lock()
	c.txnCounter++
	txn := c.txnCounter
	c.mu.Unlock()
	return fmt.Sprintf("mmx_%d_%d", time.Now().UnixMilli(), txn)
}

// SendMessageRequest represents a message to send
type SendMessageRequest struct {
	MsgType       string    `json:"msgtype"`
	Body          string    `json:"body"`
	FormattedBody string    `json:"formatted_body,omitempty"`
	Format        string    `json:"format,omitempty"`
	Mentions      *Mentions `json:"m.mentions,omitempty"`
}

// Mentions carries the intentional-mentions metadata (MSC3952) so mentioned users are
// notified and clients render the mention as a pill.
type Mentions struct {
	UserIDs []string `json:"user_ids,omitempty"`
}

// mentionsOrNil returns a *Mentions for the given user IDs, or nil when there are none
// (so the field is omitted from the event content).
func mentionsOrNil(userIDs []string) *Mentions {
	if len(userIDs) == 0 {
		return nil
	}
	return &Mentions{UserIDs: userIDs}
}

// SendMessageResponse represents the response from sending a message
type SendMessageResponse struct {
	EventID string `json:"event_id"`
	Errcode string `json:"errcode,omitempty"`
	Error   string `json:"error,omitempty"`
}

// SendMessage sends a message to a room (without timestamp - uses current time)
func (c *Client) SendMessage(roomID, message string) (*SendMessageResponse, error) {
	return c.SendMessageWithTimestamp(roomID, message, 0, "")
}

// SendMessageWithTimestamp sends a message to a room with a specific timestamp
// This requires an Application Service token to be set
// If timestamp is 0, uses current time
// If senderUserID is provided, the message will appear as sent by that user (requires AS)
func (c *Client) SendMessageWithTimestamp(roomID, message string, timestamp int64, senderUserID string) (*SendMessageResponse, error) {
	txnID := c.getNextTxnID()

	// Build endpoint
	endpoint := fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/m.room.message/%s",
		url.PathEscape(roomID), url.PathEscape(txnID))

	// Add query parameters
	params := url.Values{}

	// Add timestamp if AS token is available and timestamp is provided
	if timestamp > 0 && c.asToken != "" {
		params.Set("ts", strconv.FormatInt(timestamp, 10))
	}

	// Add user_id parameter for AS to send on behalf of user
	if senderUserID != "" && c.asToken != "" {
		params.Set("user_id", senderUserID)
	}

	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}

	// Create message content. Render mentions as pills (protecting their underscores from
	// markdown emphasis) and advertise them via m.mentions.
	formattedBody, mentionIDs := renderMatrixMessageHTML(message)
	req := &SendMessageRequest{
		MsgType:       "m.text",
		Body:          message,
		Format:        "org.matrix.custom.html",
		FormattedBody: formattedBody,
		Mentions:      mentionsOrNil(mentionIDs),
	}

	// Use AS token if available, otherwise use admin token
	token := c.adminToken
	if c.asToken != "" {
		token = c.asToken
	}

	// Make request
	body, statusCode, err := c.doRequestWithToken("PUT", endpoint, req, token)
	if err != nil {
		return nil, err
	}

	var resp SendMessageResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}

	return &resp, nil
}

// SendReplyWithTimestamp sends a reply to a message with a specific timestamp
func (c *Client) SendReplyWithTimestamp(roomID, message string, threadRootEventID, threadLatestEventID string, timestamp int64, senderUserID string) (*SendMessageResponse, error) {
	txnID := c.getNextTxnID()

	// Build endpoint
	endpoint := fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/m.room.message/%s",
		url.PathEscape(roomID), url.PathEscape(txnID))

	// Add query parameters
	params := url.Values{}

	if timestamp > 0 && c.asToken != "" {
		params.Set("ts", strconv.FormatInt(timestamp, 10))
	}

	if senderUserID != "" && c.asToken != "" {
		params.Set("user_id", senderUserID)
	}

	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}

	// Create reply content with thread relation
	formattedBody, mentionIDs := renderMatrixMessageHTML(message)
	content := map[string]interface{}{
		"msgtype":        "m.text",
		"body":           message,
		"format":         "org.matrix.custom.html",
		"formatted_body": formattedBody,
		"m.relates_to":   threadRelation(threadRootEventID, threadLatestEventID),
	}
	if len(mentionIDs) > 0 {
		content["m.mentions"] = map[string]interface{}{"user_ids": mentionIDs}
	}

	// Use AS token if available
	token := c.adminToken
	if c.asToken != "" {
		token = c.asToken
	}

	body, statusCode, err := c.doRequestWithToken("PUT", endpoint, content, token)
	if err != nil {
		return nil, err
	}

	var resp SendMessageResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}

	return &resp, nil
}

// doRequestWithToken performs an HTTP request with a specific token
func (c *Client) doRequestWithToken(method, endpoint string, body interface{}, token string) ([]byte, int, error) {
	return c.doRequestWithTokenAndRetry(method, endpoint, body, token, 0)
}

// doRequestWithTokenAndRetry performs an HTTP request with retry logic
func (c *Client) doRequestWithTokenAndRetry(method, endpoint string, body interface{}, token string, retryCount int) ([]byte, int, error) {
	// Rate limiting
	c.mu.Lock()
	if c.rateLimit > 0 {
		elapsed := time.Since(c.lastRequest)
		if elapsed < c.rateLimit {
			sleepTime := c.rateLimit - elapsed
			time.Sleep(sleepTime)
		}
	}
	c.lastRequest = time.Now()
	c.mu.Unlock()

	var reqBody io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(jsonBody)
	}

	reqURL := c.baseURL + endpoint
	req, err := http.NewRequest(method, reqURL, reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if shouldRetryRequestErr(err) && isNonIdempotent(method, endpoint) {
			logger.Warn("Request transport error on %s; not retrying (room may have been created): %v", endpoint, err)
			return nil, 0, fmt.Errorf("%w: %v", ErrCreateRoomTimeout, err)
		}
		if shouldRetryRequestErr(err) && retryCount < c.maxRetries {
			retryAfter := retryDelay(c.retryBaseDelay, retryCount)
			logger.Warn("Request transport error, retrying in %v (%d/%d): %v", retryAfter, retryCount+1, c.maxRetries, err)
			time.Sleep(retryAfter)
			return c.doRequestWithTokenAndRetry(method, endpoint, body, token, retryCount+1)
		}
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("failed to read response body: %w", err)
	}

	// Handle rate limiting (429) with exponential backoff
	if resp.StatusCode == http.StatusTooManyRequests {
		if retryCount >= c.maxRetries {
			return nil, resp.StatusCode, fmt.Errorf("rate limit exceeded after %d retries", c.maxRetries)
		}

		var retryAfter time.Duration
		if retryAfterStr := resp.Header.Get("Retry-After"); retryAfterStr != "" {
			if seconds, err := strconv.Atoi(retryAfterStr); err == nil {
				retryAfter = time.Duration(seconds) * time.Second
			}
		}

		if retryAfter == 0 {
			retryAfter = c.retryBaseDelay * time.Duration(1<<uint(retryCount))
		}

		if retryAfter > 60*time.Second {
			retryAfter = 60 * time.Second
		}

		logger.Warn("Rate limit hit (429), waiting %v before retry %d/%d", retryAfter, retryCount+1, c.maxRetries)
		time.Sleep(retryAfter)

		return c.doRequestWithTokenAndRetry(method, endpoint, body, token, retryCount+1)
	}

	return respBody, resp.StatusCode, nil
}

func retryDelay(base time.Duration, retryCount int) time.Duration {
	retryAfter := base * time.Duration(1<<uint(retryCount))
	if retryAfter > 60*time.Second {
		return 60 * time.Second
	}
	if retryAfter <= 0 {
		return 1 * time.Second
	}
	return retryAfter
}

// threadRelation builds the m.relates_to content for a message inside a thread.
// rootEventID is the thread root. latestEventID is the newest event already in that thread;
// the nested m.in_reply_to points at it so clients without thread support still render a
// sensible chain. is_falling_back is always true because Mattermost replies are flat against
// the root: the nested reply exists for compatibility, never because a user picked that
// specific message to reply to.
func threadRelation(rootEventID, latestEventID string) map[string]interface{} {
	fallback := latestEventID
	if fallback == "" {
		fallback = rootEventID
	}
	return map[string]interface{}{
		"rel_type":        "m.thread",
		"event_id":        rootEventID,
		"is_falling_back": true,
		"m.in_reply_to": map[string]interface{}{
			"event_id": fallback,
		},
	}
}

// ErrCreateRoomTimeout marks a createRoom request that failed at the transport layer.
// The server may still have created the room, so callers must look the room up rather than
// blindly issue the request again.
var ErrCreateRoomTimeout = errors.New("createRoom request did not complete")

// isNonIdempotent reports whether replaying the request could create a second copy of
// something. createRoom is the case that matters here: every call makes a new room, so an
// automatic retry after a transport timeout orphans the room the first attempt created.
func isNonIdempotent(method, endpoint string) bool {
	return method == "POST" && strings.HasPrefix(endpoint, "/_matrix/client/v3/createRoom")
}

// roomMayExist reports whether err leaves open the possibility that the room was created:
// either the server rejected the alias as taken, or the request never came back to us.
func roomMayExist(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrCreateRoomTimeout) || strings.Contains(err.Error(), "M_ROOM_IN_USE")
}

func shouldRetryRequestErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "eof")
}

// UploadMediaResponse represents the response from media upload
type UploadMediaResponse struct {
	ContentURI string `json:"content_uri"` // mxc://server/media_id
	Errcode    string `json:"errcode,omitempty"`
	Error      string `json:"error,omitempty"`
}

// UploadMedia uploads a file to Matrix media repository
// Returns the mxc:// URI for the uploaded file
func (c *Client) UploadMedia(data []byte, filename, contentType string) (*UploadMediaResponse, error) {
	endpoint := fmt.Sprintf("/_matrix/media/v3/upload?filename=%s", url.QueryEscape(filename))

	// Rate limiting
	c.mu.Lock()
	if c.rateLimit > 0 {
		elapsed := time.Since(c.lastRequest)
		if elapsed < c.rateLimit {
			time.Sleep(c.rateLimit - elapsed)
		}
	}
	c.lastRequest = time.Now()
	c.mu.Unlock()

	reqURL := c.baseURL + endpoint
	req, err := http.NewRequest("POST", reqURL, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to create upload request: %w", err)
	}

	token := c.adminToken
	if c.asToken != "" {
		token = c.asToken
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read upload response: %w", err)
	}

	var result UploadMediaResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse upload response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upload failed (%d): %s - %s", resp.StatusCode, result.Errcode, result.Error)
	}

	return &result, nil
}

// FileMessageContent represents a file message content
type FileMessageContent struct {
	MsgType  string    `json:"msgtype"`       // m.file, m.image, m.video, m.audio
	Body     string    `json:"body"`          // Filename as fallback
	URL      string    `json:"url,omitempty"` // mxc:// URI (for uploaded files)
	Filename string    `json:"filename,omitempty"`
	Info     *FileInfo `json:"info,omitempty"`
}

// FileInfo contains metadata about the file
type FileInfo struct {
	MimeType      string         `json:"mimetype,omitempty"`
	Size          int64          `json:"size,omitempty"`
	Width         int            `json:"w,omitempty"`        // For images/videos
	Height        int            `json:"h,omitempty"`        // For images/videos
	Duration      int            `json:"duration,omitempty"` // For audio/video in ms
	ThumbnailURL  string         `json:"thumbnail_url,omitempty"`
	ThumbnailInfo *ThumbnailInfo `json:"thumbnail_info,omitempty"`
}

// ThumbnailInfo contains metadata about thumbnail
type ThumbnailInfo struct {
	MimeType string `json:"mimetype,omitempty"`
	Size     int64  `json:"size,omitempty"`
	Width    int    `json:"w,omitempty"`
	Height   int    `json:"h,omitempty"`
}

// SendFileMessage sends a file message to a room
func (c *Client) SendFileMessage(roomID string, content *FileMessageContent, timestamp int64, senderUserID string) (*SendMessageResponse, error) {
	txnID := c.getNextTxnID()

	endpoint := fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/m.room.message/%s",
		url.PathEscape(roomID), url.PathEscape(txnID))

	params := url.Values{}
	if timestamp > 0 && c.asToken != "" {
		params.Set("ts", strconv.FormatInt(timestamp, 10))
	}
	if senderUserID != "" && c.asToken != "" {
		params.Set("user_id", senderUserID)
	}
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}

	token := c.adminToken
	if c.asToken != "" {
		token = c.asToken
	}

	body, statusCode, err := c.doRequestWithToken("PUT", endpoint, content, token)
	if err != nil {
		return nil, err
	}

	var resp SendMessageResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}

	return &resp, nil
}

// SendFileLink sends a message with a file link (external URL)
// Note: Matrix doesn't support external URLs directly in file messages,
// so we send as a text message with a markdown link
func (c *Client) SendFileLink(roomID, filename, fileURL, mimeType string, fileSize int64, timestamp int64, senderUserID string) (*SendMessageResponse, error) {
	// Determine emoji based on file type
	emoji := "📎"
	if strings.HasPrefix(mimeType, "image/") {
		emoji = "🖼️"
	} else if strings.HasPrefix(mimeType, "video/") {
		emoji = "🎬"
	} else if strings.HasPrefix(mimeType, "audio/") {
		emoji = "🎵"
	}

	message := fmt.Sprintf("%s [%s](%s)", emoji, filename, fileURL)

	return c.SendMessageWithTimestamp(roomID, message, timestamp, senderUserID)
}

// SendFileLinkAsReply sends a file link as a reply message (external URL).
func (c *Client) SendFileLinkAsReply(roomID, filename, fileURL, mimeType string, fileSize int64, threadRootEventID, threadLatestEventID string, timestamp int64, senderUserID string) (*SendMessageResponse, error) {
	emoji := "📎"
	if strings.HasPrefix(mimeType, "image/") {
		emoji = "🖼️"
	} else if strings.HasPrefix(mimeType, "video/") {
		emoji = "🎬"
	} else if strings.HasPrefix(mimeType, "audio/") {
		emoji = "🎵"
	}

	message := fmt.Sprintf("%s [%s](%s)", emoji, filename, fileURL)
	return c.SendReplyWithTimestamp(roomID, message, threadRootEventID, threadLatestEventID, timestamp, senderUserID)
}

// SendUploadedFile sends a file that was already uploaded to Matrix
func (c *Client) SendUploadedFile(roomID, mxcURI, filename, mimeType string, fileSize int64, width, height int, timestamp int64, senderUserID string) (*SendMessageResponse, error) {
	msgType := "m.file"
	if strings.HasPrefix(mimeType, "image/") {
		msgType = "m.image"
	} else if strings.HasPrefix(mimeType, "video/") {
		msgType = "m.video"
	} else if strings.HasPrefix(mimeType, "audio/") {
		msgType = "m.audio"
	}

	content := &FileMessageContent{
		MsgType:  msgType,
		Body:     filename,
		URL:      mxcURI,
		Filename: filename,
		Info: &FileInfo{
			MimeType: mimeType,
			Size:     fileSize,
		},
	}

	if width > 0 && height > 0 {
		content.Info.Width = width
		content.Info.Height = height
	}

	return c.SendFileMessage(roomID, content, timestamp, senderUserID)
}

// SendUploadedFileAsReply sends an already-uploaded file event as a reply.
func (c *Client) SendUploadedFileAsReply(roomID, mxcURI, filename, mimeType string, fileSize int64, width, height int, threadRootEventID, threadLatestEventID string, timestamp int64, senderUserID string) (*SendMessageResponse, error) {
	msgType := "m.file"
	if strings.HasPrefix(mimeType, "image/") {
		msgType = "m.image"
	} else if strings.HasPrefix(mimeType, "video/") {
		msgType = "m.video"
	} else if strings.HasPrefix(mimeType, "audio/") {
		msgType = "m.audio"
	}

	content := map[string]interface{}{
		"msgtype":  msgType,
		"body":     filename,
		"url":      mxcURI,
		"filename": filename,
		"info": map[string]interface{}{
			"mimetype": mimeType,
			"size":     fileSize,
		},
		"m.relates_to": threadRelation(threadRootEventID, threadLatestEventID),
	}
	if width > 0 && height > 0 {
		info := content["info"].(map[string]interface{})
		info["w"] = width
		info["h"] = height
	}

	txnID := c.getNextTxnID()
	endpoint := fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/m.room.message/%s",
		url.PathEscape(roomID), url.PathEscape(txnID))
	params := url.Values{}
	if timestamp > 0 && c.asToken != "" {
		params.Set("ts", strconv.FormatInt(timestamp, 10))
	}
	if senderUserID != "" && c.asToken != "" {
		params.Set("user_id", senderUserID)
	}
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}

	token := c.adminToken
	if c.asToken != "" {
		token = c.asToken
	}

	body, statusCode, err := c.doRequestWithToken("PUT", endpoint, content, token)
	if err != nil {
		return nil, err
	}

	var resp SendMessageResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("API error (%d): %s - %s", statusCode, resp.Errcode, resp.Error)
	}
	return &resp, nil
}
