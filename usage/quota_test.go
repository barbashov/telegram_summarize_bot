package usage

import (
	"context"
	"errors"
	"testing"
	"time"

	"telegram_summarize_bot/provider"
)

type fakeQuotaStore struct {
	snap *provider.RateLimitSnapshot
}

func (f *fakeQuotaStore) LoadCodexRateLimits(context.Context) (provider.RateLimitSnapshot, bool) {
	if f.snap == nil {
		return provider.RateLimitSnapshot{}, false
	}
	return *f.snap, true
}
func (f *fakeQuotaStore) SaveCodexRateLimits(_ context.Context, snap provider.RateLimitSnapshot) {
	s := snap
	f.snap = &s
}

// probeClient simulates the capture transport: on Complete it writes a fresh
// snapshot to the store (as the real transport does from response headers).
type probeClient struct {
	store    *fakeQuotaStore
	fail     bool
	onProbe  *provider.RateLimitSnapshot
	probedAt *bool
}

func (c *probeClient) Complete(ctx context.Context, _ provider.CompletionRequest) (provider.CompletionResponse, error) {
	if c.probedAt != nil {
		*c.probedAt = true
	}
	if c.fail {
		return provider.CompletionResponse{}, errors.New("probe failed")
	}
	if c.onProbe != nil {
		c.store.SaveCodexRateLimits(ctx, *c.onProbe)
	}
	return provider.CompletionResponse{Content: "pong"}, nil
}

func TestResolveCodexQuotaFreshCache(t *testing.T) {
	store := &fakeQuotaStore{snap: &provider.RateLimitSnapshot{CapturedAt: time.Now()}}
	probed := false
	client := &probeClient{store: store, probedAt: &probed}

	res := ResolveCodexQuota(context.Background(), store, client, "gpt-5", 15*time.Minute)
	if res.Source != SourceCache {
		t.Errorf("source = %q, want cache", res.Source)
	}
	if probed {
		t.Error("should not probe when cache is fresh")
	}
}

func TestResolveCodexQuotaStaleFallsBackToProbe(t *testing.T) {
	store := &fakeQuotaStore{snap: &provider.RateLimitSnapshot{CapturedAt: time.Now().Add(-time.Hour)}}
	fresh := &provider.RateLimitSnapshot{CapturedAt: time.Now(), PlanType: "plus"}
	client := &probeClient{store: store, onProbe: fresh}

	res := ResolveCodexQuota(context.Background(), store, client, "gpt-5", 15*time.Minute)
	if res.Source != SourceLive {
		t.Errorf("source = %q, want live", res.Source)
	}
	if res.Snapshot == nil || res.Snapshot.PlanType != "plus" {
		t.Errorf("snapshot = %+v, want fresh plus", res.Snapshot)
	}
}

func TestResolveCodexQuotaProbeFailsKeepsStale(t *testing.T) {
	stale := &provider.RateLimitSnapshot{CapturedAt: time.Now().Add(-time.Hour), PlanType: "old"}
	store := &fakeQuotaStore{snap: stale}
	client := &probeClient{store: store, fail: true}

	res := ResolveCodexQuota(context.Background(), store, client, "gpt-5", 15*time.Minute)
	if res.Source != SourceCache || res.Snapshot == nil || res.Snapshot.PlanType != "old" {
		t.Errorf("expected stale cache fallback, got source=%q snap=%+v", res.Source, res.Snapshot)
	}
}

func TestResolveCodexQuotaNothingAvailable(t *testing.T) {
	store := &fakeQuotaStore{}
	client := &probeClient{store: store, fail: true}

	res := ResolveCodexQuota(context.Background(), store, client, "gpt-5", 15*time.Minute)
	if res.Source != "" || res.Snapshot != nil {
		t.Errorf("expected empty result, got source=%q snap=%+v", res.Source, res.Snapshot)
	}
}
