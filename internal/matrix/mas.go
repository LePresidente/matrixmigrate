package matrix

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aligundogdu/matrixmigrate/internal/logger"
)

// MASClient is a client for the Matrix Authentication Service Admin API.
// It is used to create users in MAS so they can log in via SSO/OAuth without
// "Localpart not available" errors when Synapse is configured to use MAS.
type MASClient struct {
	baseURL       string
	clientID      string
	clientSecret  string
	homeserver    string
	httpClient    *http.Client
	accessToken   string
	tokenExpiry   time.Time
	mu            sync.Mutex
}

// NewMASClient creates a new MAS Admin API client.
// baseURL is the MAS API base URL (e.g. http://mas.example.com:8080).
func NewMASClient(baseURL, clientID, clientSecret, homeserver string) *MASClient {
	baseURL = strings.TrimSuffix(baseURL, "/")
	return &MASClient{
		baseURL:      baseURL,
		clientID:     clientID,
		clientSecret: clientSecret,
		homeserver:   homeserver,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// masTokenResponse is the OAuth2 token response from MAS
type masTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"` // seconds
	TokenType   string `json:"token_type"`
}

// getToken obtains an OAuth2 access token using client_credentials grant.
// Tokens are cached and refreshed when expired.
func (m *MASClient) getToken() (string, error) {
	m.mu.Lock()
	if m.accessToken != "" && time.Now().Before(m.tokenExpiry) {
		tok := m.accessToken
		m.mu.Unlock()
		return tok, nil
	}
	m.mu.Unlock()

	data := url.Values{}
	data.Set("grant_type", "client_credentials")
	data.Set("scope", "urn:mas:admin")

	req, err := http.NewRequest("POST", m.baseURL+"/oauth2/token", strings.NewReader(data.Encode()))
	if err != nil {
		return "", fmt.Errorf("create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(m.clientID, m.clientSecret)

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token request failed (status %d): %s", resp.StatusCode, string(body))
	}

	var tok masTokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("no access_token in response")
	}

	m.mu.Lock()
	m.accessToken = tok.AccessToken
	// Refresh 60s before expiry
	if tok.ExpiresIn > 60 {
		m.tokenExpiry = time.Now().Add(time.Duration(tok.ExpiresIn-60) * time.Second)
	} else {
		m.tokenExpiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	m.mu.Unlock()

	return tok.AccessToken, nil
}

// masAddUserRequest is the request body for POST /api/admin/v1/users
type masAddUserRequest struct {
	Username           string `json:"username"`
	SkipHomeserverCheck bool   `json:"skip_homeserver_check,omitempty"`
}

// masSingleUserResponse is the JSON:API-style response for a single user
type masSingleUserResponse struct {
	Data struct {
		Type       string `json:"type"`
		ID         string `json:"id"` // MAS internal ULID
		Attributes struct {
			Username string `json:"username"`
		} `json:"attributes"`
	} `json:"data"`
	Links struct {
		Self string `json:"self"`
	} `json:"links"`
}

// masErrorResponse is the error response shape from MAS
type masErrorResponse struct {
	Errors []struct {
		Title string `json:"title"`
	} `json:"errors"`
}

// doRequest performs an authenticated request to the MAS Admin API
func (m *MASClient) doRequest(method, path string, body interface{}) ([]byte, int, error) {
	token, err := m.getToken()
	if err != nil {
		return nil, 0, err
	}

	var reqBody io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(jsonBody)
	}

	req, err := http.NewRequest(method, m.baseURL+path, reqBody)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/vnd.api+json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return respBody, resp.StatusCode, nil
}

// CreateUser creates a user in MAS and sets their password.
// Returns a UserResponse with UserID set to @username:homeserver.
func (m *MASClient) CreateUser(username string, req *CreateUserRequest) (*UserResponse, error) {
	logger.Info("Creating user in MAS: %s", username)

	body, statusCode, err := m.doRequest("POST", "/api/admin/v1/users", &masAddUserRequest{
		Username: username,
	})
	if err != nil {
		logger.Error("MAS create user request failed for '%s': %v", username, err)
		return nil, err
	}

	if statusCode == http.StatusConflict || statusCode == http.StatusUnprocessableEntity {
		var errResp masErrorResponse
		_ = json.Unmarshal(body, &errResp)
		errMsg := string(body)
		if len(errResp.Errors) > 0 {
			errMsg = errResp.Errors[0].Title
		}
		if strings.Contains(strings.ToLower(errMsg), "already exists") ||
			strings.Contains(strings.ToLower(errMsg), "reserved") {
			logger.Info("User '%s' already exists in MAS, treating as success", username)
			return &UserResponse{UserID: m.FormatUserID(username)}, nil
		}
		return nil, fmt.Errorf("MAS API error (%d): %s", statusCode, errMsg)
	}

	if statusCode != http.StatusOK && statusCode != http.StatusCreated {
		var errResp masErrorResponse
		_ = json.Unmarshal(body, &errResp)
		errMsg := string(body)
		if len(errResp.Errors) > 0 {
			errMsg = errResp.Errors[0].Title
		}
		logger.Error("MAS create user failed for '%s': %d %s", username, statusCode, errMsg)
		return nil, fmt.Errorf("MAS API error (%d): %s", statusCode, errMsg)
	}

	var createResp masSingleUserResponse
	if err := json.Unmarshal(body, &createResp); err != nil {
		return nil, fmt.Errorf("parse MAS create response: %w", err)
	}
	masUserID := createResp.Data.ID

	// Set password if provided (skipped when MAS is OIDC-only and password auth is disabled)
	if req != nil && req.Password != "" {
		setPassBody, passCode, passErr := m.doRequest("POST",
			fmt.Sprintf("/api/admin/v1/users/%s/set-password", url.PathEscape(masUserID)),
			map[string]string{"password": req.Password})
		if passErr != nil {
			logger.Warn("MAS set-password failed for '%s': %v (user was created)", username, passErr)
		} else if passCode != http.StatusOK {
			bodyStr := string(setPassBody)
			if passCode == http.StatusForbidden && strings.Contains(bodyStr, "Password auth is disabled") {
				logger.Info("MAS set-password skipped for '%s': OIDC-only MAS (users will sign in via SSO)", username)
			} else {
				logger.Warn("MAS set-password failed for '%s': status %d %s", username, passCode, bodyStr)
			}
		}
	}

	userID := m.FormatUserID(username)
	logger.Success("Created user in MAS: %s -> %s", username, userID)
	return &UserResponse{
		UserID:      userID,
		Name:        username,
		DisplayName: req.DisplayName,
		Admin:       req.Admin,
		Deactivated: req.Deactivated,
	}, nil
}

// UserExists checks if a user exists in MAS by username (localpart).
func (m *MASClient) UserExists(username string) (bool, error) {
	path := "/api/admin/v1/users/by-username/" + url.PathEscape(username)
	body, statusCode, err := m.doRequest("GET", path, nil)
	if err != nil {
		logger.Error("MAS UserExists check failed for '%s': %v", username, err)
		return false, err
	}
	if statusCode == http.StatusNotFound {
		return false, nil
	}
	if statusCode != http.StatusOK {
		return false, fmt.Errorf("MAS API error (%d): %s", statusCode, string(body))
	}
	var resp masSingleUserResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return false, fmt.Errorf("parse MAS user response: %w", err)
	}
	return resp.Data.Attributes.Username != "", nil
}

// VerifyCredentials checks that MAS is reachable and accepts the configured OAuth2
// client credentials. Without this, bad credentials only surface once user creation
// starts, by which point rooms have already been created with a fallback owner.
func (m *MASClient) VerifyCredentials() error {
	if _, err := m.getToken(); err != nil {
		return err
	}
	return nil
}

// FormatUserID returns the full Matrix user ID for the given localpart.
func (m *MASClient) FormatUserID(username string) string {
	return fmt.Sprintf("@%s:%s", username, m.homeserver)
}

// SetHomeserver sets the homeserver domain used for FormatUserID.
func (m *MASClient) SetHomeserver(homeserver string) {
	m.homeserver = homeserver
}
