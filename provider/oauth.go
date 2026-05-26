package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	ChatGPTCodexBaseURL = "https://chatgpt.com/backend-api/codex"
	ChatGPTWhamUsageURL = "https://chatgpt.com/backend-api/wham/usage"
	CodexClientVersion  = "0.124.0"
	CodexOriginator     = "codex-tui"
	HeaderAccountID     = "ChatGPT-Account-ID"
)

type oauthClient struct {
	inner      LLMClient
	tokenStore *TokenStore
}

// NewOAuthClient creates an LLM client that authenticates via OAuth tokens.
// Uses the ChatGPT backend Codex API with the OAuth access_token,
// ChatGPT-Account-ID header, and a Codex client version header.
func NewOAuthClient(tokenDir, clientID, codexVersion string, timeout time.Duration, opts ...ClientOption) (LLMClient, error) {
	store := NewTokenStore(tokenDir, clientID)
	if err := store.Load(); err != nil {
		return nil, fmt.Errorf("load OAuth tokens: %w (run '%s openai auth' first)", err, os.Args[0])
	}

	codexVersion = resolveCodexClientVersion(codexVersion)

	// Get initial token to verify it's valid
	token, err := store.GetValidToken()
	if err != nil {
		return nil, err
	}

	inner, err := NewResponsesClient(token, ChatGPTCodexBaseURL, timeout, opts...)
	if err != nil {
		return nil, fmt.Errorf("create responses client: %w", err)
	}
	if rc, ok := inner.(*responsesClient); ok {
		rc.accountID = store.GetAccountID()
		rc.codexClientVersion = codexVersion
	}

	return &oauthClient{
		inner:      inner,
		tokenStore: store,
	}, nil
}

func (c *oauthClient) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	// Get a valid token (auto-refreshes if needed)
	token, err := c.tokenStore.GetValidToken()
	if err != nil {
		return CompletionResponse{}, fmt.Errorf("get OAuth token: %w", err)
	}

	// Update token and account ID on the inner responses client.
	if rc, ok := c.inner.(*responsesClient); ok {
		rc.token = token
		rc.accountID = c.tokenStore.GetAccountID()
	}

	return c.inner.Complete(ctx, req)
}

// CodexTokenStore exposes the underlying token store so quota helpers can reuse
// the already-loaded credentials (account ID, plan, refresh).
func (c *oauthClient) CodexTokenStore() *TokenStore { return c.tokenStore }

// CodexTokenStorer is implemented by clients backed by Codex OAuth credentials.
type CodexTokenStorer interface {
	CodexTokenStore() *TokenStore
}

// CodexStoreOf unwraps client decorators to find the Codex token store, or nil.
func CodexStoreOf(client LLMClient) *TokenStore {
	if h, ok := client.(CodexTokenStorer); ok {
		return h.CodexTokenStore()
	}
	return nil
}

// FetchWhamUsage performs a best-effort GET of the (undocumented) Codex usage
// endpoint and parses it into a snapshot. The schema is not contractual, so any
// HTTP or parse failure returns an error and callers should fall back.
func FetchWhamUsage(ctx context.Context, store *TokenStore) (RateLimitSnapshot, error) {
	if store == nil {
		return RateLimitSnapshot{}, fmt.Errorf("no token store")
	}
	token, err := store.GetValidToken()
	if err != nil {
		return RateLimitSnapshot{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ChatGPTWhamUsageURL, http.NoBody)
	if err != nil {
		return RateLimitSnapshot{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("version", CodexClientVersion)
	req.Header.Set("originator", CodexOriginator)
	if id := store.GetAccountID(); id != "" {
		req.Header.Set(HeaderAccountID, id)
	}

	resp, err := HTTPClient(10 * time.Second).Do(req)
	if err != nil {
		return RateLimitSnapshot{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return RateLimitSnapshot{}, fmt.Errorf("wham usage HTTP %d", resp.StatusCode)
	}

	// Headers may also carry the limits; prefer them when present.
	if snap, ok := ParseCodexRateLimits(resp.Header); ok {
		snap.PlanType = firstNonEmpty(snap.PlanType, store.GetPlanType())
		return snap, nil
	}

	var body whamUsageBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return RateLimitSnapshot{}, fmt.Errorf("decode wham usage: %w", err)
	}
	snap, ok := body.toSnapshot()
	if !ok {
		return RateLimitSnapshot{}, fmt.Errorf("wham usage: no rate-limit data")
	}
	snap.PlanType = firstNonEmpty(snap.PlanType, store.GetPlanType())
	return snap, nil
}

// whamUsageBody mirrors the (undocumented) rate_limits JSON shape. Parsed
// defensively; missing fields just yield a partial snapshot.
type whamUsageBody struct {
	RateLimits struct {
		Primary   *whamWindow `json:"primary"`
		Secondary *whamWindow `json:"secondary"`
	} `json:"rate_limits"`
	PlanType string `json:"plan_type"`
}

type whamWindow struct {
	UsedPercent     float64 `json:"used_percent"`
	WindowMinutes   int     `json:"window_minutes"`
	ResetsInSeconds int64   `json:"resets_in_seconds"`
}

func (w *whamWindow) toWindow() *RateLimitWindow {
	if w == nil {
		return nil
	}
	out := &RateLimitWindow{UsedPercent: w.UsedPercent, WindowMinutes: w.WindowMinutes}
	if w.ResetsInSeconds > 0 {
		out.ResetsAt = time.Now().Add(time.Duration(w.ResetsInSeconds) * time.Second)
	}
	return out
}

func (b whamUsageBody) toSnapshot() (RateLimitSnapshot, bool) {
	primary := b.RateLimits.Primary.toWindow()
	secondary := b.RateLimits.Secondary.toWindow()
	if primary == nil && secondary == nil {
		return RateLimitSnapshot{}, false
	}
	return RateLimitSnapshot{
		CapturedAt: time.Now(),
		PlanType:   b.PlanType,
		Primary:    primary,
		Secondary:  secondary,
	}, true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func resolveCodexClientVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return CodexClientVersion
	}
	return version
}
