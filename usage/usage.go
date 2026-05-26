// Package usage builds the /usage report: persisted token-usage history plus
// the Codex account quota, shared by the admin command and the CLI.
package usage

import (
	"context"
	"strings"
	"time"

	"telegram_summarize_bot/db"
)

// Store is the read side of token-usage aggregation (satisfied by *db.DB).
type Store interface {
	SumTokenUsageSince(ctx context.Context, since time.Time) (db.TokenUsageTotals, error)
	TokenUsageByModelSince(ctx context.Context, since time.Time) ([]db.TokenUsageGroup, error)
	TokenUsageByOperationSince(ctx context.Context, since time.Time) ([]db.TokenUsageGroup, error)
	LatestPromptTokens(ctx context.Context) (model string, promptTokens int, err error)
}

// Window is one labeled time window with its aggregate totals.
type Window struct {
	Label  string
	Totals db.TokenUsageTotals
}

// Report is the fully-aggregated /usage view, ready to Format.
type Report struct {
	Windows     []Window // today, 7d, 30d
	ByModel     []db.TokenUsageGroup
	ByOperation []db.TokenUsageGroup
	ContextUsed int
	ContextMax  int // 0 => unknown, context line omitted
	Quota       QuotaResult
}

// breakdownWindow is the lookback used for the per-model / per-operation tables.
const breakdownDays = 30

// Build aggregates token usage from the store. model is the configured model
// (fallback for the context line); contextOverride (>0) forces the context-window
// denominator. quota is the already-resolved Codex quota (zero value if none).
func Build(ctx context.Context, src Store, model string, contextOverride int, quota QuotaResult) Report {
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	windows := []struct {
		label string
		since time.Time
	}{
		{"Сегодня", midnight},
		{"7 дней", now.AddDate(0, 0, -7)},
		{"30 дней", now.AddDate(0, 0, -30)},
	}

	var r Report
	for _, w := range windows {
		totals, _ := src.SumTokenUsageSince(ctx, w.since)
		r.Windows = append(r.Windows, Window{Label: w.label, Totals: totals})
	}

	breakdownSince := now.AddDate(0, 0, -breakdownDays)
	r.ByModel, _ = src.TokenUsageByModelSince(ctx, breakdownSince)
	r.ByOperation, _ = src.TokenUsageByOperationSince(ctx, breakdownSince)

	latestModel, prompt, _ := src.LatestPromptTokens(ctx)
	if latestModel == "" {
		latestModel = model
	}
	r.ContextUsed = prompt
	if contextOverride > 0 {
		r.ContextMax = contextOverride
	} else {
		r.ContextMax = modelContextWindow(latestModel)
	}

	r.Quota = quota
	return r
}

// FormatModel returns the configured model name for display headers.
func (r Report) hasData() bool {
	for _, w := range r.Windows {
		if w.Totals.Calls > 0 {
			return true
		}
	}
	return r.Quota.Snapshot != nil
}

// modelContextWindow returns a best-effort max context size (tokens) for known
// model families, or 0 when unknown.
func modelContextWindow(model string) int {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "gpt-5"):
		return 272000
	case strings.Contains(m, "gpt-4o"), strings.Contains(m, "gpt-4.1"):
		return 128000
	case strings.Contains(m, "o1"), strings.Contains(m, "o3"):
		return 200000
	case strings.Contains(m, "llama-3.1"), strings.Contains(m, "llama-3.3"):
		return 128000
	case strings.Contains(m, "claude"):
		return 200000
	default:
		return 0
	}
}
