package provider

import (
	"testing"

	"telegram_summarize_bot/config"
)

func TestAPIError(t *testing.T) {
	err := &APIError{HTTPStatusCode: 429, Message: "rate limit exceeded"}
	want := "LLM API error (HTTP 429): rate limit exceeded"
	if got := err.Error(); got != want {
		t.Errorf("APIError.Error() = %q, want %q", got, want)
	}
}

func TestNewCompletionsMode(t *testing.T) {
	cfg := &config.Config{
		LLMMode:     config.LLMModeCompletions,
		LLMToken:    "test-key",
		LLMEndpoint: "https://example.com/v1",
	}
	client, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := client.(*completionsClient); !ok {
		t.Errorf("expected *completionsClient, got %T", client)
	}
}

func TestNewResponsesMode(t *testing.T) {
	cfg := &config.Config{
		LLMMode:     config.LLMModeResponses,
		LLMToken:    "test-key",
		LLMEndpoint: "https://api.openai.com/v1",
	}
	client, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := client.(*responsesClient); !ok {
		t.Errorf("expected *responsesClient, got %T", client)
	}
}

func TestNewOAuthModeNoTokens(t *testing.T) {
	cfg := &config.Config{
		LLMMode:       config.LLMModeOAuth,
		OAuthTokenDir: t.TempDir(),
		OAuthClientID: "test-client",
	}
	_, err := New(cfg)
	if err == nil {
		t.Fatal("expected error when no tokens exist")
	}
}

func TestNewInvalidMode(t *testing.T) {
	cfg := &config.Config{
		LLMMode: "invalid",
	}
	_, err := New(cfg)
	if err == nil {
		t.Fatal("expected error for invalid mode")
	}
}
