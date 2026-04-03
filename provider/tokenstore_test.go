package provider

import (
	"os"
	"path/filepath"
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
