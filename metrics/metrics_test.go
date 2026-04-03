package metrics

import (
	"context"
	"strings"
	"testing"
	"time"
)

type fakeEventWriter struct {
	events []struct {
		metric     string
		ts         time.Time
		durationNS int64
	}
	errors []struct {
		ts  time.Time
		key string
		msg string
	}
}

func (f *fakeEventWriter) InsertBotEvent(_ context.Context, metric string, ts time.Time, durationNS int64) error {
	f.events = append(f.events, struct {
		metric     string
		ts         time.Time
		durationNS int64
	}{metric, ts, durationNS})
	return nil
}

func (f *fakeEventWriter) InsertErrorLog(_ context.Context, ts time.Time, key, msg string) error {
	f.errors = append(f.errors, struct {
		ts  time.Time
		key string
		msg string
	}{ts, key, msg})
	return nil
}

func TestLatencyStatRecord(t *testing.T) {
	w := &fakeEventWriter{}
	s := NewLatencyStat("test_metric", w)

	s.Record(100 * time.Millisecond)
	s.Record(200 * time.Millisecond)

	if len(w.events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(w.events))
	}
	if w.events[0].metric != "test_metric" {
		t.Errorf("expected metric 'test_metric', got %q", w.events[0].metric)
	}
	if w.events[0].durationNS != int64(100*time.Millisecond) {
		t.Errorf("expected 100ms, got %d ns", w.events[0].durationNS)
	}
}

func TestLatencyStatNilDB(t *testing.T) {
	s := LatencyStat{metric: "test"}
	// Should not panic with nil db.
	s.Record(100 * time.Millisecond)
}

func TestComputeDetailSnapshot(t *testing.T) {
	now := time.Now()
	timestamps := []time.Time{
		now.Add(-3 * time.Second),
		now.Add(-2 * time.Second),
		now.Add(-1 * time.Second),
	}
	durations := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		300 * time.Millisecond,
	}

	d := ComputeDetailSnapshot(timestamps, durations)
	if d.Count != 3 {
		t.Fatalf("count=%d, want 3", d.Count)
	}
	if d.Min != 100*time.Millisecond {
		t.Errorf("min=%v, want 100ms", d.Min)
	}
	if d.Max != 300*time.Millisecond {
		t.Errorf("max=%v, want 300ms", d.Max)
	}
	// Samples should be newest-first.
	if d.Samples[0].Duration != 300*time.Millisecond {
		t.Errorf("first sample=%v, want 300ms", d.Samples[0].Duration)
	}
	if len(d.Durations) != 3 {
		t.Errorf("durations len=%d, want 3", len(d.Durations))
	}
}

func TestComputeDetailSnapshotEmpty(t *testing.T) {
	d := ComputeDetailSnapshot(nil, nil)
	if d.Count != 0 {
		t.Fatalf("count=%d, want 0", d.Count)
	}
}

func TestMetricsUpdateAndReadCache(t *testing.T) {
	m := New()
	detail := ComputeDetailSnapshot(
		[]time.Time{time.Now()},
		[]time.Duration{42 * time.Millisecond},
	)
	m.UpdateCache(
		map[string]LatencyDetailSnapshot{"db_add": detail},
		CachedCounters{MessagesStored: 10, ErrorCounts: make(map[string]int64)},
	)

	got := m.CachedLatency("db_add")
	if got.Count != 1 {
		t.Errorf("cached count=%d, want 1", got.Count)
	}

	snap := m.Snapshot()
	if snap.MessagesStored != 10 {
		t.Errorf("MessagesStored=%d, want 10", snap.MessagesStored)
	}
	if snap.DBAdd.Count != 1 {
		t.Errorf("DBAdd count=%d, want 1", snap.DBAdd.Count)
	}
}

func TestMetricsErrorRing(t *testing.T) {
	m := New()
	w := &fakeEventWriter{}
	m.InitLatencyStats(w)

	for i := 0; i < ringSize+5; i++ {
		m.RecordError("key", "msg")
	}
	errors := m.recentErrors()
	if len(errors) != ringSize {
		t.Fatalf("recent errors len=%d, want %d", len(errors), ringSize)
	}
	// Should also persist to DB.
	if len(w.errors) != ringSize+5 {
		t.Fatalf("persisted errors=%d, want %d", len(w.errors), ringSize+5)
	}
}

func TestMetricsReset(t *testing.T) {
	m := New()
	w := &fakeEventWriter{}
	m.InitLatencyStats(w)

	m.RecordError("test", "err")
	m.UpdateCache(
		map[string]LatencyDetailSnapshot{
			"db_add": ComputeDetailSnapshot(
				[]time.Time{time.Now()},
				[]time.Duration{1 * time.Millisecond},
			),
		},
		CachedCounters{MessagesStored: 5, ErrorCounts: make(map[string]int64)},
	)

	m.Reset()

	snap := m.Snapshot()
	if snap.MessagesStored != 0 {
		t.Errorf("MessagesStored=%d, want 0", snap.MessagesStored)
	}
	if len(snap.RecentErrors) != 0 {
		t.Errorf("recent errors=%d, want 0", len(snap.RecentErrors))
	}
	if snap.DBAdd.Count != 0 {
		t.Errorf("DBAdd count=%d, want 0", snap.DBAdd.Count)
	}
}

func TestFormatStatusReportNoIssues(t *testing.T) {
	m := New()
	report := m.FormatStatusReport("test-model")
	if !strings.Contains(report, "✅") {
		t.Errorf("expected ✅ in clean report, got:\n%s", report)
	}
}

func TestFormatLatencyDeepDiveEmpty(t *testing.T) {
	result := FormatLatencyDeepDive("llm_cluster", LatencyDetailSnapshot{})
	if !strings.Contains(result, "Нет данных") {
		t.Errorf("expected 'Нет данных' for empty snapshot, got:\n%s", result)
	}
}

func TestFormatLatencyDeepDiveSections(t *testing.T) {
	now := time.Now()
	ts := make([]time.Time, 25)
	dur := make([]time.Duration, 25)
	for i := range 25 {
		ts[i] = now.Add(time.Duration(i) * time.Second)
		dur[i] = time.Duration(i+1) * 100 * time.Millisecond
	}
	d := ComputeDetailSnapshot(ts, dur)
	result := FormatLatencyDeepDive("llm_cluster", d)

	for _, section := range []string{"📊 Детализация", "📈 Статистика", "📉 Распределение", "🕐 Последние"} {
		if !strings.Contains(result, section) {
			t.Errorf("missing section %q in:\n%s", section, result)
		}
	}
}

func TestFormatLatencyDeepDiveCapsAt20(t *testing.T) {
	now := time.Now()
	ts := make([]time.Time, 50)
	dur := make([]time.Duration, 50)
	for i := range 50 {
		ts[i] = now.Add(time.Duration(i) * time.Second)
		dur[i] = time.Duration(i+1) * time.Millisecond
	}
	d := ComputeDetailSnapshot(ts, dur)
	result := FormatLatencyDeepDive("db_add", d)
	if !strings.Contains(result, "Последние 20 замеров") {
		t.Errorf("expected cap at 20, got:\n%s", result)
	}
}
