package metrics

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"telegram_summarize_bot/logger"
)

const (
	ringSize = 10

	thresholdTelegramSend = 3 * time.Second
	thresholdTelegramEdit = 3 * time.Second
	thresholdLLMCluster   = 10 * time.Second
	thresholdLLMSummarize = 30 * time.Second
	thresholdDB           = 500 * time.Millisecond
	thresholdFailRatio    = 0.20
	thresholdRecentErrors = 5

	deepDiveMaxSamples = 20 // max recent samples shown in deep-dive
)

// EventWriter persists a single metric event to the database.
type EventWriter interface {
	InsertBotEvent(ctx context.Context, metric string, ts time.Time, durationNS int64) error
	InsertErrorLog(ctx context.Context, ts time.Time, key, msg string) error
}

// LatencyStat records latency samples directly to the database.
type LatencyStat struct {
	metric string
	db     EventWriter
}

// NewLatencyStat creates a LatencyStat that writes to the given DB.
func NewLatencyStat(metric string, db EventWriter) LatencyStat {
	return LatencyStat{metric: metric, db: db}
}

// Record inserts a duration sample into the database.
func (l *LatencyStat) Record(d time.Duration) {
	if l.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := l.db.InsertBotEvent(ctx, l.metric, time.Now(), int64(d)); err != nil {
		logger.Warn().Err(err).Str("metric", l.metric).Msg("failed to record latency event")
	}
}

// Start returns a closure that, when called, records time.Since(start).
// Intended for deferred use: defer stat.Start()()
func (l *LatencyStat) Start() func() {
	start := time.Now()
	return func() { l.Record(time.Since(start)) }
}

// LatencySnapshot is a point-in-time copy of latency statistics.
type LatencySnapshot struct {
	Count int
	Min   time.Duration
	Mean  time.Duration
	P95   time.Duration
	Max   time.Duration
}

// TimedSample is a single latency measurement with its wall-clock time.
type TimedSample struct {
	At       time.Time
	Duration time.Duration
}

// LatencyDetailSnapshot provides raw data points and statistics for deep-dive views.
type LatencyDetailSnapshot struct {
	Samples   []TimedSample   // newest-first
	Durations []time.Duration // all durations (for histogram)
	LatencySnapshot
	P50 time.Duration
}

// ErrorEntry is a single error event stored in the ring buffer.
type ErrorEntry struct {
	Ts  time.Time
	Key string
	Msg string
}

// CachedCounters holds counter values derived from DB during cache refresh.
type CachedCounters struct {
	MessagesStored int64
	SummarizeOK    int64
	SummarizeFail  int64
	RateLimitHits  int64
	ErrorCounts    map[string]int64
}

// MetricsSnapshot is a point-in-time copy of all metrics, safe to read without holding locks.
type MetricsSnapshot struct {
	Uptime         time.Duration
	TelegramSend   LatencySnapshot
	TelegramEdit   LatencySnapshot
	LLMCluster     LatencySnapshot
	LLMSummarize   LatencySnapshot
	DBAdd          LatencySnapshot
	DBGet          LatencySnapshot
	MessagesStored int64
	SummarizeOK    int64
	SummarizeFail  int64
	RateLimitHits  int64
	ErrorCounts    map[string]int64
	RecentErrors   []ErrorEntry
}

// Metrics holds all runtime observability data for the bot.
// Latency stats are written directly to DB; reads come from a periodically refreshed cache.
type Metrics struct {
	StartTime time.Time

	TelegramSend LatencyStat
	TelegramEdit LatencyStat
	LLMCluster   LatencyStat
	LLMSummarize LatencyStat
	DBAdd        LatencyStat
	DBGet        LatencyStat
	RateLimit    LatencyStat

	db EventWriter // set by InitLatencyStats

	cacheMu      sync.RWMutex
	latencyCache map[string]LatencyDetailSnapshot
	counters     CachedCounters

	mu              sync.Mutex
	errorRing       [ringSize]ErrorEntry
	errorRingPos    int
	errorRingFilled int
}

// New returns a new Metrics instance with the start time set to now.
func New() *Metrics {
	return &Metrics{
		StartTime:    time.Now(),
		latencyCache: make(map[string]LatencyDetailSnapshot),
		counters:     CachedCounters{ErrorCounts: make(map[string]int64)},
	}
}

// InitLatencyStats initializes all LatencyStat fields with a DB writer.
// Must be called after the DB is available.
func (m *Metrics) InitLatencyStats(db EventWriter) {
	m.db = db
	m.TelegramSend = NewLatencyStat("telegram_send", db)
	m.TelegramEdit = NewLatencyStat("telegram_edit", db)
	m.LLMCluster = NewLatencyStat("llm_cluster", db)
	m.LLMSummarize = NewLatencyStat("llm_summarize", db)
	m.DBAdd = NewLatencyStat("db_add", db)
	m.DBGet = NewLatencyStat("db_get", db)
	m.RateLimit = NewLatencyStat("rate_limit", db)
}

// UpdateCache replaces the latency cache and counter values.
func (m *Metrics) UpdateCache(latency map[string]LatencyDetailSnapshot, counters CachedCounters) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	m.latencyCache = latency
	m.counters = counters
}

// CachedLatency returns the cached detail snapshot for a metric.
func (m *Metrics) CachedLatency(metric string) LatencyDetailSnapshot {
	m.cacheMu.RLock()
	defer m.cacheMu.RUnlock()
	return m.latencyCache[metric]
}

func (m *Metrics) cachedSnapshot(metric string) LatencySnapshot {
	m.cacheMu.RLock()
	defer m.cacheMu.RUnlock()
	return m.latencyCache[metric].LatencySnapshot
}

func (m *Metrics) cachedCounters() CachedCounters {
	m.cacheMu.RLock()
	defer m.cacheMu.RUnlock()
	return m.counters
}

// RecordError adds an error entry to the ring buffer and persists it to DB.
func (m *Metrics) RecordError(key, errMsg string) {
	now := time.Now()
	m.mu.Lock()
	m.errorRing[m.errorRingPos] = ErrorEntry{Ts: now, Key: key, Msg: errMsg}
	m.errorRingPos = (m.errorRingPos + 1) % ringSize
	if m.errorRingFilled < ringSize {
		m.errorRingFilled++
	}
	m.mu.Unlock()

	if m.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = m.db.InsertErrorLog(ctx, now, key, errMsg)
	}
}

func (m *Metrics) recentErrors() []ErrorEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	filled := m.errorRingFilled
	start := (m.errorRingPos - filled + ringSize) % ringSize
	result := make([]ErrorEntry, filled)
	for i := 0; i < filled; i++ {
		result[i] = m.errorRing[(start+i)%ringSize]
	}
	return result
}

// Reset clears the in-memory error ring and latency cache.
func (m *Metrics) Reset() {
	m.mu.Lock()
	m.errorRing = [ringSize]ErrorEntry{}
	m.errorRingPos = 0
	m.errorRingFilled = 0
	m.mu.Unlock()

	m.cacheMu.Lock()
	m.latencyCache = make(map[string]LatencyDetailSnapshot)
	m.counters = CachedCounters{ErrorCounts: make(map[string]int64)}
	m.cacheMu.Unlock()
}

// Snapshot returns a consistent point-in-time copy of all metrics.
func (m *Metrics) Snapshot() MetricsSnapshot {
	c := m.cachedCounters()
	return MetricsSnapshot{
		Uptime:         time.Since(m.StartTime),
		TelegramSend:   m.cachedSnapshot("telegram_send"),
		TelegramEdit:   m.cachedSnapshot("telegram_edit"),
		LLMCluster:     m.cachedSnapshot("llm_cluster"),
		LLMSummarize:   m.cachedSnapshot("llm_summarize"),
		DBAdd:          m.cachedSnapshot("db_add"),
		DBGet:          m.cachedSnapshot("db_get"),
		MessagesStored: c.MessagesStored,
		SummarizeOK:    c.SummarizeOK,
		SummarizeFail:  c.SummarizeFail,
		RateLimitHits:  c.RateLimitHits,
		ErrorCounts:    c.ErrorCounts,
		RecentErrors:   m.recentErrors(),
	}
}

// FormatStatusReport generates a human-readable status report in Russian.
func (m *Metrics) FormatStatusReport(model string) string {
	return formatSnapshot(m.Snapshot(), model)
}

// ComputeDetailSnapshot computes a LatencyDetailSnapshot from raw event data.
func ComputeDetailSnapshot(timestamps []time.Time, durations []time.Duration) LatencyDetailSnapshot {
	ds := LatencyDetailSnapshot{}
	if len(durations) == 0 {
		return ds
	}

	// Build timed samples (newest-first for display).
	ds.Durations = durations
	for i := len(durations) - 1; i >= 0; i-- {
		ds.Samples = append(ds.Samples, TimedSample{
			At:       timestamps[i],
			Duration: durations[i],
		})
	}

	sorted := make([]time.Duration, len(durations))
	copy(sorted, durations)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var sum time.Duration
	for _, s := range sorted {
		sum += s
	}

	n := len(sorted)
	ds.Count = n
	ds.Min = sorted[0]
	ds.Mean = sum / time.Duration(n)
	ds.P50 = sorted[int(float64(n-1)*0.50)]
	ds.P95 = sorted[int(float64(n-1)*0.95)]
	ds.Max = sorted[n-1]

	return ds
}

func trafficLight(p95, threshold time.Duration) string {
	if threshold == 0 {
		return "🟢"
	}
	ratio := float64(p95) / float64(threshold)
	switch {
	case ratio > 1.0:
		return "🔴"
	case ratio >= 0.7:
		return "🟡"
	default:
		return "🟢"
	}
}

func formatDur(d time.Duration) string {
	if d == 0 {
		return "0"
	}
	if d < time.Millisecond {
		return fmt.Sprintf("%dмкс", d.Microseconds())
	}
	if d < time.Second {
		return fmt.Sprintf("%dмс", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fс", d.Seconds())
}

func latencyRow(emoji, name string, snap LatencySnapshot) string {
	if snap.Count == 0 {
		return fmt.Sprintf("%s %-20s нет данных", emoji, name+":")
	}
	return fmt.Sprintf("%s %-20s count=%-5d mean=%-8s p95=%-8s max=%s",
		emoji, name+":", snap.Count, formatDur(snap.Mean), formatDur(snap.P95), formatDur(snap.Max))
}

func formatSnapshot(snap MetricsSnapshot, model string) string {
	var sb strings.Builder

	sb.WriteString("🤖 Статус бота\n\n")
	fmt.Fprintf(&sb, "🧠 Модель: %s\n", model)

	h := int(snap.Uptime.Hours())
	m := int(snap.Uptime.Minutes()) % 60
	fmt.Fprintf(&sb, "⏱ Аптайм: %dч %dм\n\n", h, m)

	// Issue detection.
	var issues []string
	if snap.TelegramSend.Count > 0 && snap.TelegramSend.P95 > thresholdTelegramSend {
		issues = append(issues, fmt.Sprintf("  ⚠️ Высокая задержка telegram_send (p95=%s > %s)",
			formatDur(snap.TelegramSend.P95), formatDur(thresholdTelegramSend)))
	}
	if snap.TelegramEdit.Count > 0 && snap.TelegramEdit.P95 > thresholdTelegramEdit {
		issues = append(issues, fmt.Sprintf("  ⚠️ Высокая задержка telegram_edit (p95=%s > %s)",
			formatDur(snap.TelegramEdit.P95), formatDur(thresholdTelegramEdit)))
	}
	if snap.LLMCluster.Count > 0 && snap.LLMCluster.P95 > thresholdLLMCluster {
		issues = append(issues, fmt.Sprintf("  ⚠️ Высокая задержка llm_cluster (p95=%s > %s)",
			formatDur(snap.LLMCluster.P95), formatDur(thresholdLLMCluster)))
	}
	if snap.LLMSummarize.Count > 0 && snap.LLMSummarize.P95 > thresholdLLMSummarize {
		issues = append(issues, fmt.Sprintf("  ⚠️ Высокая задержка llm_summarize (p95=%s > %s)",
			formatDur(snap.LLMSummarize.P95), formatDur(thresholdLLMSummarize)))
	}
	if snap.DBAdd.Count > 0 && snap.DBAdd.P95 > thresholdDB {
		issues = append(issues, fmt.Sprintf("  ⚠️ Высокая задержка db_add (p95=%s > %s)",
			formatDur(snap.DBAdd.P95), formatDur(thresholdDB)))
	}
	if snap.DBGet.Count > 0 && snap.DBGet.P95 > thresholdDB {
		issues = append(issues, fmt.Sprintf("  ⚠️ Высокая задержка db_get (p95=%s > %s)",
			formatDur(snap.DBGet.P95), formatDur(thresholdDB)))
	}
	total := snap.SummarizeOK + snap.SummarizeFail
	if total > 0 && float64(snap.SummarizeFail)/float64(total) > thresholdFailRatio {
		issues = append(issues, fmt.Sprintf("  ⚠️ Высокий процент ошибок суммаризации (%d/%d)",
			snap.SummarizeFail, total))
	}
	if len(snap.RecentErrors) >= thresholdRecentErrors {
		issues = append(issues, fmt.Sprintf("  ⚠️ Много недавних ошибок (%d записей)", len(snap.RecentErrors)))
	}

	if len(issues) > 0 {
		sb.WriteString("🔴 Проблемы обнаружены:\n")
		for _, issue := range issues {
			sb.WriteString(issue + "\n")
		}
	} else {
		sb.WriteString("✅ Проблем не обнаружено\n")
	}

	// Latency table.
	sb.WriteString("\n📡 Задержки:\n")
	sb.WriteString(latencyRow(trafficLight(snap.TelegramSend.P95, thresholdTelegramSend), "telegram_send", snap.TelegramSend) + "\n")
	sb.WriteString(latencyRow(trafficLight(snap.TelegramEdit.P95, thresholdTelegramEdit), "telegram_edit", snap.TelegramEdit) + "\n")
	sb.WriteString(latencyRow(trafficLight(snap.LLMCluster.P95, thresholdLLMCluster), "llm_cluster", snap.LLMCluster) + "\n")
	sb.WriteString(latencyRow(trafficLight(snap.LLMSummarize.P95, thresholdLLMSummarize), "llm_summarize", snap.LLMSummarize) + "\n")
	sb.WriteString(latencyRow(trafficLight(snap.DBAdd.P95, thresholdDB), "db_add", snap.DBAdd) + "\n")
	sb.WriteString(latencyRow(trafficLight(snap.DBGet.P95, thresholdDB), "db_get", snap.DBGet) + "\n")

	// Counters.
	sb.WriteString("\n📊 Счётчики:\n")
	fmt.Fprintf(&sb, "Сообщений сохранено:      %d\n", snap.MessagesStored)
	if snap.SummarizeFail == 0 {
		fmt.Fprintf(&sb, "Суммаризаций ОК:          %d ✅\n", snap.SummarizeOK)
	} else {
		fmt.Fprintf(&sb, "Суммаризаций ОК:          %d\n", snap.SummarizeOK)
	}
	if snap.SummarizeFail > 0 {
		fmt.Fprintf(&sb, "Суммаризаций ошибок:      %d ❌\n", snap.SummarizeFail)
	} else {
		fmt.Fprintf(&sb, "Суммаризаций ошибок:      %d\n", snap.SummarizeFail)
	}
	fmt.Fprintf(&sb, "Срабатываний рейт-лимита: %d\n", snap.RateLimitHits)

	if len(snap.ErrorCounts) > 0 {
		sb.WriteString("\nОшибки по типу:\n")
		keys := make([]string, 0, len(snap.ErrorCounts))
		for k := range snap.ErrorCounts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&sb, "  %-15s %d\n", k+":", snap.ErrorCounts[k])
		}
	}

	if len(snap.RecentErrors) > 0 {
		sb.WriteString("\n🚨 Последние ошибки:\n")
		for i := len(snap.RecentErrors) - 1; i >= 0; i-- {
			e := snap.RecentErrors[i]
			fmt.Fprintf(&sb, "[%s] %s: %s\n", e.Ts.Format("2006-01-02 15:04:05 MST"), e.Key, e.Msg)
		}
	}

	return strings.TrimRight(sb.String(), "\n")
}

// FormatLatencyDeepDive formats a detailed latency report for a single metric.
func FormatLatencyDeepDive(name string, d LatencyDetailSnapshot) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "📊 Детализация: %s\n", name)

	if d.Count == 0 {
		sb.WriteString("\nНет данных")
		return sb.String()
	}

	fmt.Fprintf(&sb, "\n📈 Статистика (%d замеров):\n", d.Count)
	fmt.Fprintf(&sb, "  Min:  %s\n", formatDur(d.Min))
	fmt.Fprintf(&sb, "  Mean: %s\n", formatDur(d.Mean))
	fmt.Fprintf(&sb, "  P50:  %s\n", formatDur(d.P50))
	fmt.Fprintf(&sb, "  P95:  %s\n", formatDur(d.P95))
	fmt.Fprintf(&sb, "  Max:  %s\n", formatDur(d.Max))

	// Distribution histogram with fixed buckets.
	type bucket struct {
		label string
		lo    time.Duration
		hi    time.Duration // exclusive; 0 means +∞
	}
	buckets := []bucket{
		{"<100мс", 0, 100 * time.Millisecond},
		{"100-500мс", 100 * time.Millisecond, 500 * time.Millisecond},
		{"0.5-1с", 500 * time.Millisecond, time.Second},
		{"1-5с", time.Second, 5 * time.Second},
		{"5-30с", 5 * time.Second, 30 * time.Second},
		{">30с", 30 * time.Second, 0},
	}

	counts := make([]int, len(buckets))
	for _, dur := range d.Durations {
		for bi, b := range buckets {
			if dur >= b.lo && (b.hi == 0 || dur < b.hi) {
				counts[bi]++
				break
			}
		}
	}

	maxCount := 0
	for _, c := range counts {
		if c > maxCount {
			maxCount = c
		}
	}

	sb.WriteString("\n📉 Распределение:\n")
	const maxBar = 15
	for i, b := range buckets {
		c := counts[i]
		barLen := 0
		if maxCount > 0 {
			barLen = c * maxBar / maxCount
		}
		if c > 0 && barLen == 0 {
			barLen = 1
		}
		bar := strings.Repeat("█", barLen)
		fmt.Fprintf(&sb, "  %-10s %s %d\n", b.label, bar, c)
	}

	// Recent timestamped samples.
	if len(d.Samples) > 0 {
		show := d.Samples
		if len(show) > deepDiveMaxSamples {
			show = show[:deepDiveMaxSamples]
		}
		fmt.Fprintf(&sb, "\n🕐 Последние %d замеров:\n", len(show))
		for _, s := range show {
			fmt.Fprintf(&sb, "  %s  %s\n", s.At.Format("15:04:05"), formatDur(s.Duration))
		}
	}

	return strings.TrimRight(sb.String(), "\n")
}
