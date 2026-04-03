package provider

import (
	"context"
	"testing"
	"time"
)

// fakeLLMClient records calls and returns canned responses.
type fakeLLMClient struct {
	lastReq  CompletionRequest
	response CompletionResponse
	err      error
}

func (f *fakeLLMClient) Complete(_ context.Context, req CompletionRequest) (CompletionResponse, error) {
	f.lastReq = req
	return f.response, f.err
}

func TestOAuthClientDelegatesToInner(t *testing.T) {
	dir := t.TempDir()
	store := NewTokenStore(dir, "client-id")
	tokens := &OAuthTokens{
		AccessToken:  "my-token",
		RefreshToken: "refresh",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	if err := store.Save(tokens); err != nil {
		t.Fatalf("Save: %v", err)
	}

	inner := &fakeLLMClient{
		response: CompletionResponse{Content: "delegated", FinishReason: "stop"},
	}

	client := &oauthClient{
		inner:      inner,
		tokenStore: store,
	}

	req := CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}
	resp, err := client.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "delegated" {
		t.Errorf("content = %q, want %q", resp.Content, "delegated")
	}
	if inner.lastReq.Model != "gpt-4o" {
		t.Errorf("inner model = %q, want %q", inner.lastReq.Model, "gpt-4o")
	}
}

func TestOAuthClientFailsWithNoTokens(t *testing.T) {
	dir := t.TempDir()
	store := NewTokenStore(dir, "client-id")
	// Don't save any tokens

	client := &oauthClient{
		inner:      &fakeLLMClient{},
		tokenStore: store,
	}

	_, err := client.Complete(context.Background(), CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error when no tokens loaded")
	}
}

func TestOAuthClientInjectsTokenToInner(t *testing.T) {
	dir := t.TempDir()
	store := NewTokenStore(dir, "client-id")
	tokens := &OAuthTokens{
		AccessToken:  "fresh-token",
		RefreshToken: "refresh",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	if err := store.Save(tokens); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Use a fake inner that captures the request
	inner := &fakeLLMClient{
		response: CompletionResponse{Content: "ok", FinishReason: "stop"},
	}

	client := &oauthClient{
		inner:      inner,
		tokenStore: store,
	}

	resp, err := client.Complete(context.Background(), CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q, want %q", resp.Content, "ok")
	}
	// Verify the request was forwarded
	if inner.lastReq.Model != "gpt-4o" {
		t.Errorf("inner model = %q, want %q", inner.lastReq.Model, "gpt-4o")
	}
}
