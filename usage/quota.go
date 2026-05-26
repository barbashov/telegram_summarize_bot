package usage

import (
	"context"
	"time"

	"telegram_summarize_bot/logger"
	"telegram_summarize_bot/provider"
)

// Quota source labels.
const (
	SourceCache = "cache"
	SourceWham  = "wham"
	SourceLive  = "live"
)

// QuotaResult is a resolved Codex quota snapshot plus where it came from.
type QuotaResult struct {
	Snapshot *provider.RateLimitSnapshot
	Source   string // SourceCache / SourceWham / SourceLive, or "" if unavailable
}

// QuotaStore is the persistence the resolver needs (satisfied by *db.DB).
type QuotaStore interface {
	LoadCodexRateLimits(ctx context.Context) (provider.RateLimitSnapshot, bool)
	SaveCodexRateLimits(ctx context.Context, snap provider.RateLimitSnapshot)
}

// ResolveCodexQuota returns the current Codex quota using a tiered strategy:
//  1. the persisted snapshot if newer than ttl;
//  2. otherwise a best-effort fetch of the (undocumented) wham usage endpoint;
//  3. otherwise a minimal throwaway "probe" completion whose response headers
//     the capture transport persists, which is then reloaded.
//
// Falls back to a stale cached snapshot if every live attempt fails. Only
// meaningful in OAuth/Codex mode; client must carry Codex credentials.
func ResolveCodexQuota(ctx context.Context, store QuotaStore, client provider.LLMClient, model string, ttl time.Duration) QuotaResult {
	var stale *provider.RateLimitSnapshot
	if snap, ok := store.LoadCodexRateLimits(ctx); ok {
		if time.Since(snap.CapturedAt) < ttl {
			s := snap
			return QuotaResult{Snapshot: &s, Source: SourceCache}
		}
		s := snap
		stale = &s
	}

	tokenStore := provider.CodexStoreOf(client)

	// Tier 2: wham endpoint.
	if tokenStore != nil {
		if snap, err := provider.FetchWhamUsage(ctx, tokenStore); err == nil {
			store.SaveCodexRateLimits(ctx, snap)
			s := snap
			return QuotaResult{Snapshot: &s, Source: SourceWham}
		} else {
			logger.Debug().Err(err).Msg("wham usage fetch failed; trying live probe")
		}
	}

	// Tier 3: live throwaway probe. The capture transport persists the fresh
	// snapshot synchronously, so we can reload it immediately afterwards.
	if client != nil {
		if _, err := client.Complete(ctx, provider.CompletionRequest{
			Model:     model,
			Operation: provider.OpProbe,
			Messages:  []provider.Message{{Role: "user", Content: "ping"}},
			MaxTokens: 16,
		}); err == nil {
			if snap, ok := store.LoadCodexRateLimits(ctx); ok {
				s := snap
				return QuotaResult{Snapshot: &s, Source: SourceLive}
			}
		} else {
			logger.Debug().Err(err).Msg("codex quota probe failed")
		}
	}

	if stale != nil {
		return QuotaResult{Snapshot: stale, Source: SourceCache}
	}
	return QuotaResult{}
}
