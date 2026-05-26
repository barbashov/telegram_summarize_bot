package usage

import (
	"context"
	"testing"
	"time"

	"telegram_summarize_bot/db"
)

type fakeStore struct {
	totals      db.TokenUsageTotals
	byModel     []db.TokenUsageGroup
	byOperation []db.TokenUsageGroup
	latestModel string
	latestTok   int
}

func (f *fakeStore) SumTokenUsageSince(context.Context, time.Time) (db.TokenUsageTotals, error) {
	return f.totals, nil
}
func (f *fakeStore) TokenUsageByModelSince(context.Context, time.Time) ([]db.TokenUsageGroup, error) {
	return f.byModel, nil
}
func (f *fakeStore) TokenUsageByOperationSince(context.Context, time.Time) ([]db.TokenUsageGroup, error) {
	return f.byOperation, nil
}
func (f *fakeStore) LatestPromptTokens(context.Context) (model string, promptTokens int, err error) {
	return f.latestModel, f.latestTok, nil
}

func TestBuild(t *testing.T) {
	src := &fakeStore{
		totals:      db.TokenUsageTotals{TotalTokens: 500, Calls: 3},
		byModel:     []db.TokenUsageGroup{{Label: "gpt-5", TotalTokens: 500}},
		byOperation: []db.TokenUsageGroup{{Label: "summarize", TotalTokens: 500}},
		latestModel: "gpt-5",
		latestTok:   1000,
	}

	r := Build(context.Background(), src, "gpt-5", 0, QuotaResult{})
	if len(r.Windows) != 3 {
		t.Fatalf("windows = %d, want 3", len(r.Windows))
	}
	if r.Windows[0].Totals.TotalTokens != 500 {
		t.Errorf("today total = %d, want 500", r.Windows[0].Totals.TotalTokens)
	}
	if r.ContextUsed != 1000 || r.ContextMax != 272000 { // gpt-5 -> 272k
		t.Errorf("context = %d/%d, want 1000/272000", r.ContextUsed, r.ContextMax)
	}
}

func TestBuildContextOverride(t *testing.T) {
	src := &fakeStore{latestModel: "unknown-model", latestTok: 500}
	r := Build(context.Background(), src, "unknown-model", 8000, QuotaResult{})
	if r.ContextMax != 8000 {
		t.Errorf("context max = %d, want override 8000", r.ContextMax)
	}
}

func TestModelContextWindow(t *testing.T) {
	cases := map[string]int{
		"gpt-5.5":                  272000,
		"gpt-4o":                   128000,
		"meta-llama/llama-3.3-70b": 128000,
		"claude-sonnet":            200000,
		"some-unknown-model":       0,
	}
	for model, want := range cases {
		if got := modelContextWindow(model); got != want {
			t.Errorf("modelContextWindow(%q) = %d, want %d", model, got, want)
		}
	}
}
