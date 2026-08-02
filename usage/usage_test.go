package usage

import (
	"context"
	"errors"
	"strings"
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
	err         error // returned from every query when set
}

func (f *fakeStore) SumTokenUsageSince(context.Context, time.Time) (db.TokenUsageTotals, error) {
	return f.totals, f.err
}
func (f *fakeStore) TokenUsageByModelSince(context.Context, time.Time) ([]db.TokenUsageGroup, error) {
	return f.byModel, f.err
}
func (f *fakeStore) TokenUsageByOperationSince(context.Context, time.Time) ([]db.TokenUsageGroup, error) {
	return f.byOperation, f.err
}
func (f *fakeStore) LatestPromptTokens(context.Context) (model string, promptTokens int, err error) {
	return f.latestModel, f.latestTok, f.err
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

func TestBuildSurfacesStoreErrors(t *testing.T) {
	// A locked DB must not render as a confident "Нет данных".
	src := &fakeStore{err: errors.New("database is locked")}
	r := Build(context.Background(), src, "gpt-5", 0, QuotaResult{})
	if !r.StoreErr {
		t.Fatal("StoreErr not set on failing store")
	}
	out := r.Format()
	if !strings.Contains(out, "Данные недоступны") {
		t.Errorf("Format() = %q, want unavailable notice", out)
	}
	if strings.Contains(out, "Нет данных") {
		t.Errorf("Format() = %q, must not claim there is simply no data", out)
	}
}

func TestBuildNoErrorsNoStoreErr(t *testing.T) {
	r := Build(context.Background(), &fakeStore{}, "gpt-5", 0, QuotaResult{})
	if r.StoreErr {
		t.Fatal("StoreErr set on healthy store")
	}
	if out := r.Format(); !strings.Contains(out, "Нет данных") {
		t.Errorf("Format() = %q, want plain no-data notice", out)
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
