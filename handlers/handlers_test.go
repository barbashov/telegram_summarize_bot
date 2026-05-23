package handlers

import (
	"context"
	"errors"
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

func (f *fakeTelegram) GetMe(_ context.Context) (*telego.User, error) {
	return &telego.User{Username: "testbot"}, nil
}

func (f *fakeTelegram) UpdatesViaLongPolling(_ context.Context, _ *telego.GetUpdatesParams, _ ...telego.LongPollingOption) (<-chan telego.Update, error) {
	return nil, nil
}

func (f *fakeTelegram) SendMessage(_ context.Context, params *telego.SendMessageParams) (*telego.Message, error) {
	f.sentTexts = append(f.sentTexts, params.Text)
	f.nextID++
	return &telego.Message{MessageID: f.nextID}, nil
}

func (f *fakeTelegram) EditMessageText(_ context.Context, params *telego.EditMessageTextParams) (*telego.Message, error) {
	f.editTexts = append(f.editTexts, params.Text)
	return &telego.Message{MessageID: params.MessageID}, nil
}

func (f *fakeTelegram) GetChatMember(_ context.Context, params *telego.GetChatMemberParams) (telego.ChatMember, error) {
	return &telego.ChatMemberAdministrator{Status: "administrator"}, nil
}

func (f *fakeTelegram) GetChat(_ context.Context, _ *telego.GetChatParams) (*telego.ChatFullInfo, error) {
	return &telego.ChatFullInfo{}, nil
}

func (f *fakeTelegram) SetMyCommands(_ context.Context, _ *telego.SetMyCommandsParams) error {
	return nil
}

func (f *fakeTelegram) AnswerCallbackQuery(_ context.Context, _ *telego.AnswerCallbackQueryParams) error {
	return nil
}

func (f *fakeTelegram) GetFile(_ context.Context, _ *telego.GetFileParams) (*telego.File, error) {
	return &telego.File{FilePath: "test/file.jpg"}, nil
}

type fakeSummarizer struct {
	summary                *summarizer.StructuredSummary
	err                    error
	calls                  int
	topicMax               int
	additionalInstructions string
	urlSummary             string
	urlErr                 error
	urlCalls               int
	urlInstr               string
	textSummary            string
	textErr                error
	textCalls              int
	textInput              string
	textInstr              string
	imageDesc              string
	imageErr               error
	imageCalls             int
}

func (f *fakeSummarizer) SummarizeByTopics(_ context.Context, _ []db.Message, topicMax int, additionalInstructions string) (*summarizer.StructuredSummary, error) {
	f.calls++
	f.topicMax = topicMax
	f.additionalInstructions = additionalInstructions
	if f.err != nil {
		return nil, f.err
	}
	return f.summary, nil
}

func (f *fakeSummarizer) SummarizeURL(_ context.Context, _, _, instructions string) (string, error) {
	f.urlCalls++
	f.urlInstr = instructions
	if f.urlErr != nil {
		return "", f.urlErr
	}
	return f.urlSummary, nil
}

func (f *fakeSummarizer) SummarizeText(_ context.Context, content, instructions string) (string, error) {
	f.textCalls++
	f.textInput = content
	f.textInstr = instructions
	if f.textErr != nil {
		return "", f.textErr
	}
	return f.textSummary, nil
}

func (f *fakeSummarizer) DescribeImage(_ context.Context, _ db.PhotoRecord) (string, error) {
	f.imageCalls++
	if f.imageErr != nil {
		return "", f.imageErr
	}
	return f.imageDesc, nil
}

func newTestBot(t *testing.T, sum summaryService) (*Bot, *db.DB, *fakeTelegram) {
	t.Helper()

	database, err := db.New(filepath.Join(t.TempDir(), "bot.db"), metrics.New())
	if err != nil {
		t.Fatalf("db.New error: %v", err)
	}

	tg := &fakeTelegram{}
	cfg := &config.Config{
		SummaryHours:  24,
		MaxMessages:   250,
		TopicMax:      5,
		ReplyMinChars: 1000,
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
		// Default to a stub so tests never hit the network; link tests override.
		fetchURL: func(_ context.Context, _ string, _ int) (string, error) {
			return "", errors.New("fetchURL not stubbed in this test")
		},
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
