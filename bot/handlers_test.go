package bot

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"telegram_summarize_bot/config"
	"telegram_summarize_bot/db"
	"telegram_summarize_bot/summarizer"
)

type fakeTelegram struct {
	sentTexts []string
	editTexts []string
	nextID    int
}

func (f *fakeTelegram) GetMe() (*telego.User, error) {
	return &telego.User{Username: "testbot"}, nil
}

func (f *fakeTelegram) UpdatesViaLongPolling(_ *telego.GetUpdatesParams, _ ...telego.LongPollingOption) (<-chan telego.Update, error) {
	return nil, nil
}

func (f *fakeTelegram) StopLongPolling() {}

func (f *fakeTelegram) SendMessage(params *telego.SendMessageParams) (*telego.Message, error) {
	f.sentTexts = append(f.sentTexts, params.Text)
	f.nextID++
	return &telego.Message{MessageID: f.nextID}, nil
}

func (f *fakeTelegram) EditMessageText(params *telego.EditMessageTextParams) (*telego.Message, error) {
	f.editTexts = append(f.editTexts, params.Text)
	return &telego.Message{MessageID: params.MessageID}, nil
}

type fakeSummarizer struct {
	summary  *summarizer.StructuredSummary
	err      error
	calls    int
	topicMax int
}

func (f *fakeSummarizer) SummarizeByTopics(_ context.Context, _ []db.Message, topicMax int) (*summarizer.StructuredSummary, error) {
	f.calls++
	f.topicMax = topicMax
	if f.err != nil {
		return nil, f.err
	}
	return f.summary, nil
}

func newTestBot(t *testing.T, sum summaryService) (*Bot, *db.DB, *fakeTelegram) {
	t.Helper()

	database, err := db.New(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatalf("db.New error: %v", err)
	}

	tg := &fakeTelegram{}
	b := &Bot{
		telegram:    tg,
		db:          database,
		summarizer:  sum,
		rateLimiter: NewRateLimiter(60),
		cfg: &config.Config{
			SummaryHours: 24,
			MaxMessages:  250,
			TopicMax:     5,
		},
		username: "testbot",
	}

	return b, database, tg
}

func summarizeUpdate() telego.Update {
	return telego.Update{
		Message: &telego.Message{
			Text: "@testbot summarize",
			Chat: telego.Chat{ID: 42, Type: "group"},
			From: &telego.User{ID: 7, Username: "alice"},
		},
	}
}

func TestHandleSummarizeNoMessages(t *testing.T) {
	b, database, tg := newTestBot(t, &fakeSummarizer{})
	defer database.Close()

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
	defer database.Close()

	err := database.AddMessage(context.Background(), &db.Message{
		GroupID:   42,
		UserID:    7,
		Username:  "alice",
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
	if !strings.Contains(tg.editTexts[0], "Обсудили релиз.") {
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

func TestHandleSummarizeDoesNotUpdateLastSummarizeOnFailure(t *testing.T) {
	sum := &fakeSummarizer{err: context.DeadlineExceeded}
	b, database, tg := newTestBot(t, sum)
	defer database.Close()

	err := database.AddMessage(context.Background(), &db.Message{
		GroupID:   42,
		UserID:    7,
		Username:  "alice",
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

func TestSplitTelegramMessageSplitsLongOutput(t *testing.T) {
	text := "📝 *Суммаризация:*\n\n" + strings.Repeat("a", 50) + "\n" + strings.Repeat("b", 50)
	chunks := splitTelegramMessage(text, 60)

	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for _, chunk := range chunks {
		if len(chunk) > 60 {
			t.Fatalf("chunk exceeds limit: %d", len(chunk))
		}
	}
}
