package usage

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"telegram_summarize_bot/provider"
)

// Format renders the report as plain text (no Markdown), matching the /status
// style. The account-limits block is included only when a quota snapshot exists.
func (r Report) Format() string {
	if !r.hasData() {
		return "📊 Использование токенов\n\nНет данных."
	}

	var sb strings.Builder
	sb.WriteString("📊 Использование токенов\n")
	for i, w := range r.Windows {
		label := padRight(w.Label+":", 9)
		if i == 0 {
			fmt.Fprintf(&sb, "%s %s  (in %s · cache %s · out %s · %d запр.)\n",
				label, abbrev(w.Totals.TotalTokens), abbrev(w.Totals.PromptTokens),
				abbrev(w.Totals.CachedTokens), abbrev(w.Totals.CompletionTokens), w.Totals.Calls)
		} else {
			fmt.Fprintf(&sb, "%s %s  (%d запр.)\n", label, abbrev(w.Totals.TotalTokens), w.Totals.Calls)
		}
	}
	if r.ContextMax > 0 && r.ContextUsed > 0 {
		fmt.Fprintf(&sb, "Контекст: %s / %s (%d%%)\n",
			groupThousands(r.ContextUsed), groupThousands(r.ContextMax),
			r.ContextUsed*100/r.ContextMax)
	}

	if len(r.ByModel) > 0 {
		sb.WriteString("\nПо модели\n")
		for _, g := range r.ByModel {
			fmt.Fprintf(&sb, "  %s  %s\n", labelOr(g.Label, "—"), abbrev(g.TotalTokens))
		}
	}
	if len(r.ByOperation) > 0 {
		sb.WriteString("\nПо операции\n")
		parts := make([]string, 0, len(r.ByOperation))
		for _, g := range r.ByOperation {
			parts = append(parts, fmt.Sprintf("%s %s", labelOr(g.Label, "—"), abbrev(g.TotalTokens)))
		}
		sb.WriteString("  " + strings.Join(parts, " · ") + "\n")
	}

	if r.Quota.Snapshot != nil {
		writeQuota(&sb, r.Quota)
	}

	return strings.TrimRight(sb.String(), "\n")
}

func writeQuota(sb *strings.Builder, q QuotaResult) {
	s := q.Snapshot
	sb.WriteString("\n📈 Лимиты аккаунта\n")
	prov := "openai-codex"
	if s.PlanType != "" {
		prov += " (" + titleCase(s.PlanType) + ")"
	}
	fmt.Fprintf(sb, "Провайдер: %s\n", prov)
	if s.Primary != nil {
		sb.WriteString(formatWindow("Сессия", s.Primary) + "\n")
	}
	if s.Secondary != nil {
		sb.WriteString(formatWindow("Неделя", s.Secondary) + "\n")
	}
	if foot := sourceFooter(q); foot != "" {
		sb.WriteString(foot + "\n")
	}
}

func formatWindow(name string, w *provider.RateLimitWindow) string {
	label := name + " (" + windowLabel(w.WindowMinutes) + ")"
	out := fmt.Sprintf("%s: %.0f%% осталось (%.0f%% исп.)", label, 100-w.UsedPercent, w.UsedPercent)
	if !w.ResetsAt.IsZero() {
		out += fmt.Sprintf(" • сброс через %s (%s)",
			humanizeDuration(time.Until(w.ResetsAt)), w.ResetsAt.UTC().Format("2006-01-02 15:04 UTC"))
	}
	return out
}

func sourceFooter(q QuotaResult) string {
	switch q.Source {
	case SourceCache:
		if q.Snapshot != nil && !q.Snapshot.CapturedAt.IsZero() {
			return "(данные от " + humanizeDuration(time.Since(q.Snapshot.CapturedAt)) + " назад)"
		}
		return "(кэшированные данные)"
	case SourceWham:
		return "(данные: wham)"
	default:
		return ""
	}
}

// windowLabel renders a rolling window length (minutes) compactly: 300→"5ч",
// 10080→"7д".
func windowLabel(minutes int) string {
	switch {
	case minutes <= 0:
		return "?"
	case minutes%1440 == 0:
		return strconv.Itoa(minutes/1440) + "д"
	case minutes%60 == 0:
		return strconv.Itoa(minutes/60) + "ч"
	default:
		return strconv.Itoa((minutes+30)/60) + "ч"
	}
}

// humanizeDuration renders a non-negative duration as "Nд Hч", "Hч Mм" or "Mм".
func humanizeDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dд %dч", int(d/(24*time.Hour)), int((d%(24*time.Hour))/time.Hour))
	case d >= time.Hour:
		return fmt.Sprintf("%dч %dм", int(d/time.Hour), int((d%time.Hour)/time.Minute))
	default:
		return fmt.Sprintf("%dм", int(d/time.Minute))
	}
}

// abbrev shortens large counts: 980→"980", 4811→"4.8k", 1769970→"1.8M".
func abbrev(n int64) string {
	switch {
	case n < 0:
		return "0"
	case n < 1000:
		return strconv.FormatInt(n, 10)
	case n < 1_000_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1000), ".0") + "k"
	default:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1_000_000), ".0") + "M"
	}
}

// groupThousands inserts thin spaces as thousands separators: 69032→"69 032".
func groupThousands(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(' ')
		}
		b.WriteByte(s[i])
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func padRight(s string, width int) string {
	if n := width - len([]rune(s)); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

func labelOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}
