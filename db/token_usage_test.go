package db

import (
	"context"
	"testing"
	"time"

	"telegram_summarize_bot/provider"
)

func TestTokenUsageAggregation(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	for _, u := range []struct {
		model, op                         string
		prompt, cached, completion, total int
	}{
		{"gpt-5", provider.OpSummarize, 100, 20, 50, 150},
		{"gpt-5", provider.OpCluster, 200, 40, 30, 230},
		{"gpt-4o", provider.OpVision, 300, 0, 10, 310},
		{"gpt-5", provider.OpProbe, 5, 0, 1, 6}, // excluded from aggregation
	} {
		if err := d.InsertTokenUsage(ctx, u.model, u.op, u.prompt, u.cached, u.completion, u.total); err != nil {
			t.Fatalf("InsertTokenUsage: %v", err)
		}
	}

	totals, err := d.SumTokenUsageSince(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("SumTokenUsageSince: %v", err)
	}
	if totals.TotalTokens != 690 { // 150+230+310, probe excluded
		t.Errorf("total = %d, want 690", totals.TotalTokens)
	}
	if totals.CachedTokens != 60 {
		t.Errorf("cached = %d, want 60", totals.CachedTokens)
	}
	if totals.Calls != 3 {
		t.Errorf("calls = %d, want 3 (probe excluded)", totals.Calls)
	}

	byModel, err := d.TokenUsageByModelSince(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("TokenUsageByModelSince: %v", err)
	}
	if len(byModel) != 2 || byModel[0].Label != "gpt-5" || byModel[0].TotalTokens != 380 {
		t.Errorf("byModel = %+v, want gpt-5 first with 380", byModel)
	}

	byOp, err := d.TokenUsageByOperationSince(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("TokenUsageByOperationSince: %v", err)
	}
	for _, g := range byOp {
		if g.Label == provider.OpProbe {
			t.Errorf("probe should be excluded, got %+v", byOp)
		}
	}
}

func TestLatestPromptTokens(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	if model, prompt, err := d.LatestPromptTokens(ctx); err != nil || model != "" || prompt != 0 {
		t.Fatalf("empty: got (%q, %d, %v), want empty", model, prompt, err)
	}

	_ = d.InsertTokenUsage(ctx, "gpt-5", provider.OpSummarize, 111, 0, 22, 133)
	_ = d.InsertTokenUsage(ctx, "gpt-5", provider.OpProbe, 9, 0, 1, 10) // ignored

	model, prompt, err := d.LatestPromptTokens(ctx)
	if err != nil {
		t.Fatalf("LatestPromptTokens: %v", err)
	}
	if model != "gpt-5" || prompt != 111 {
		t.Errorf("got (%q, %d), want (gpt-5, 111)", model, prompt)
	}
}

func TestCodexRateLimitsRoundTrip(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	if _, ok := d.LoadCodexRateLimits(ctx); ok {
		t.Fatal("expected no snapshot initially")
	}

	snap := provider.RateLimitSnapshot{
		CapturedAt: time.Now().Truncate(time.Second),
		PlanType:   "plus",
		Primary:    &provider.RateLimitWindow{UsedPercent: 16, WindowMinutes: 300, ResetsAt: time.Now().Add(time.Hour).Truncate(time.Second)},
		Secondary:  &provider.RateLimitWindow{UsedPercent: 28, WindowMinutes: 10080},
	}
	d.SaveCodexRateLimits(ctx, snap)

	got, ok := d.LoadCodexRateLimits(ctx)
	if !ok {
		t.Fatal("expected snapshot after save")
	}
	if got.PlanType != "plus" || got.Primary == nil || got.Primary.WindowMinutes != 300 {
		t.Errorf("loaded snapshot mismatch: %+v", got)
	}

	// Overwrite replaces the previous snapshot.
	snap.PlanType = "pro"
	d.SaveCodexRateLimits(ctx, snap)
	got, _ = d.LoadCodexRateLimits(ctx)
	if got.PlanType != "pro" {
		t.Errorf("plan = %q, want pro after overwrite", got.PlanType)
	}
}

func TestPurgeOldTokenUsage(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	_ = d.InsertTokenUsage(ctx, "gpt-5", provider.OpSummarize, 10, 0, 5, 15)
	purged, err := d.PurgeOldTokenUsage(ctx, time.Now().Add(time.Hour)) // everything older than 1h ahead => all
	if err != nil {
		t.Fatalf("PurgeOldTokenUsage: %v", err)
	}
	if purged != 1 {
		t.Errorf("purged = %d, want 1", purged)
	}
}
