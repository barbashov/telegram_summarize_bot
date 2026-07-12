package provider

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTokenStoreSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	store := NewTokenStore(dir, "test-client-id")

	tokens := &OAuthTokens{
		AccessToken:  "access-123",
		RefreshToken: "refresh-456",
		IDToken:      "id-token-789",
		AccountID:    "account-abc",
		ExpiresAt:    time.Now().Add(time.Hour),
	}

	if err := store.Save(tokens); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify file exists
	fpath := filepath.Join(dir, tokenFileName)
	if _, err := os.Stat(fpath); err != nil {
		t.Fatalf("token file not found: %v", err)
	}

	// Load into a new store
	store2 := NewTokenStore(dir, "test-client-id")
	if err := store2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	token, err := store2.GetValidToken()
	if err != nil {
		t.Fatalf("GetValidToken: %v", err)
	}
	if token != "access-123" {
		t.Errorf("token = %q, want %q", token, "access-123")
	}
	if store2.tokens.IDToken != "id-token-789" {
		t.Errorf("IDToken = %q, want %q", store2.tokens.IDToken, "id-token-789")
	}
	if store2.tokens.AccountID != "account-abc" {
		t.Errorf("AccountID = %q, want %q", store2.tokens.AccountID, "account-abc")
	}
}

func TestTokenStoreLoadMissing(t *testing.T) {
	dir := t.TempDir()
	store := NewTokenStore(dir, "test-client-id")
	if err := store.Load(); err == nil {
		t.Fatal("expected error loading non-existent tokens")
	}
}

func TestTokenStoreGetValidTokenNoTokens(t *testing.T) {
	dir := t.TempDir()
	store := NewTokenStore(dir, "test-client-id")
	_, err := store.GetValidToken()
	if err == nil {
		t.Fatal("expected error when no tokens loaded")
	}
}

func TestTokenStoreFilePermissions(t *testing.T) {
	dir := t.TempDir()
	store := NewTokenStore(dir, "test-client-id")

	tokens := &OAuthTokens{
		AccessToken:  "access",
		RefreshToken: "refresh",
		ExpiresAt:    time.Now().Add(time.Hour),
	}

	if err := store.Save(tokens); err != nil {
		t.Fatalf("Save: %v", err)
	}

	fpath := filepath.Join(dir, tokenFileName)
	info, err := os.Stat(fpath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	perm := info.Mode().Perm()
	if perm != 0o600 {
		t.Errorf("file permissions = %o, want 600", perm)
	}
}

// makeIDToken builds a minimal JWT (header.payload.sig) whose payload carries
// the chatgpt_account_id claim under the OpenAI auth namespace.
func makeIDToken(t *testing.T, accountID string) string {
	t.Helper()
	payload := fmt.Sprintf(`{"https://api.openai.com/auth":{"chatgpt_account_id":%q}}`, accountID)
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(payload)) + ".sig"
}

// tokenServer returns an httptest server that responds to the refresh request
// with the given token response JSON, and rewires OpenAITokenURL to it for the
// duration of the test.
func tokenServer(t *testing.T, responseJSON string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseJSON))
	}))
	t.Cleanup(srv.Close)
	orig := OpenAITokenURL
	OpenAITokenURL = srv.URL
	t.Cleanup(func() { OpenAITokenURL = orig })
}

func TestTokenStoreRefreshPreservesAccountIDWhenNoIDToken(t *testing.T) {
	dir := t.TempDir()
	store := NewTokenStore(dir, "client-id")
	store.tokens = &OAuthTokens{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		IDToken:      makeIDToken(t, "account-keep"),
		AccountID:    "account-keep",
		ExpiresAt:    time.Now().Add(-time.Hour),
	}

	// Refresh response omits id_token and refresh_token.
	tokenServer(t, `{"access_token":"new-access","expires_in":3600,"token_type":"Bearer"}`)

	if err := store.ForceRefresh(); err != nil {
		t.Fatalf("ForceRefresh: %v", err)
	}
	if store.tokens.AccessToken != "new-access" {
		t.Errorf("access token = %q, want new-access", store.tokens.AccessToken)
	}
	if store.tokens.RefreshToken != "old-refresh" {
		t.Errorf("refresh token = %q, want old-refresh preserved", store.tokens.RefreshToken)
	}
	if store.tokens.AccountID != "account-keep" {
		t.Errorf("account ID = %q, want account-keep preserved", store.tokens.AccountID)
	}
	if store.GetAccountID() != "account-keep" {
		t.Errorf("GetAccountID = %q, want account-keep", store.GetAccountID())
	}
}

func TestTokenStoreRefreshAdoptsNewIDToken(t *testing.T) {
	dir := t.TempDir()
	store := NewTokenStore(dir, "client-id")
	store.tokens = &OAuthTokens{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		IDToken:      makeIDToken(t, "old-account"),
		AccountID:    "old-account",
		ExpiresAt:    time.Now().Add(-time.Hour),
	}

	newID := makeIDToken(t, "new-account")
	tokenServer(t, fmt.Sprintf(`{"access_token":"new-access","refresh_token":"rotated","id_token":%q,"expires_in":3600}`, newID))

	if err := store.ForceRefresh(); err != nil {
		t.Fatalf("ForceRefresh: %v", err)
	}
	if store.tokens.RefreshToken != "rotated" {
		t.Errorf("refresh token = %q, want rotated", store.tokens.RefreshToken)
	}
	if store.tokens.AccountID != "new-account" {
		t.Errorf("account ID = %q, want new-account", store.tokens.AccountID)
	}
}

func TestTokenStoreRefreshKeepsTokensWhenPersistFails(t *testing.T) {
	dir := t.TempDir()
	store := NewTokenStore(dir, "client-id")
	store.tokens = &OAuthTokens{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour),
	}
	// Make the store dir unwritable by pointing it at a path whose parent is a
	// file, so MkdirAll/temp-file creation fails.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	store.dir = filepath.Join(blocker, "sub")

	tokenServer(t, `{"access_token":"new-access","refresh_token":"rotated","expires_in":3600}`)

	// Refresh must not fail even though persistence cannot succeed.
	if err := store.ForceRefresh(); err != nil {
		t.Fatalf("ForceRefresh should not fail on persist error: %v", err)
	}
	if store.tokens.AccessToken != "new-access" {
		t.Errorf("access token = %q, want new-access kept in memory", store.tokens.AccessToken)
	}
	if store.tokens.RefreshToken != "rotated" {
		t.Errorf("refresh token = %q, want rotated kept in memory", store.tokens.RefreshToken)
	}
}

func TestTokenStoreAtomicWriteContentAndPerms(t *testing.T) {
	dir := t.TempDir()
	store := NewTokenStore(dir, "client-id")
	tokens := &OAuthTokens{
		AccessToken:  "access-xyz",
		RefreshToken: "refresh-xyz",
		IDToken:      "id-xyz",
		AccountID:    "acct-xyz",
		ExpiresAt:    time.Now().Add(time.Hour).Round(time.Second),
	}
	if err := store.Save(tokens); err != nil {
		t.Fatalf("Save: %v", err)
	}

	fpath := filepath.Join(dir, tokenFileName)
	info, err := os.Stat(fpath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 600", perm)
	}

	// No leftover temp files in the dir.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tokenFileName+".tmp-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}

	// Content round-trips.
	data, err := os.ReadFile(fpath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var got OAuthTokens
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.AccessToken != "access-xyz" || got.AccountID != "acct-xyz" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestPostTokenRequestStringError(t *testing.T) {
	tokenServer(t, `{"error":"invalid_grant","error_description":"refresh token expired"}`)

	_, err := PostTokenRequest(url.Values{"grant_type": {"refresh_token"}})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	want := "invalid_grant: refresh token expired"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestPostTokenRequestObjectError(t *testing.T) {
	// The OpenAI API form: "error" is an object, not a string.
	tokenServer(t, `{"error":{"message":"Your refresh token has already been used to generate a new access token. Please try signing in again.","type":"invalid_request_error","param":null,"code":"refresh_token_reused"}}`)

	_, err := PostTokenRequest(url.Values{"grant_type": {"refresh_token"}})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	got := err.Error()
	if !strings.HasPrefix(got, "refresh_token_reused: ") {
		t.Errorf("error = %q, want prefix %q", got, "refresh_token_reused: ")
	}
	if !strings.Contains(got, "already been used") {
		t.Errorf("error = %q, want it to carry the server message", got)
	}
}

func TestPostTokenRequestObjectErrorWithoutCode(t *testing.T) {
	tokenServer(t, `{"error":{"message":"something went wrong"}}`)

	_, err := PostTokenRequest(url.Values{"grant_type": {"refresh_token"}})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	want := "token endpoint error: something went wrong"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}
