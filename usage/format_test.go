package usage

import (
	"strings"
	"testing"
	"time"

	"telegram_summarize_bot/db"
	"telegram_summarize_bot/provider"
)

func TestReportFormatFull(t *testing.T) {
	r := Report{
		Windows: []Window{
			{Label: "Сегодня", Totals: db.TokenUsageTotals{PromptTokens: 117543, CachedTokens: 1647616, CompletionTokens: 4811, TotalTokens: 1769970, Calls: 36}},
			{Label: "7 дней", Totals: db.TokenUsageTotals{TotalTokens: 12400000, Calls: 980}},
			{Label: "30 дней", Totals: db.TokenUsageTotals{TotalTokens: 51000000, Calls: 4100}},
		},
		ByModel:     []db.TokenUsageGroup{{Label: "gpt-5.5", TotalTokens: 1769970, Calls: 36}},
		ByOperation: []db.TokenUsageGroup{{Label: "summarize", TotalTokens: 1200000}, {Label: "cluster", TotalTokens: 480000}},
		ContextUsed: 69032,
		ContextMax:  272000,
		Quota: QuotaResult{
			Source: SourceLive,
			Snapshot: &provider.RateLimitSnapshot{
				CapturedAt: time.Now(),
				PlanType:   "plus",
				Primary:    &provider.RateLimitWindow{UsedPercent: 16, WindowMinutes: 300, ResetsAt: time.Now().Add(3*time.Hour + 25*time.Minute)},
				Secondary:  &provider.RateLimitWindow{UsedPercent: 28, WindowMinutes: 10080, ResetsAt: time.Now().Add(4*24*time.Hour + 11*time.Hour)},
			},
		},
	}

	out := r.Format()
	for _, want := range []string{
		"📊 Использование токенов",
		"Контекст: 69 032 / 272 000 (25%)",
		"По модели",
		"По операции",
		"📈 Лимиты аккаунта",
		"Провайдер: openai-codex (Plus)",
		"Сессия (5ч): 84% осталось (16% исп.)",
		"Неделя (7д): 72% осталось (28% исп.)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

func TestReportFormatNoQuota(t *testing.T) {
	r := Report{
		Windows: []Window{
			{Label: "Сегодня", Totals: db.TokenUsageTotals{TotalTokens: 1000, Calls: 2}},
			{Label: "7 дней", Totals: db.TokenUsageTotals{}},
			{Label: "30 дней", Totals: db.TokenUsageTotals{}},
		},
	}
	out := r.Format()
	if strings.Contains(out, "Лимиты аккаунта") {
		t.Errorf("quota block should be absent without a snapshot:\n%s", out)
	}
}

func TestReportFormatEmpty(t *testing.T) {
	out := Report{Windows: []Window{{Label: "Сегодня"}}}.Format()
	if !strings.Contains(out, "Нет данных") {
		t.Errorf("expected 'Нет данных', got:\n%s", out)
	}
}

func TestAbbrev(t *testing.T) {
	cases := map[int64]string{0: "0", 980: "980", 4811: "4.8k", 117543: "117.5k", 1769970: "1.8M", 1000000: "1M"}
	for n, want := range cases {
		if got := abbrev(n); got != want {
			t.Errorf("abbrev(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestWindowLabel(t *testing.T) {
	cases := map[int]string{300: "5ч", 10080: "7д", 60: "1ч", 0: "?"}
	for m, want := range cases {
		if got := windowLabel(m); got != want {
			t.Errorf("windowLabel(%d) = %q, want %q", m, got, want)
		}
	}
}

func TestGroupThousands(t *testing.T) {
	cases := map[int]string{69032: "69 032", 272000: "272 000", 999: "999", 1000000: "1 000 000"}
	for n, want := range cases {
		if got := groupThousands(n); got != want {
			t.Errorf("groupThousands(%d) = %q, want %q", n, got, want)
		}
	}
}
