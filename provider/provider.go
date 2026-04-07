package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"telegram_summarize_bot/config"
	"telegram_summarize_bot/logger"
)

// debugTransport logs the outgoing request body and response status for debugging.
type debugTransport struct{ inner http.RoundTripper }

func (d *debugTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		body, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(strings.NewReader(string(body)))
		logger.Debug().Str("url", req.URL.String()).Str("body", string(body)).Msg("LLM request")
	}
	resp, err := d.inner.RoundTrip(req)
	if resp != nil && resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body = io.NopCloser(strings.NewReader(string(respBody)))
		logger.Debug().Int("status", resp.StatusCode).Str("body", string(respBody)).Str("url", req.URL.String()).Msg("LLM error response")
	}
	return resp, err
}

// HTTPClient creates an HTTP client with proxy support and the given timeout.
func HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
		},
	}
}

// DebugHTTPClient creates an HTTP client that logs all requests and responses.
func DebugHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &debugTransport{inner: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
		}},
	}
}

// Message represents a chat message for the LLM.
type Message struct {
	Role    string // "system", "user", "assistant"
	Content string
}

// CompletionRequest is an API-agnostic request to the LLM.
type CompletionRequest struct {
	Model       string
	Messages    []Message
	MaxTokens   int
	Temperature float32
}

// CompletionResponse is an API-agnostic response from the LLM.
type CompletionResponse struct {
	Content        string
	FinishReason   string // "stop", "length", etc.
	HTTPStatusCode int
}

// APIError represents an LLM API error with HTTP status code.
type APIError struct {
	HTTPStatusCode int
	Message        string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("LLM API error (HTTP %d): %s", e.HTTPStatusCode, e.Message)
}

// LLMClient is the interface that all LLM providers implement.
type LLMClient interface {
	Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error)
}

// New creates the appropriate LLM client based on config.
func New(cfg *config.Config) (LLMClient, error) {
	switch cfg.LLMMode {
	case config.LLMModeCompletions, "":
		return NewCompletionsClient(cfg.LLMToken, cfg.LLMEndpoint)
	case config.LLMModeResponses:
		return NewResponsesClient(cfg.LLMToken, cfg.LLMEndpoint)
	case config.LLMModeOAuth:
		return NewOAuthClient(cfg.OAuthTokenDir, cfg.OAuthClientID)
	default:
		return nil, fmt.Errorf("unknown LLM mode: %q", cfg.LLMMode)
	}
}
