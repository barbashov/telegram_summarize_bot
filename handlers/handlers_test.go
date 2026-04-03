package handlers

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/mymmrac/telego"
	"telegram_summarize_bot/config"
	"telegram_summarize_bot/db"
	"telegram_summarize_bot/handlers/admin"
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

func (f *fakeTelegram) AnswerCallbackQuery(_ *telego.AnswerCallbackQueryParams) error { return nil }

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
	cfg := &config.Config{
		SummaryHours: 24,
		MaxMessages:  250,
		TopicMax:     5,
	}
	m := metrics.New()
	b := &Bot{
		telegram:     tg,
		db:           database,
		summarizer:   sum,
		rateLimiter:  NewRateLimiter(60),
		cfg:          cfg,
		username:     "testbot",
		metrics:      m,
		userHashSalt: []byte("testsalt"),
	}
	b.admin = admin.New(b, database, m, cfg, sum, b.rateLimiter, tg)

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
