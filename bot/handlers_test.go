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
	"telegram_summarize_bot/metrics"
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

func (f *fakeTelegram) GetChatMember(params *telego.GetChatMemberParams) (telego.ChatMember, error) {
	return &telego.ChatMemberAdministrator{Status: "administrator"}, nil
}

func (f *fakeTelegram) GetChat(_ *telego.GetChatParams) (*telego.ChatFullInfo, error) {
	return &telego.ChatFullInfo{}, nil
}

func (f *fakeTelegram) SetMyCommands(_ *telego.SetMyCommandsParams) error { return nil }

type fakeSummarizer struct {
	summary    *summarizer.StructuredSummary
	err        error
	calls      int
	topicMax   int
	urlSummary string
	urlErr     error
	urlCalls   int
}

func (f *fakeSummarizer) SummarizeByTopics(_ context.Context, _ []db.Message, topicMax int) (*summarizer.StructuredSummary, error) {
	f.calls++
	f.topicMax = topicMax
	if f.err != nil {
		return nil, f.err
	}
	return f.summary, nil
}

func (f *fakeSummarizer) SummarizeURL(_ context.Context, _, _ string) (string, error) {
	f.urlCalls++
	if f.urlErr != nil {
		return "", f.urlErr
	}
	return f.urlSummary, nil
}

func newTestBot(t *testing.T, sum summaryService) (*Bot, *db.DB, *fakeTelegram) {
	t.Helper()

	database, err := db.New(filepath.Join(t.TempDir(), "bot.db"), metrics.New())
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
		username:     "testbot",
		metrics:      metrics.New(),
		userHashSalt: []byte("testsalt"),
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

func TestPrivateCommandStatus_AlertUser(t *testing.T) {
	b, database, tg := newTestBot(t, &fakeSummarizer{})
	defer func() { _ = database.Close() }()

	alertUserID := int64(999)
	b.cfg.AdminUserIDs = []int64{alertUserID}

	update := telego.Update{
		Message: &telego.Message{
			Text: "/status",
			Chat: telego.Chat{ID: alertUserID, Type: "private"},
			From: &telego.User{ID: alertUserID, Username: "admin"},
		},
	}

	b.handlePrivateCommand(context.Background(), update)

	if len(tg.sentTexts) != 1 {
		t.Fatalf("expected 1 message, got %d", len(tg.sentTexts))
	}
	if tg.sentTexts[0] == "" {
		t.Fatal("expected non-empty status report")
	}
	if tg.sentTexts[0] == "Нет доступа." {
		t.Fatal("alert user should receive status report, not access denied")
	}
}

func TestPrivateCommandStatus_NonAlertUser(t *testing.T) {
	b, database, tg := newTestBot(t, &fakeSummarizer{})
	defer func() { _ = database.Close() }()

	b.cfg.AdminUserIDs = []int64{999}

	update := telego.Update{
		Message: &telego.Message{
			Text: "/status",
			Chat: telego.Chat{ID: 123, Type: "private"},
			From: &telego.User{ID: 123, Username: "stranger"},
		},
	}

	b.handlePrivateCommand(context.Background(), update)

	if len(tg.sentTexts) != 1 {
		t.Fatalf("expected 1 message, got %d", len(tg.sentTexts))
	}
	if tg.sentTexts[0] != "Нет доступа." {
		t.Fatalf("expected access denied message, got: %q", tg.sentTexts[0])
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
