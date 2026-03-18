package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	windowSize = 100
	ringSize   = 10

	thresholdTelegramSend = 3 * time.Second
	thresholdTelegramEdit = 3 * time.Second
	thresholdLLMCluster   = 10 * time.Second
	thresholdLLMSummarize = 30 * time.Second
	thresholdDB           = 500 * time.Millisecond
	thresholdFailRatio    = 0.20
	thresholdRecentErrors = 5
)

// LatencyStat is a rolling window of the last windowSize duration samples.
type LatencyStat struct {
	mu      sync.Mutex
	samples [windowSize]time.Duration
	count   int // number of filled slots (capped at windowSize)
	pos     int // next write index
}

// Record adds a duration sample to the rolling window.
func (l *LatencyStat) Record(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.samples[l.pos] = d
	l.pos = (l.pos + 1) % windowSize
	if l.count < windowSize {
		l.count++
	}
}

// Start returns a closure that, when called, records time.Since(start).
// Intended for deferred use: defer stat.Start()()
func (l *LatencyStat) Start() func() {
	start := time.Now()
	return func() { l.Record(time.Since(start)) }
}

// LatencySnapshot is a point-in-time copy of LatencyStat statistics.
type LatencySnapshot struct {
	Count int
	Min   time.Duration
	Mean  time.Duration
	P95   time.Duration
	Max   time.Duration
}

// Snapshot returns a safe copy of the current statistics.
func (l *LatencyStat) Snapshot() LatencySnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.count == 0 {
		return LatencySnapshot{}
	}

	// Collect samples in insertion order.
	samples := make([]time.Duration, l.count)
	start := (l.pos - l.count + windowSize) % windowSize
	for i := 0; i < l.count; i++ {
		samples[i] = l.samples[(start+i)%windowSize]
	}

	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var sum time.Duration
	for _, s := range sorted {
		sum += s
	}
	p95idx := int(float64(len(sorted)-1) * 0.95)
	return LatencySnapshot{
		Count: l.count,
		Min:   sorted[0],
		Mean:  sum / time.Duration(len(sorted)),
		P95:   sorted[p95idx],
		Max:   sorted[len(sorted)-1],
	}
}

// rawState returns the internal samples/count/pos for persistence.
func (l *LatencyStat) rawState() LatencyRawState {
	l.mu.Lock()
	defer l.mu.Unlock()
	var s LatencyRawState
	for i, d := range l.samples {
		s.Samples[i] = int64(d)
	}
	s.Count = l.count
	s.Pos = l.pos
	return s
}

// loadRawState restores internal state from a persisted snapshot.
func (l *LatencyStat) loadRawState(s LatencyRawState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, ns := range s.Samples {
		l.samples[i] = time.Duration(ns)
	}
	l.count = s.Count
	l.pos = s.Pos
}

// ErrorEntry is a single error event stored in the ring buffer.
type ErrorEntry struct {
	Ts  time.Time
	Key string
	Msg string
}

// LatencyRawState is the internal state of a LatencyStat, suitable for serialization.
type LatencyRawState struct {
	Samples [windowSize]int64 // nanoseconds
	Count   int
	Pos     int
}

// PersistableSnapshot holds all state that is persisted to DB.
type PersistableSnapshot struct {
	MessagesStored int64
	SummarizeOK    int64
	SummarizeFail  int64
	RateLimitHits  int64
	ErrorCounts    map[string]int64
	RecentErrors   []ErrorEntry
	TelegramSend   LatencyRawState
	TelegramEdit   LatencyRawState
	LLMCluster     LatencyRawState
	LLMSummarize   LatencyRawState
	DBAdd          LatencyRawState
	DBGet          LatencyRawState
}

// Metrics holds all runtime observability data for the bot.
// All methods are safe for concurrent use.
type Metrics struct {
	StartTime time.Time

	TelegramSend LatencyStat
	TelegramEdit LatencyStat
	LLMCluster   LatencyStat
	LLMSummarize LatencyStat
	DBAdd        LatencyStat
	DBGet        LatencyStat

	mu              sync.Mutex
	messagesStored  int64
	summarizeOK     int64
	summarizeFail   int64
	rateLimitHits   int64
	errorCounts     map[string]int64
	errorRing       [ringSize]ErrorEntry
	errorRingPos    int
	errorRingFilled int
}

// New returns a new Metrics instance with the start time set to now.
func New() *Metrics {
	return &Metrics{
		StartTime:   time.Now(),
		errorCounts: make(map[string]int64),
	}
}

func (m *Metrics) IncMessagesStored() {
	m.mu.Lock()
	m.messagesStored++
	m.mu.Unlock()
}

func (m *Metrics) IncSummarizeOK() {
	m.mu.Lock()
	m.summarizeOK++
	m.mu.Unlock()
}

func (m *Metrics) IncSummarizeFail() {
	m.mu.Lock()
	m.summarizeFail++
	m.mu.Unlock()
}

func (m *Metrics) IncRateLimitHit() {
	m.mu.Lock()
	m.rateLimitHits++
	m.mu.Unlock()
}

// RecordError adds an error entry to the ring buffer and increments the per-key counter.
func (m *Metrics) RecordError(key, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errorCounts[key]++
	m.errorRing[m.errorRingPos] = ErrorEntry{Ts: time.Now(), Key: key, Msg: errMsg}
	m.errorRingPos = (m.errorRingPos + 1) % ringSize
	if m.errorRingFilled < ringSize {
		m.errorRingFilled++
	}
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

// Snapshot returns a consistent point-in-time copy of all metrics.
func (m *Metrics) Snapshot() MetricsSnapshot {
	s := MetricsSnapshot{
		Uptime:       time.Since(m.StartTime),
		TelegramSend: m.TelegramSend.Snapshot(),
		TelegramEdit: m.TelegramEdit.Snapshot(),
		LLMCluster:   m.LLMCluster.Snapshot(),
		LLMSummarize: m.LLMSummarize.Snapshot(),
		DBAdd:        m.DBAdd.Snapshot(),
		DBGet:        m.DBGet.Snapshot(),
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	s.MessagesStored = m.messagesStored
	s.SummarizeOK = m.summarizeOK
	s.SummarizeFail = m.summarizeFail
	s.RateLimitHits = m.rateLimitHits
	s.ErrorCounts = make(map[string]int64, len(m.errorCounts))
	for k, v := range m.errorCounts {
		s.ErrorCounts[k] = v
	}

	filled := m.errorRingFilled
	start := (m.errorRingPos - filled + ringSize) % ringSize
	s.RecentErrors = make([]ErrorEntry, filled)
	for i := 0; i < filled; i++ {
		s.RecentErrors[i] = m.errorRing[(start+i)%ringSize]
	}

	return s
}

// PersistableSnapshot returns a consistent copy of all persistable state.
func (m *Metrics) PersistableSnapshot() PersistableSnapshot {
	m.mu.Lock()
	s := PersistableSnapshot{
		MessagesStored: m.messagesStored,
		SummarizeOK:    m.summarizeOK,
		SummarizeFail:  m.summarizeFail,
		RateLimitHits:  m.rateLimitHits,
		ErrorCounts:    make(map[string]int64, len(m.errorCounts)),
	}
	for k, v := range m.errorCounts {
		s.ErrorCounts[k] = v
	}
	filled := m.errorRingFilled
	start := (m.errorRingPos - filled + ringSize) % ringSize
	s.RecentErrors = make([]ErrorEntry, filled)
	for i := 0; i < filled; i++ {
		s.RecentErrors[i] = m.errorRing[(start+i)%ringSize]
	}
	m.mu.Unlock()

	s.TelegramSend = m.TelegramSend.rawState()
	s.TelegramEdit = m.TelegramEdit.rawState()
	s.LLMCluster = m.LLMCluster.rawState()
	s.LLMSummarize = m.LLMSummarize.rawState()
	s.DBAdd = m.DBAdd.rawState()
	s.DBGet = m.DBGet.rawState()
	return s
}

// LoadFromPersistable initializes all persistable fields from a stored snapshot.
// Does not modify StartTime.
func (m *Metrics) LoadFromPersistable(s PersistableSnapshot) {
	m.mu.Lock()
	m.messagesStored = s.MessagesStored
	m.summarizeOK = s.SummarizeOK
	m.summarizeFail = s.SummarizeFail
	m.rateLimitHits = s.RateLimitHits
	m.errorCounts = make(map[string]int64, len(s.ErrorCounts))
	for k, v := range s.ErrorCounts {
		m.errorCounts[k] = v
	}
	// Restore error ring in insertion order.
	n := len(s.RecentErrors)
	if n > ringSize {
		n = ringSize
	}
	for i := 0; i < n; i++ {
		m.errorRing[i] = s.RecentErrors[i]
	}
	m.errorRingPos = n % ringSize
	m.errorRingFilled = n
	m.mu.Unlock()

	m.TelegramSend.loadRawState(s.TelegramSend)
	m.TelegramEdit.loadRawState(s.TelegramEdit)
	m.LLMCluster.loadRawState(s.LLMCluster)
	m.LLMSummarize.loadRawState(s.LLMSummarize)
	m.DBAdd.loadRawState(s.DBAdd)
	m.DBGet.loadRawState(s.DBGet)
}

// FormatStatusReport generates a human-readable status report in Russian.
func (m *Metrics) FormatStatusReport() string {
	return formatSnapshot(m.Snapshot())
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

func formatSnapshot(snap MetricsSnapshot) string {
	var sb strings.Builder

	sb.WriteString("🤖 Статус бота\n\n")

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
		for _, e := range snap.RecentErrors {
			fmt.Fprintf(&sb, "[%s] %s: %s\n", e.Ts.Format("15:04:05"), e.Key, e.Msg)
		}
	}

	return strings.TrimRight(sb.String(), "\n")
}
