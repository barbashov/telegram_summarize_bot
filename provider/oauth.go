package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	ChatGPTCodexBaseURL = "https://chatgpt.com/backend-api/codex"
	ChatGPTWhamUsageURL = "https://chatgpt.com/backend-api/wham/usage"
	CodexClientVersion  = "0.161.0"
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
	store.codexVersion = resolveCodexClientVersion(codexVersion)
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

	complete := func(token string) (CompletionResponse, error) {
		if rc, ok := c.inner.(*responsesClient); ok {
			return rc.complete(ctx, req, token, c.tokenStore.GetAccountID())
		}
		return c.inner.Complete(ctx, req)
	}
	resp, err := complete(token)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatusCode != http.StatusUnauthorized || ctx.Err() != nil {
		return resp, err
	}
	token, err = c.tokenStore.refreshRejected(token)
	if err != nil {
		return resp, fmt.Errorf("renew rejected OAuth credentials: %w", err)
	}
	return complete(token)
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
	req.Header.Set("version", resolveCodexClientVersion(store.codexVersion))
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
	UsedPercent     *float64 `json:"used_percent"`
	WindowMinutes   int      `json:"window_minutes"`
	ResetsInSeconds int64    `json:"resets_in_seconds"`
}

func (w *whamWindow) toWindow() *RateLimitWindow {
	if w == nil {
		return nil
	}
	out := &RateLimitWindow{UnknownUsage: true, WindowMinutes: w.WindowMinutes}
	if w.UsedPercent != nil && *w.UsedPercent >= 0 && *w.UsedPercent <= 100 {
		out.UsedPercent, out.UnknownUsage = *w.UsedPercent, false
	}
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

// ModelContext reads the advertised context size for an exact Codex model.
func (s *TokenStore) ModelContext(ctx context.Context, model string) int {
	if model == "" {
		return 0
	}
	s.mu.Lock()
	windows, observed := s.contextWindows, s.contextWindowsAt
	s.mu.Unlock()
	if time.Since(observed) < time.Hour {
		return windows[model]
	}
	token, err := s.GetValidToken()
	if err != nil {
		return 0
	}
	version := resolveCodexClientVersion(s.codexVersion)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ChatGPTCodexBaseURL+"/models?client_version="+url.QueryEscape(version), http.NoBody)
	if err != nil {
		return 0
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("version", version)
	req.Header.Set("originator", CodexOriginator)
	if account := s.GetAccountID(); account != "" {
		req.Header.Set(HeaderAccountID, account)
	}
	resp, err := HTTPClient(5 * time.Second).Do(req)
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	var catalog struct {
		Models []struct {
			Slug    string `json:"slug"`
			Context int    `json:"context_window"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&catalog); err != nil {
		return 0
	}
	windows = make(map[string]int, len(catalog.Models))
	for _, entry := range catalog.Models {
		if entry.Context > 0 {
			windows[entry.Slug] = entry.Context
		}
	}
	s.mu.Lock()
	s.contextWindows, s.contextWindowsAt = windows, time.Now()
	s.mu.Unlock()
	return windows[model]
}
