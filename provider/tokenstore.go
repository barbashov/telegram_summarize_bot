package provider

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"telegram_summarize_bot/logger"
)

const (
	tokenFileName  = "openai_tokens.json"
	refreshBuffer  = 5 * time.Minute
	OpenAITokenURL = "https://auth.openai.com/oauth/token"
)

// TokenResponse is the OAuth token endpoint response format.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// OAuthTokens holds OAuth access and refresh tokens.
type OAuthTokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	IDToken      string    `json:"id_token,omitempty"`
	AccountID    string    `json:"account_id,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// TokenStore manages OAuth token persistence and refresh.
type TokenStore struct {
	dir      string
	clientID string
	mu       sync.Mutex
	tokens   *OAuthTokens
}

// NewTokenStore creates a token store. Call Load() to read existing tokens.
func NewTokenStore(dir, clientID string) *TokenStore {
	return &TokenStore{
		dir:      dir,
		clientID: clientID,
	}
}

func (s *TokenStore) filePath() string {
	return filepath.Join(s.dir, tokenFileName)
}

// Load reads tokens from disk.
func (s *TokenStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.filePath())
	if err != nil {
		return fmt.Errorf("read token file: %w", err)
	}

	var tokens OAuthTokens
	if err := json.Unmarshal(data, &tokens); err != nil {
		return fmt.Errorf("parse token file: %w", err)
	}

	s.tokens = &tokens
	return nil
}

// Save writes tokens to disk.
func (s *TokenStore) Save(tokens *OAuthTokens) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create token dir: %w", err)
	}

	data, err := json.MarshalIndent(tokens, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal tokens: %w", err)
	}

	if err := os.WriteFile(s.filePath(), data, 0o600); err != nil {
		return fmt.Errorf("write token file: %w", err)
	}

	s.tokens = tokens
	return nil
}

// GetValidToken returns a valid access token, refreshing if needed.
func (s *TokenStore) GetValidToken() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.tokens == nil {
		return "", fmt.Errorf("no OAuth tokens loaded (run '%s openai auth' first)", os.Args[0])
	}

	if time.Now().Add(refreshBuffer).Before(s.tokens.ExpiresAt) {
		return s.tokens.AccessToken, nil
	}

	logger.Info().Msg("OAuth access token expiring soon, refreshing...")
	if err := s.refreshLocked(); err != nil {
		return "", fmt.Errorf("token refresh: %w", err)
	}
	return s.tokens.AccessToken, nil
}

// ForceRefresh forces a token refresh regardless of expiration.
func (s *TokenStore) ForceRefresh() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshLocked()
}

func (s *TokenStore) refreshLocked() error {
	if s.tokens.RefreshToken == "" {
		return fmt.Errorf("no refresh token available, re-authenticate with '%s openai auth'", os.Args[0])
	}

	tr, err := PostTokenRequest(url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {s.clientID},
		"refresh_token": {s.tokens.RefreshToken},
	})
	if err != nil {
		return err
	}

	newTokens := &OAuthTokens{
		AccessToken:  tr.AccessToken,
		RefreshToken: s.tokens.RefreshToken,
		IDToken:      tr.IDToken,
		AccountID:    ExtractAccountID(tr.IDToken),
		ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}
	// Store rotated refresh token if provided
	if tr.RefreshToken != "" {
		newTokens.RefreshToken = tr.RefreshToken
	}

	// Persist to disk
	persistData, err := json.MarshalIndent(newTokens, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal refreshed tokens: %w", err)
	}
	if err := os.WriteFile(s.filePath(), persistData, 0o600); err != nil {
		return fmt.Errorf("write refreshed tokens: %w", err)
	}

	s.tokens = newTokens
	logger.Info().Time("expires_at", newTokens.ExpiresAt).Msg("OAuth token refreshed successfully")
	return nil
}

// PostTokenRequest posts form data to OpenAITokenURL and returns the parsed response.
func PostTokenRequest(data url.Values) (*TokenResponse, error) {
	client := HTTPClient(30 * time.Second)
	resp, err := client.Post(
		OpenAITokenURL, "application/x-www-form-urlencoded",
		strings.NewReader(data.Encode()),
	)
	if err != nil {
		return nil, fmt.Errorf("HTTP request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var tr TokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("parse response: %w (body: %s)", err, string(body))
	}
	if tr.Error != "" {
		return nil, fmt.Errorf("%s: %s", tr.Error, tr.ErrorDesc)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("no access_token in response")
	}
	return &tr, nil
}

// GetAccountID returns the ChatGPT account ID extracted from the stored id_token.
func (s *TokenStore) GetAccountID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens == nil {
		return ""
	}
	return s.tokens.AccountID
}

// ExtractAccountID extracts the chatgpt_account_id claim from a JWT id_token.
// No signature verification is performed — we trust the token endpoint.
func ExtractAccountID(idToken string) string {
	if idToken == "" {
		return ""
	}
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Auth.AccountID
}
