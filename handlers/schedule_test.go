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

	b.runScheduledSummary(ctx, 42, now)

	if sum.additionalInstructions != "фокусируйся на решениях" {
		t.Fatalf("additionalInstructions = %q, want %q", sum.additionalInstructions, "фокусируйся на решениях")
	}
}
