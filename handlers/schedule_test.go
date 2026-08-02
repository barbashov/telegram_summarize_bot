package handlers

import (
	"context"
	"strings"
	"testing"
	"time"

	"telegram_summarize_bot/db"
	"telegram_summarize_bot/summarizer"

	"github.com/mymmrac/telego"
)

func TestHandleScheduleShowStatus(t *testing.T) {
	b, database, tg := newTestBot(t, &fakeSummarizer{})
	defer func() { _ = database.Close() }()

	update := telego.Update{
		Message: &telego.Message{
			Text: "@testbot schedule",
			Chat: telego.Chat{ID: 42, Type: "group"},
			From: &telego.User{ID: 7, Username: "alice"},
		},
	}

	// No schedule set — should show disabled.
	b.handleSchedule(context.Background(), update, nil)
	if len(tg.sentTexts) != 1 || !strings.Contains(tg.sentTexts[0], "отключена") {
		t.Fatalf("expected disabled message, got: %v", tg.sentTexts)
	}
}

func TestHandleScheduleSetTime(t *testing.T) {
	b, database, tg := newTestBot(t, &fakeSummarizer{})
	defer func() { _ = database.Close() }()

	update := telego.Update{
		Message: &telego.Message{
			Text: "@testbot schedule 09:30",
			Chat: telego.Chat{ID: 42, Type: "group"},
			From: &telego.User{ID: 7, Username: "alice"},
		},
	}

	b.handleSchedule(context.Background(), update, []string{"09:30"})

	// Should show enabled with the set time.
	found := false
	for _, text := range tg.sentTexts {
		if strings.Contains(text, "09:30") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected schedule confirmation with 09:30, got: %v", tg.sentTexts)
	}

	// Verify in DB.
	s, err := database.GetGroupSchedule(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetGroupSchedule error: %v", err)
	}
	if s == nil || !s.Enabled || s.Hour != 9 || s.Minute != 30 {
		t.Fatalf("unexpected schedule: %+v", s)
	}
}

func TestHandleScheduleInvalidTime(t *testing.T) {
	b, database, tg := newTestBot(t, &fakeSummarizer{})
	defer func() { _ = database.Close() }()

	update := telego.Update{
		Message: &telego.Message{
			Text: "@testbot schedule 25:00",
			Chat: telego.Chat{ID: 42, Type: "group"},
			From: &telego.User{ID: 7, Username: "alice"},
		},
	}

	b.handleSchedule(context.Background(), update, []string{"25:00"})
	if len(tg.sentTexts) == 0 {
		t.Fatal("expected error message")
	}
	if !strings.Contains(tg.sentTexts[0], "Неверное время") {
		t.Fatalf("expected invalid time error, got: %v", tg.sentTexts)
	}
}

func TestRunScheduledSummaryPassesGroupSummaryInstructions(t *testing.T) {
	sum := &fakeSummarizer{
		summary: &summarizer.StructuredSummary{
			TLDR: "Итог",
		},
	}
	b, database, _ := newTestBot(t, sum)
	defer func() { _ = database.Close() }()

	ctx := context.Background()
	now := time.Now()
	if err := database.AddAllowedGroup(ctx, 42, 7); err != nil {
		t.Fatalf("AddAllowedGroup error: %v", err)
	}
	if err := database.SetGroupSummaryInstructions(ctx, 42, 7, "фокусируйся на решениях"); err != nil {
		t.Fatalf("SetGroupSummaryInstructions error: %v", err)
	}
	if err := database.AddMessage(ctx, &db.Message{
		GroupID:   42,
		UserHash:  "abc123",
		Text:      "решили катить",
		Timestamp: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("AddMessage error: %v", err)
	}

	b.runScheduledSummary(ctx, 42, now, false)

	if sum.additionalInstructions != "фокусируйся на решениях" {
		t.Fatalf("additionalInstructions = %q, want %q", sum.additionalInstructions, "фокусируйся на решениях")
	}
}

// A group revoked via /groups remove must stop receiving scheduled digests:
// schedules live in their own table, so the allowlist is re-checked at run time.
func TestRunScheduledSummarySkipsRevokedGroup(t *testing.T) {
	sum := &fakeSummarizer{summary: &summarizer.StructuredSummary{TLDR: "Итог"}}
	b, database, tg := newTestBot(t, sum)
	defer func() { _ = database.Close() }()

	ctx := context.Background()
	now := time.Now()
	// Group 42 is NOT in allowed_groups.
	if err := database.AddMessage(ctx, &db.Message{
		GroupID:   42,
		UserHash:  "abc123",
		Text:      "привет",
		Timestamp: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("AddMessage error: %v", err)
	}

	b.runScheduledSummary(ctx, 42, now, false)

	if sum.calls != 0 {
		t.Fatal("summarizer must not run for a group outside the allowlist")
	}
	if len(tg.sentTexts) != 0 {
		t.Fatalf("nothing should be sent to a revoked group, got: %v", tg.sentTexts)
	}
}

// "schedule now" must not stamp the daily checkpoint, or the regular scheduled
// run would dedup against it and skip that day's digest.
func TestRunScheduledSummaryManualDoesNotStampCheckpoint(t *testing.T) {
	sum := &fakeSummarizer{summary: &summarizer.StructuredSummary{TLDR: "Итог"}}
	b, database, _ := newTestBot(t, sum)
	defer func() { _ = database.Close() }()

	ctx := context.Background()
	now := time.Now()
	if err := database.AddAllowedGroup(ctx, 42, 7); err != nil {
		t.Fatalf("AddAllowedGroup error: %v", err)
	}
	if err := database.SetGroupSchedule(ctx, &db.GroupSchedule{GroupID: 42, Enabled: true, Hour: 7}); err != nil {
		t.Fatalf("SetGroupSchedule error: %v", err)
	}
	if err := database.AddMessage(ctx, &db.Message{
		GroupID:   42,
		UserHash:  "abc123",
		Text:      "привет",
		Timestamp: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("AddMessage error: %v", err)
	}

	b.runScheduledSummary(ctx, 42, now, true)
	if sum.calls == 0 {
		t.Fatal("manual run should have summarized")
	}

	s, err := database.GetGroupSchedule(ctx, 42)
	if err != nil {
		t.Fatalf("GetGroupSchedule error: %v", err)
	}
	if s.LastDailySummary != nil {
		t.Fatalf("manual run stamped the daily checkpoint: %v", s.LastDailySummary)
	}

	// The scheduled path does stamp it.
	b.runScheduledSummary(ctx, 42, now, false)
	s, err = database.GetGroupSchedule(ctx, 42)
	if err != nil {
		t.Fatalf("GetGroupSchedule error: %v", err)
	}
	if s.LastDailySummary == nil {
		t.Fatal("scheduled run did not stamp the daily checkpoint")
	}
}

func TestScheduleDueCatchesUpAfterMissedTick(t *testing.T) {
	day := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	sched := db.GroupSchedule{Hour: 7, Minute: 0}
	yesterday := day.Add(-10 * time.Hour)

	tests := []struct {
		name string
		s    db.GroupSchedule
		last *time.Time
		now  time.Time
		want bool
	}{
		{"before scheduled time", sched, nil, day.Add(6 * time.Hour), false},
		{"exactly on time", sched, nil, day.Add(7 * time.Hour), true},
		{"missed tick, later same day", sched, nil, day.Add(9*time.Hour + 13*time.Minute), true},
		{"already ran today", sched, timePtr(day.Add(7 * time.Hour)), day.Add(9 * time.Hour), false},
		{"ran yesterday", sched, &yesterday, day.Add(7*time.Hour + 1*time.Minute), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.s
			s.LastDailySummary = tt.last
			if got := scheduleDue(s, tt.now); got != tt.want {
				t.Errorf("scheduleDue(last=%v, now=%v) = %v, want %v", tt.last, tt.now, got, tt.want)
			}
		})
	}
}

func timePtr(t time.Time) *time.Time { return &t }
