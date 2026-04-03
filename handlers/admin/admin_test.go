package admin

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
)

type fakeDeps struct {
	sentTexts     []string
	formattedText []string
	editTexts     []string
	nextID        int64
}

func (f *fakeDeps) SendMessage(chatID int64, text string) int64 {
	f.sentTexts = append(f.sentTexts, text)
	f.nextID++
	return f.nextID
}

func (f *fakeDeps) SendFormatted(chatID int64, text string) {
	f.formattedText = append(f.formattedText, text)
}

func (f *fakeDeps) EditMessage(chatID, messageID int64, text string) error {
	f.editTexts = append(f.editTexts, text)
	return nil
}

func (f *fakeDeps) EditOrSend(chatID, msgID int64, text string) {
	f.sentTexts = append(f.sentTexts, text)
}

func (f *fakeDeps) EditOrSendFormatted(chatID, msgID int64, text string) {
	f.formattedText = append(f.formattedText, text)
}

type fakeTelegram struct {
	sentTexts []string
	nextID    int
}

func (f *fakeTelegram) SendMessage(params *telego.SendMessageParams) (*telego.Message, error) {
	f.sentTexts = append(f.sentTexts, params.Text)
	f.nextID++
	return &telego.Message{MessageID: f.nextID}, nil
}

func (f *fakeTelegram) AnswerCallbackQuery(_ *telego.AnswerCallbackQueryParams) error { return nil }

type fakeSummarizer struct {
	summary string
	err     error
}

func (f *fakeSummarizer) SummarizeURL(_ context.Context, _, _ string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.summary, nil
}

func newTestAdmin(t *testing.T) (*Admin, *db.DB, *fakeDeps) {
	t.Helper()

	database, err := db.New(filepath.Join(t.TempDir(), "bot.db"), metrics.New())
	if err != nil {
		t.Fatalf("db.New error: %v", err)
	}

	deps := &fakeDeps{}
	tg := &fakeTelegram{}
	cfg := &config.Config{
		AdminUserIDs: []int64{999},
		Model:        "test-model",
	}

	a := New(deps, database, metrics.New(), cfg, &fakeSummarizer{}, &fakeRateLimiter{}, tg)
	return a, database, deps
}

type fakeRateLimiter struct {
	allowed bool
}

func (f *fakeRateLimiter) Allow(_ int64) bool                  { return f.allowed }
func (f *fakeRateLimiter) RemainingTime(_ int64) time.Duration { return 30 * time.Second }

func TestHandle_Help(t *testing.T) {
	a, database, deps := newTestAdmin(t)
	defer func() { _ = database.Close() }()

	update := telego.Update{
		Message: &telego.Message{
			Text: "/help",
			Chat: telego.Chat{ID: 999, Type: "private"},
			From: &telego.User{ID: 999},
		},
	}

	handled := a.Handle(context.Background(), update)
	if !handled {
		t.Fatal("expected /help to be handled")
	}
	if len(deps.formattedText) != 1 {
		t.Fatalf("expected 1 formatted message, got %d", len(deps.formattedText))
	}
	if !strings.Contains(deps.formattedText[0], "Команды администратора") {
		t.Fatalf("expected admin help, got: %q", deps.formattedText[0])
	}
}

func TestHandle_Reset(t *testing.T) {
	a, database, deps := newTestAdmin(t)
	defer func() { _ = database.Close() }()

	update := telego.Update{
		Message: &telego.Message{
			Text: "/reset",
			Chat: telego.Chat{ID: 999, Type: "private"},
			From: &telego.User{ID: 999},
		},
	}

	handled := a.Handle(context.Background(), update)
	if !handled {
		t.Fatal("expected /reset to be handled")
	}
	if len(deps.sentTexts) != 1 || deps.sentTexts[0] != "Метрики сброшены." {
		t.Fatalf("expected reset confirmation, got: %v", deps.sentTexts)
	}
}

func TestHandle_Status(t *testing.T) {
	a, database, deps := newTestAdmin(t)
	defer func() { _ = database.Close() }()

	update := telego.Update{
		Message: &telego.Message{
			Text: "/status",
			Chat: telego.Chat{ID: 999, Type: "private"},
			From: &telego.User{ID: 999},
		},
	}

	handled := a.Handle(context.Background(), update)
	if !handled {
		t.Fatal("expected /status to be handled")
	}
	// Status sends via Telegram directly (with inline keyboard).
	_ = deps // deps won't capture the status message since it goes through telegram
}

func TestHandle_GroupsEmpty(t *testing.T) {
	a, database, deps := newTestAdmin(t)
	defer func() { _ = database.Close() }()

	update := telego.Update{
		Message: &telego.Message{
			Text: "/groups",
			Chat: telego.Chat{ID: 999, Type: "private"},
			From: &telego.User{ID: 999},
		},
	}

	handled := a.Handle(context.Background(), update)
	if !handled {
		t.Fatal("expected /groups to be handled")
	}
	if len(deps.sentTexts) != 1 || !strings.Contains(deps.sentTexts[0], "Нет известных групп") {
		t.Fatalf("expected empty groups message, got: %v", deps.sentTexts)
	}
}

func TestHandle_GroupsAddRemove(t *testing.T) {
	a, database, deps := newTestAdmin(t)
	defer func() { _ = database.Close() }()

	ctx := context.Background()

	// Add a known group first so it has a title.
	if err := database.UpsertKnownGroup(ctx, -100123, "Test Group", "testgroup"); err != nil {
		t.Fatalf("UpsertKnownGroup error: %v", err)
	}

	// Add the group.
	update := telego.Update{
		Message: &telego.Message{
			Text: "/groups add -100123",
			Chat: telego.Chat{ID: 999, Type: "private"},
			From: &telego.User{ID: 999},
		},
	}
	a.Handle(ctx, update)

	if len(deps.sentTexts) != 1 || !strings.Contains(deps.sentTexts[0], "добавлена") {
		t.Fatalf("expected add confirmation, got: %v", deps.sentTexts)
	}

	// Verify allowed.
	allowed, err := database.IsGroupAllowed(ctx, -100123)
	if err != nil {
		t.Fatalf("IsGroupAllowed error: %v", err)
	}
	if !allowed {
		t.Fatal("expected group to be allowed")
	}

	// Remove the group.
	deps.sentTexts = nil
	update.Message.Text = "/groups remove -100123"
	a.Handle(ctx, update)

	if len(deps.sentTexts) != 1 || !strings.Contains(deps.sentTexts[0], "удалена") {
		t.Fatalf("expected remove confirmation, got: %v", deps.sentTexts)
	}

	allowed, _ = database.IsGroupAllowed(ctx, -100123)
	if allowed {
		t.Fatal("expected group to be removed")
	}
}

func TestHandle_UnknownCommand(t *testing.T) {
	a, database, deps := newTestAdmin(t)
	defer func() { _ = database.Close() }()

	update := telego.Update{
		Message: &telego.Message{
			Text: "random text",
			Chat: telego.Chat{ID: 999, Type: "private"},
			From: &telego.User{ID: 999},
		},
	}

	handled := a.Handle(context.Background(), update)
	if !handled {
		t.Fatal("expected unknown command to be handled (shows help)")
	}
	// Should show admin help.
	if len(deps.formattedText) != 1 || !strings.Contains(deps.formattedText[0], "Команды администратора") {
		t.Fatalf("expected admin help for unknown command, got: %v", deps.formattedText)
	}
}

func TestHandle_EmptyMessage(t *testing.T) {
	a, database, _ := newTestAdmin(t)
	defer func() { _ = database.Close() }()

	// Nil message should return false.
	handled := a.Handle(context.Background(), telego.Update{})
	if handled {
		t.Fatal("expected nil message to return false")
	}
}

func TestHandle_EmptyText(t *testing.T) {
	a, database, deps := newTestAdmin(t)
	defer func() { _ = database.Close() }()

	update := telego.Update{
		Message: &telego.Message{
			Text: "",
			Chat: telego.Chat{ID: 999, Type: "private"},
			From: &telego.User{ID: 999},
		},
	}

	handled := a.Handle(context.Background(), update)
	if !handled {
		t.Fatal("expected empty text to be handled (shows help)")
	}
	if len(deps.formattedText) != 1 {
		t.Fatalf("expected help message, got: %v", deps.formattedText)
	}
}

func TestHandleCallbackQuery_ValidMetric(t *testing.T) {
	a, database, deps := newTestAdmin(t)
	defer func() { _ = database.Close() }()

	cq := &telego.CallbackQuery{
		ID:   "123",
		From: telego.User{ID: 999},
		Data: "lat:llm_cluster",
		Message: &telego.InaccessibleMessage{
			Chat: telego.Chat{ID: 999},
		},
	}

	a.HandleCallbackQuery(cq)

	if len(deps.sentTexts) != 1 {
		t.Fatalf("expected 1 message, got %d", len(deps.sentTexts))
	}
}

func TestHandleCallbackQuery_InvalidMetric(t *testing.T) {
	a, database, deps := newTestAdmin(t)
	defer func() { _ = database.Close() }()

	cq := &telego.CallbackQuery{
		ID:   "123",
		From: telego.User{ID: 999},
		Data: "lat:invalid_metric",
	}

	a.HandleCallbackQuery(cq)

	if len(deps.sentTexts) != 0 {
		t.Fatalf("expected no messages for invalid metric, got: %v", deps.sentTexts)
	}
}

func TestHandleCallbackQuery_NonAdmin(t *testing.T) {
	a, database, deps := newTestAdmin(t)
	defer func() { _ = database.Close() }()

	cq := &telego.CallbackQuery{
		ID:   "123",
		From: telego.User{ID: 123}, // not an admin
		Data: "lat:llm_cluster",
	}

	a.HandleCallbackQuery(cq)

	if len(deps.sentTexts) != 0 {
		t.Fatalf("expected no messages for non-admin, got: %v", deps.sentTexts)
	}
}

func TestExtractURL(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		entities []telego.MessageEntity
		want     string
	}{
		{
			name:     "url entity",
			text:     "check https://example.com please",
			entities: []telego.MessageEntity{{Type: "url", Offset: 6, Length: 19}},
			want:     "https://example.com",
		},
		{
			name:     "text_link entity",
			text:     "click here",
			entities: []telego.MessageEntity{{Type: "text_link", URL: "https://example.com"}},
			want:     "https://example.com",
		},
		{
			name:     "no url",
			text:     "no links here",
			entities: nil,
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractURL(tt.text, tt.entities)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandle_GroupsAddInvalidID(t *testing.T) {
	a, database, deps := newTestAdmin(t)
	defer func() { _ = database.Close() }()

	update := telego.Update{
		Message: &telego.Message{
			Text: "/groups add notanumber",
			Chat: telego.Chat{ID: 999, Type: "private"},
			From: &telego.User{ID: 999},
		},
	}

	a.Handle(context.Background(), update)

	if len(deps.sentTexts) != 1 || deps.sentTexts[0] != "Неверный ID группы." {
		t.Fatalf("expected invalid ID error, got: %v", deps.sentTexts)
	}
}

func TestHandle_GroupsAddMissingID(t *testing.T) {
	a, database, deps := newTestAdmin(t)
	defer func() { _ = database.Close() }()

	update := telego.Update{
		Message: &telego.Message{
			Text: "/groups add",
			Chat: telego.Chat{ID: 999, Type: "private"},
			From: &telego.User{ID: 999},
		},
	}

	a.Handle(context.Background(), update)

	if len(deps.formattedText) != 1 || !strings.Contains(deps.formattedText[0], "Использование") {
		t.Fatalf("expected usage message, got: %v", deps.formattedText)
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30 секунд"},
		{60 * time.Second, "1 минут"},
		{0, "0 секунд"},
	}

	for _, tt := range tests {
		got := formatDuration(tt.d)
		if got != tt.want {
			t.Fatalf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestSplitMessage(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		limit  int
		chunks int
	}{
		{"empty", "", 100, 0},
		{"within limit", "hello", 100, 1},
		{"over limit", strings.Repeat("a", 50) + "\n" + strings.Repeat("b", 50), 60, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitMessage(tt.text, tt.limit)
			if len(got) != tt.chunks {
				t.Fatalf("got %d chunks, want %d", len(got), tt.chunks)
			}
		})
	}
}
