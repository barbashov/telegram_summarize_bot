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

	report := m.FormatStatusReport()
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

	report := m.FormatStatusReport()
	if !strings.Contains(report, "⚠️") {
		t.Errorf("expected ⚠️ in problem report, got:\n%s", report)
	}
	if !strings.Contains(report, "🔴 Проблемы") {
		t.Errorf("expected 🔴 Проблемы in report, got:\n%s", report)
	}
}
