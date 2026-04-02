package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestLatencyStatEmpty(t *testing.T) {
	var s LatencyStat
	snap := s.Snapshot()
	if snap.Count != 0 {
		t.Fatalf("expected count=0, got %d", snap.Count)
	}
	if snap.Min != 0 || snap.Mean != 0 || snap.P95 != 0 || snap.Max != 0 {
		t.Fatalf("expected zero values for empty stat, got %+v", snap)
	}
}

func TestLatencyStatRollingWindow(t *testing.T) {
	var s LatencyStat
	// Record more than windowSize samples; oldest should be evicted.
	for i := 0; i < windowSize+50; i++ {
		s.Record(time.Duration(i) * time.Millisecond)
	}
	snap := s.Snapshot()
	if snap.Count > windowSize {
		t.Fatalf("count=%d exceeds windowSize=%d", snap.Count, windowSize)
	}
	// The window should hold the 100 most recent samples: 50..149 ms.
	if snap.Min != 50*time.Millisecond {
		t.Fatalf("expected min=50ms, got %v", snap.Min)
	}
	if snap.Max != 149*time.Millisecond {
		t.Fatalf("expected max=149ms, got %v", snap.Max)
	}
}

func TestLatencyStatP95(t *testing.T) {
	var s LatencyStat
	// Record 20 samples: 1ms..20ms.
	for i := 1; i <= 20; i++ {
		s.Record(time.Duration(i) * time.Millisecond)
	}
	snap := s.Snapshot()
	// p95 index = int(19 * 0.95) = 18, which is the 19th element (20ms sorted: [1,2,...,20]).
	// sorted[18] = 19ms
	expected := 19 * time.Millisecond
	if snap.P95 != expected {
		t.Fatalf("p95=%v, want %v", snap.P95, expected)
	}
}

func TestMetricsCounters(t *testing.T) {
	m := New()
	m.IncMessagesStored()
	m.IncMessagesStored()
	m.IncSummarizeOK()
	m.IncSummarizeFail()
	m.IncSummarizeFail()
	m.IncRateLimitHit()

	snap := m.Snapshot()
	if snap.MessagesStored != 2 {
		t.Errorf("MessagesStored=%d, want 2", snap.MessagesStored)
	}
	if snap.SummarizeOK != 1 {
		t.Errorf("SummarizeOK=%d, want 1", snap.SummarizeOK)
	}
	if snap.SummarizeFail != 2 {
		t.Errorf("SummarizeFail=%d, want 2", snap.SummarizeFail)
	}
	if snap.RateLimitHits != 1 {
		t.Errorf("RateLimitHits=%d, want 1", snap.RateLimitHits)
	}
}

func TestMetricsErrorRing(t *testing.T) {
	m := New()
	// Insert ringSize+5 errors; the ring should evict oldest.
	for i := 0; i < ringSize+5; i++ {
		m.RecordError("key", "msg")
	}
	snap := m.Snapshot()
	if len(snap.RecentErrors) != ringSize {
		t.Fatalf("RecentErrors len=%d, want %d", len(snap.RecentErrors), ringSize)
	}
	if snap.ErrorCounts["key"] != int64(ringSize+5) {
		t.Fatalf("error count=%d, want %d", snap.ErrorCounts["key"], ringSize+5)
	}
}

func TestFormatStatusReportNoIssues(t *testing.T) {
	m := New()
	// Record small latencies well below thresholds.
	for i := 0; i < 10; i++ {
		m.TelegramSend.Record(100 * time.Millisecond)
		m.DBAdd.Record(5 * time.Millisecond)
	}
	m.IncSummarizeOK()

	report := m.FormatStatusReport("test-model")
	if !strings.Contains(report, "✅") {
		t.Errorf("expected ✅ in clean report, got:\n%s", report)
	}
	if strings.Contains(report, "🔴 Проблемы") {
		t.Errorf("unexpected problem block in clean report:\n%s", report)
	}
}

func TestFormatStatusReportWithIssues(t *testing.T) {
	m := New()
	// Inject p95 > thresholdTelegramSend (3s) for TelegramSend.
	for i := 0; i < windowSize; i++ {
		m.TelegramSend.Record(5 * time.Second)
	}

	report := m.FormatStatusReport("test-model")
	if !strings.Contains(report, "⚠️") {
		t.Errorf("expected ⚠️ in problem report, got:\n%s", report)
	}
	if !strings.Contains(report, "🔴 Проблемы") {
		t.Errorf("expected 🔴 Проблемы in report, got:\n%s", report)
	}
}

func TestDetailSnapshotTimestamps(t *testing.T) {
	var s LatencyStat
	s.Record(100 * time.Millisecond)
	s.Record(200 * time.Millisecond)
	s.Record(300 * time.Millisecond)

	d := s.DetailSnapshot()
	if d.Count != 3 {
		t.Fatalf("count=%d, want 3", d.Count)
	}
	if len(d.Samples) != 3 {
		t.Fatalf("samples len=%d, want 3", len(d.Samples))
	}
	// Newest first.
	if d.Samples[0].Duration != 300*time.Millisecond {
		t.Errorf("first sample=%v, want 300ms", d.Samples[0].Duration)
	}
	if d.Samples[2].Duration != 100*time.Millisecond {
		t.Errorf("last sample=%v, want 100ms", d.Samples[2].Duration)
	}
	// All timestamps should be non-zero.
	for i, s := range d.Samples {
		if s.At.IsZero() {
			t.Errorf("sample %d has zero timestamp", i)
		}
	}
}

func TestDetailSnapshotEmpty(t *testing.T) {
	var s LatencyStat
	d := s.DetailSnapshot()
	if d.Count != 0 {
		t.Fatalf("count=%d, want 0", d.Count)
	}
	if len(d.Samples) != 0 {
		t.Fatalf("samples len=%d, want 0", len(d.Samples))
	}
}

func TestFormatLatencyDeepDiveEmpty(t *testing.T) {
	result := FormatLatencyDeepDive("llm_cluster", LatencyDetailSnapshot{})
	if !strings.Contains(result, "Нет данных") {
		t.Errorf("expected 'Нет данных' for empty snapshot, got:\n%s", result)
	}
}

func TestFormatLatencyDeepDiveSections(t *testing.T) {
	var s LatencyStat
	for i := 1; i <= 25; i++ {
		s.Record(time.Duration(i*100) * time.Millisecond)
	}
	d := s.DetailSnapshot()
	result := FormatLatencyDeepDive("llm_cluster", d)

	for _, section := range []string{"📊 Детализация", "📈 Статистика", "📉 Распределение", "🕐 Последние"} {
		if !strings.Contains(result, section) {
			t.Errorf("missing section %q in:\n%s", section, result)
		}
	}
	if !strings.Contains(result, "P50") || !strings.Contains(result, "P95") {
		t.Errorf("missing percentile labels in:\n%s", result)
	}
}

func TestFormatLatencyDeepDiveCapsAt20(t *testing.T) {
	var s LatencyStat
	for i := 0; i < 50; i++ {
		s.Record(time.Duration(i+1) * time.Millisecond)
	}
	d := s.DetailSnapshot()
	result := FormatLatencyDeepDive("db_add", d)
	if !strings.Contains(result, "Последние 20 замеров") {
		t.Errorf("expected cap at 20, got:\n%s", result)
	}
}

func TestLoadRawStateZeroTimestamps(t *testing.T) {
	// Simulate old data without timestamps.
	var s LatencyStat
	raw := LatencyRawState{Count: 2, Pos: 2}
	raw.Samples[0] = int64(100 * time.Millisecond)
	raw.Samples[1] = int64(200 * time.Millisecond)
	// Timestamps are all zero (old format).
	s.loadRawState(raw)

	d := s.DetailSnapshot()
	if d.Count != 2 {
		t.Fatalf("count=%d, want 2", d.Count)
	}
	// No timestamped samples since timestamps were zero.
	if len(d.Samples) != 0 {
		t.Errorf("expected 0 timestamped samples from old data, got %d", len(d.Samples))
	}
	// Stats should still work.
	if d.Min != 100*time.Millisecond {
		t.Errorf("min=%v, want 100ms", d.Min)
	}
}
