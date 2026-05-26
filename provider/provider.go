package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"telegram_summarize_bot/config"
	"telegram_summarize_bot/httputil"
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
	return httputil.NewClient(timeout)
}

// DebugHTTPClient creates an HTTP client that logs all requests and responses.
func DebugHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: &debugTransport{inner: httputil.NewTransport()},
	}
}

// ImageInput is an image attached to a Message. Bytes is the raw image
// payload; MIMEType is the content type (e.g. "image/jpeg"). Providers that
// don't support vision must ignore Images.
type ImageInput struct {
	Bytes    []byte
	MIMEType string
}

// Message represents a chat message for the LLM.
type Message struct {
	Role    string // "system", "user", "assistant"
	Content string
	// Images is the optional list of images attached to this message.
	// Providers without multimodal support ignore this field.
	Images []ImageInput
}

// Operation labels identify which logical task an LLM call serves. They are
// recorded with token usage so the /usage report can break usage down by task.
const (
	OpCluster   = "cluster"
	OpSummarize = "summarize"
	OpText      = "text"
	OpURL       = "url"
	OpVision    = "vision"
	OpProbe     = "probe" // throwaway quota probe; excluded from usage reports
)

// CompletionRequest is an API-agnostic request to the LLM.
type CompletionRequest struct {
	Model       string
	Messages    []Message
	MaxTokens   int
	Temperature float32
	// Operation tags the call for usage accounting (see Op* constants). Empty
	// is allowed and recorded as an unlabeled operation.
	Operation string
}

// TokenUsage is the token accounting reported by the LLM for a single call.
type TokenUsage struct {
	PromptTokens      int
	CachedInputTokens int // subset of PromptTokens served from cache, when reported
	CompletionTokens  int
	TotalTokens       int
}

// CompletionResponse is an API-agnostic response from the LLM.
type CompletionResponse struct {
	Content        string
	FinishReason   string // "stop", "length", etc.
	HTTPStatusCode int
	Usage          TokenUsage
}

// RateLimitWindow describes one rolling quota window (e.g. the Codex 5h or 7d limit).
type RateLimitWindow struct {
	UsedPercent   float64
	WindowMinutes int
	ResetsAt      time.Time // zero if unknown
}

// RateLimitSnapshot is a point-in-time view of the Codex account quota,
// captured from the x-codex-* response headers (OAuth mode only).
type RateLimitSnapshot struct {
	CapturedAt time.Time
	PlanType   string           // e.g. "plus"; empty if unknown
	Primary    *RateLimitWindow // ~5h "session" window
	Secondary  *RateLimitWindow // ~7d "weekly" window
}

// Recorder is the sink the provider writes usage and quota observations to.
// Implemented by *db.DB; a nil Recorder disables recording.
type Recorder interface {
	RecordTokenUsage(ctx context.Context, model, operation string, u TokenUsage)
	SaveCodexRateLimits(ctx context.Context, snap RateLimitSnapshot)
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

// VisionCapable is implemented by clients that can describe whether a given
// model supports multimodal image inputs. Callers (e.g. the image describer)
// gate vision attempts on this check.
type VisionCapable interface {
	SupportsVision(model string) bool
}

// defaultLLMHTTPTimeout is used when a constructor is called with a
// non-positive timeout (e.g. tests that don't care).
const defaultLLMHTTPTimeout = 180 * time.Second

// New creates the appropriate LLM client based on config. When rec is non-nil,
// the client records per-call token usage and (in OAuth mode) Codex quota
// snapshots through it.
func New(cfg *config.Config, rec Recorder) (LLMClient, error) {
	timeout := cfg.LLMHTTPTimeout()
	var (
		client LLMClient
		err    error
	)
	switch cfg.LLMMode {
	case config.LLMModeCompletions, "":
		client, err = NewCompletionsClient(cfg.LLMToken, cfg.LLMEndpoint, timeout)
	case config.LLMModeResponses:
		client, err = NewResponsesClient(cfg.LLMToken, cfg.LLMEndpoint, timeout, WithRecorder(rec))
	case config.LLMModeOAuth:
		client, err = NewOAuthClient(cfg.OAuthTokenDir, cfg.OAuthClientID, cfg.OAuthCodexVersion, timeout, WithRecorder(rec))
	default:
		return nil, fmt.Errorf("unknown LLM mode: %q", cfg.LLMMode)
	}
	if err != nil {
		return nil, err
	}
	if rec != nil {
		client = &recordingClient{inner: client, rec: rec}
	}
	return client, nil
}

// clientOptions collects optional behaviour shared by the client constructors.
type clientOptions struct{ rec Recorder }

// ClientOption configures an LLM client at construction.
type ClientOption func(*clientOptions)

// WithRecorder installs a usage/quota recorder. A nil recorder is ignored.
func WithRecorder(rec Recorder) ClientOption {
	return func(o *clientOptions) { o.rec = rec }
}

func applyClientOptions(opts []ClientOption) clientOptions {
	var o clientOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// recordingClient wraps an LLMClient and records token usage per call.
type recordingClient struct {
	inner LLMClient
	rec   Recorder
}

func (c *recordingClient) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	resp, err := c.inner.Complete(ctx, req)
	if err == nil && c.rec != nil && resp.Usage.TotalTokens > 0 {
		c.rec.RecordTokenUsage(ctx, req.Model, req.Operation, resp.Usage)
	}
	return resp, err
}

// SupportsVision forwards the capability check so vision gating keeps working
// through the recording wrapper.
func (c *recordingClient) SupportsVision(model string) bool {
	if vc, ok := c.inner.(VisionCapable); ok {
		return vc.SupportsVision(model)
	}
	return false
}

// CodexTokenStore forwards the Codex credentials accessor through the wrapper.
func (c *recordingClient) CodexTokenStore() *TokenStore {
	if h, ok := c.inner.(CodexTokenStorer); ok {
		return h.CodexTokenStore()
	}
	return nil
}

// codexRateLimitTransport captures Codex quota headers from responses and
// records the latest snapshot. It is installed only when a recorder is present.
type codexRateLimitTransport struct {
	inner http.RoundTripper
	rec   Recorder
}

func (t *codexRateLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(req)
	if err == nil && resp != nil && t.rec != nil {
		if snap, ok := ParseCodexRateLimits(resp.Header); ok {
			// Synchronous so callers (e.g. a quota probe) can reload the fresh
			// snapshot immediately; the write is a fast local SQLite upsert.
			t.rec.SaveCodexRateLimits(context.Background(), snap)
		}
	}
	return resp, err
}

// ParseCodexRateLimits extracts the Codex quota windows from x-codex-* response
// headers. Returns ok=false when no recognizable rate-limit headers are present.
func ParseCodexRateLimits(h http.Header) (RateLimitSnapshot, bool) {
	primary := parseCodexWindow(h, "primary")
	secondary := parseCodexWindow(h, "secondary")
	if primary == nil && secondary == nil {
		return RateLimitSnapshot{}, false
	}
	snap := RateLimitSnapshot{
		CapturedAt: time.Now(),
		PlanType:   firstHeader(h, "x-codex-plan-type", "x-codex-primary-limit-name"),
		Primary:    primary,
		Secondary:  secondary,
	}
	return snap, true
}

func parseCodexWindow(h http.Header, tier string) *RateLimitWindow {
	used := h.Get("x-codex-" + tier + "-used-percent")
	window := h.Get("x-codex-" + tier + "-window-minutes")
	if used == "" && window == "" {
		return nil
	}
	w := &RateLimitWindow{}
	if v, err := strconv.ParseFloat(used, 64); err == nil {
		w.UsedPercent = v
	}
	if v, err := strconv.Atoi(window); err == nil {
		w.WindowMinutes = v
	}
	w.ResetsAt = parseCodexReset(h, tier)
	return w
}

// parseCodexReset reads the window reset time, tolerating both the
// "-reset-after-seconds" (relative) and "-reset-at" (absolute) header variants.
func parseCodexReset(h http.Header, tier string) time.Time {
	if v := h.Get("x-codex-" + tier + "-reset-after-seconds"); v != "" {
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.Now().Add(time.Duration(secs) * time.Second)
		}
	}
	if v := h.Get("x-codex-" + tier + "-reset-at"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t
		}
		if unix, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.Unix(unix, 0)
		}
	}
	return time.Time{}
}

func firstHeader(h http.Header, keys ...string) string {
	for _, k := range keys {
		if v := h.Get(k); v != "" {
			return v
		}
	}
	return ""
}
