package handlers

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"telegram_summarize_bot/db"
	"telegram_summarize_bot/summarizer"
)

func TestHandleSummarizeNoMessages(t *testing.T) {
	b, database, tg := newTestBot(t, &fakeSummarizer{})
	defer func() { _ = database.Close() }()

	b.handleSummarize(context.Background(), summarizeUpdate(), nil)

	if len(tg.sentTexts) != 1 {
		t.Fatalf("sent message count = %d, want 1", len(tg.sentTexts))
	}
	if tg.sentTexts[0] != "Нет сообщений за последние 24 часа." {
		t.Fatalf("unexpected message: %q", tg.sentTexts[0])
	}
}

func TestHandleSummarizeUpdatesLastSummarizeOnSuccess(t *testing.T) {
	sum := &fakeSummarizer{
		summary: &summarizer.StructuredSummary{
			TLDR: "Обсудили релиз.",
			Topics: []summarizer.TopicSummary{
				{Title: "Релиз", Summary: "Решили катить вечером."},
			},
		},
	}
	b, database, tg := newTestBot(t, sum)
	defer func() { _ = database.Close() }()

	err := database.AddMessage(context.Background(), &db.Message{
		GroupID:   42,
		UserHash:  "a3f2b1c4",
		Text:      "Надо катить сегодня",
		Timestamp: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("AddMessage error: %v", err)
	}

	b.handleSummarize(context.Background(), summarizeUpdate(), nil)

	if sum.calls != 1 {
		t.Fatalf("summarizer calls = %d, want 1", sum.calls)
	}
	if sum.topicMax != 5 {
		t.Fatalf("topicMax = %d, want 5", sum.topicMax)
	}
	if len(tg.editTexts) != 1 {
		t.Fatalf("edit count = %d, want 1", len(tg.editTexts))
	}
	if !strings.Contains(tg.editTexts[0], "Обсудили релиз\\.") {
		t.Fatalf("unexpected edited summary: %q", tg.editTexts[0])
	}

	last, err := database.GetLastSummarizeTime(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetLastSummarizeTime error: %v", err)
	}
	if last == nil {
		t.Fatal("expected last summarize time to be set")
	}
}

func TestHandleSummarizePassesGroupSummaryInstructions(t *testing.T) {
	sum := &fakeSummarizer{
		summary: &summarizer.StructuredSummary{
			TLDR: "Итог",
		},
	}
	b, database, _ := newTestBot(t, sum)
	defer func() { _ = database.Close() }()

	ctx := context.Background()
	if err := database.SetGroupSummaryInstructions(ctx, 42, 7, "выделяй риски"); err != nil {
		t.Fatalf("SetGroupSummaryInstructions error: %v", err)
	}
	if err := database.AddMessage(ctx, &db.Message{
		GroupID:   42,
		UserHash:  "a3f2b1c4",
		Text:      "Надо катить сегодня",
		Timestamp: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("AddMessage error: %v", err)
	}

	b.handleSummarize(ctx, summarizeUpdate(), nil)

	if sum.additionalInstructions != "выделяй риски" {
		t.Fatalf("additionalInstructions = %q, want %q", sum.additionalInstructions, "выделяй риски")
	}
}

func TestHandleSummarizeDoesNotUpdateLastSummarizeOnFailure(t *testing.T) {
	sum := &fakeSummarizer{err: context.DeadlineExceeded}
	b, database, tg := newTestBot(t, sum)
	defer func() { _ = database.Close() }()

	err := database.AddMessage(context.Background(), &db.Message{
		GroupID:   42,
		UserHash:  "a3f2b1c4",
		Text:      "Надо катить сегодня",
		Timestamp: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("AddMessage error: %v", err)
	}

	b.handleSummarize(context.Background(), summarizeUpdate(), nil)

	last, err := database.GetLastSummarizeTime(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetLastSummarizeTime error: %v", err)
	}
	if last != nil {
		t.Fatal("expected last summarize time to remain nil")
	}
	if len(tg.editTexts) != 1 || tg.editTexts[0] != "Ошибка суммаризации. Попробуйте позже." {
		t.Fatalf("unexpected failure message: %#v", tg.editTexts)
	}
}

func TestHandleSummarizeRateLimited(t *testing.T) {
	sum := &fakeSummarizer{
		summary: &summarizer.StructuredSummary{
			TLDR: "test",
			Topics: []summarizer.TopicSummary{
				{Title: "T", Summary: "S"},
			},
		},
	}
	b, database, tg := newTestBot(t, sum)
	defer func() { _ = database.Close() }()

	err := database.AddMessage(context.Background(), &db.Message{
		GroupID:   42,
		UserHash:  "abc123",
		Text:      "test message",
		Timestamp: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("AddMessage error: %v", err)
	}

	// First call should succeed.
	b.handleSummarize(context.Background(), summarizeUpdate(), nil)
	if sum.calls != 1 {
		t.Fatalf("expected 1 summarizer call, got %d", sum.calls)
	}

	// Add another message for the second attempt.
	err = database.AddMessage(context.Background(), &db.Message{
		GroupID:     42,
		UserHash:    "abc123",
		Text:        "another message",
		Timestamp:   time.Now(),
		TgMessageID: 999,
	})
	if err != nil {
		t.Fatalf("AddMessage error: %v", err)
	}

	// Second call should be rate limited.
	tg.sentTexts = nil
	b.handleSummarize(context.Background(), summarizeUpdate(), nil)
	if sum.calls != 1 {
		t.Fatalf("expected summarizer not called again, got %d calls", sum.calls)
	}
	if len(tg.sentTexts) != 1 || !strings.Contains(tg.sentTexts[0], "Подождите") {
		t.Fatalf("expected rate limit message, got: %v", tg.sentTexts)
	}
}

func TestHandleSummarizeCustomHours(t *testing.T) {
	b, database, tg := newTestBot(t, &fakeSummarizer{})
	defer func() { _ = database.Close() }()

	// Try invalid hours.
	b.handleSummarize(context.Background(), summarizeUpdate(), []string{"-5"})
	if len(tg.sentTexts) != 1 || !strings.Contains(tg.sentTexts[0], "Неверный формат") {
		t.Fatalf("expected format error, got: %v", tg.sentTexts)
	}

	// Try hours exceeding max.
	tg.sentTexts = nil
	b.handleSummarize(context.Background(), summarizeUpdate(), []string{"48"})
	if len(tg.sentTexts) != 1 || !strings.Contains(tg.sentTexts[0], fmt.Sprintf("Максимальный период суммаризации — %d", b.cfg.SummaryHours)) {
		t.Fatalf("expected max hours error, got: %v", tg.sentTexts)
	}
}

// Message splitting is now handled by telegramify.Split; see the summarizer and
// integration tests for end-to-end rendering coverage.
