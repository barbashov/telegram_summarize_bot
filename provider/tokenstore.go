package provider

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"telegram_summarize_bot/logger"
)

const (
	tokenFileName     = "openai_tokens.json" // #nosec G101 -- token storage filename, not a credential
	tokenLockFileName = "openai_tokens.lock" // #nosec G101 -- lock filename, not a credential
	refreshBuffer     = 5 * time.Minute
	// defaultExpiresIn is assumed when the token endpoint omits expires_in (or
	// returns something unusable); without it the token would be born expired
	// and every LLM call would trigger a refresh POST.
	defaultExpiresIn = 3600
)

// OpenAITokenURL is the OAuth token endpoint. It is a var (not a const) so tests
// can point it at an httptest server; production code never reassigns it.
var OpenAITokenURL = "https://auth.openai.com/oauth/token" // #nosec G101 -- OAuth endpoint URL, not a credential

// TokenResponse is the OAuth token endpoint response format.
type TokenResponse struct {
	AccessToken  string     `json:"access_token"`
	RefreshToken string     `json:"refresh_token"`
	IDToken      string     `json:"id_token"`
	ExpiresIn    FlexInt    `json:"expires_in"`
	TokenType    string     `json:"token_type"`
	Error        TokenError `json:"error"`
	ErrorDesc    string     `json:"error_description"`
}

// ExpiresInSeconds returns the token lifetime, substituting defaultExpiresIn
// when the endpoint returned nothing usable (missing, zero, or negative).
func (tr *TokenResponse) ExpiresInSeconds() int {
	if tr.ExpiresIn <= 0 {
		logger.Warn().Int("expires_in", int(tr.ExpiresIn)).Msg("token endpoint returned no usable expires_in; assuming 1 hour")
		return defaultExpiresIn
	}
	return int(tr.ExpiresIn)
}

// FlexInt is an int that unmarshals from either a JSON number or a
// string-typed number ("3600") — the token endpoint mixes shapes, same class
// as the object-form "error" handled by TokenError.
type FlexInt int

func (n *FlexInt) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("parse int %q: %w", s, err)
	}
	*n = FlexInt(v)
	return nil
}

// TokenError is the "error" field of a token endpoint response. The endpoint
// returns either the OAuth2 form (a bare code string, with the human-readable
// text in error_description) or the OpenAI API form (an object with
// code/message/type).
type TokenError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *TokenError) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		return json.Unmarshal(data, &e.Code)
	}
	type object TokenError // drop methods to avoid recursing into UnmarshalJSON
	var obj object
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	*e = TokenError(obj)
	return nil
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

	tokens, err := s.readTokenFile()
	if err != nil {
		return err
	}
	s.tokens = tokens
	return nil
}

// readTokenFile reads and parses the token file without touching s.tokens.
func (s *TokenStore) readTokenFile() (*OAuthTokens, error) {
	data, err := os.ReadFile(s.filePath())
	if err != nil {
		return nil, fmt.Errorf("read token file: %w", err)
	}

	var tokens OAuthTokens
	if err := json.Unmarshal(data, &tokens); err != nil {
		return nil, fmt.Errorf("parse token file: %w", err)
	}
	return &tokens, nil
}

// Save writes tokens to disk.
func (s *TokenStore) Save(tokens *OAuthTokens) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.persistLocked(tokens); err != nil {
		return err
	}
	s.tokens = tokens
	return nil
}

// persistLocked atomically writes tokens to the token file: marshal to a temp
// file in the same directory (0600), then rename over the target so a reader
// never observes a half-written file. Callers must hold s.mu.
func (s *TokenStore) persistLocked(tokens *OAuthTokens) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create token dir: %w", err)
	}

	data, err := json.MarshalIndent(tokens, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal tokens: %w", err)
	}

	tmp, err := os.CreateTemp(s.dir, tokenFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp token file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail out before the rename.
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp token file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp token file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp token file: %w", err)
	}
	if err := os.Rename(tmpName, s.filePath()); err != nil {
		return fmt.Errorf("rename token file: %w", err)
	}
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
	if err := s.refreshLocked(true); err != nil {
		// Inside the refresh buffer the current token may still be valid;
		// prefer serving it over failing the call outright.
		if time.Now().Before(s.tokens.ExpiresAt) {
			logger.Warn().Err(err).Time("expires_at", s.tokens.ExpiresAt).
				Msg("token refresh failed; using still-valid access token")
			return s.tokens.AccessToken, nil
		}
		return "", fmt.Errorf("token refresh: %w", err)
	}
	return s.tokens.AccessToken, nil
}

// ForceRefresh forces a token refresh regardless of expiration.
func (s *TokenStore) ForceRefresh() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshLocked(false)
}

// lockTokenFile takes an exclusive advisory lock serializing token refreshes
// across processes (bot vs CLI). Returns an unlock func, or an error if the
// lock file can't be created/locked.
func (s *TokenStore) lockTokenFile() (func(), error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, fmt.Errorf("create token dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(s.dir, tokenLockFileName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open token lock file: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock token file: %w", err)
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}

// refreshLocked refreshes the access token. Callers must hold s.mu.
//
// The bot and the CLIs (usage, openai token-refresh) each hold an independent
// TokenStore over the same file, and the endpoint rotates refresh tokens:
// refreshing from a stale in-memory copy yields invalid_grant, and two
// concurrent refreshes can trip reuse detection and kill the token family.
// So: take a cross-process file lock, re-read the file to adopt tokens a
// sibling process may have rotated, and only then refresh. With skipIfFresh,
// an adopted still-valid token short-circuits the network refresh entirely.
func (s *TokenStore) refreshLocked(skipIfFresh bool) error {
	unlock, err := s.lockTokenFile()
	if err != nil {
		logger.Warn().Err(err).Msg("could not acquire token file lock; refreshing without it")
	} else {
		defer unlock()
	}

	if disk, err := s.readTokenFile(); err == nil {
		if s.tokens == nil || disk.ExpiresAt.After(s.tokens.ExpiresAt) || disk.RefreshToken != s.tokens.RefreshToken {
			s.tokens = disk
			if skipIfFresh && time.Now().Add(refreshBuffer).Before(disk.ExpiresAt) {
				logger.Info().Time("expires_at", disk.ExpiresAt).Msg("adopted newer OAuth tokens from disk; skipping refresh")
				return nil
			}
		}
	}
	if s.tokens == nil {
		return fmt.Errorf("no OAuth tokens loaded (run '%s openai auth' first)", os.Args[0])
	}

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
		IDToken:      s.tokens.IDToken,
		AccountID:    s.tokens.AccountID,
		ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresInSeconds()) * time.Second),
	}
	// Only adopt a new id_token (and re-derive the account ID) when one is
	// returned; refresh responses often omit it, and overwriting with empty
	// values would lose the account ID needed for the ChatGPT-Account-ID header.
	if tr.IDToken != "" {
		newTokens.IDToken = tr.IDToken
		newTokens.AccountID = ExtractAccountID(tr.IDToken)
	}
	// Store rotated refresh token if provided
	if tr.RefreshToken != "" {
		newTokens.RefreshToken = tr.RefreshToken
	}

	// Update in-memory tokens first so a rotated refresh token survives even if
	// persistence fails — the old refresh token may already be invalidated
	// server-side, so we must not roll back on a write error.
	s.tokens = newTokens
	if err := s.persistLocked(newTokens); err != nil {
		logger.Warn().Err(err).Msg("OAuth token refreshed but persisting to disk failed; keeping refreshed tokens in memory")
	}

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
	if tr.Error.Code != "" || tr.Error.Message != "" {
		code := firstNonEmpty(tr.Error.Code, "token endpoint error")
		desc := firstNonEmpty(tr.Error.Message, tr.ErrorDesc)
		return nil, fmt.Errorf("%s: %s", code, desc)
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

// GetPlanType returns the ChatGPT subscription plan (e.g. "plus") parsed from
// the stored id_token, or "" if unknown.
func (s *TokenStore) GetPlanType() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens == nil {
		return ""
	}
	return ExtractPlanType(s.tokens.IDToken)
}

// ExtractPlanType extracts the chatgpt_plan_type claim from a JWT id_token.
// No signature verification is performed — we trust the token endpoint.
func ExtractPlanType(idToken string) string {
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
			PlanType string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Auth.PlanType
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
