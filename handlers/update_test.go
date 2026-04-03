package handlers

import (
	"context"
	"strings"
	"testing"

	"github.com/mymmrac/telego"
	"telegram_summarize_bot/handlers/admin"
)

func TestPrivateCommandStatus_AlertUser(t *testing.T) {
	b, database, tg := newTestBot(t, &fakeSummarizer{})
	defer func() { _ = database.Close() }()

	alertUserID := int64(999)
	b.cfg.AdminUserIDs = []int64{alertUserID}

	// Re-initialize admin with updated config.
	b.admin = admin.New(b, database, b.metrics, b.cfg, &fakeSummarizer{}, b.rateLimiter, tg)

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

	// Non-admin user should get the private chat info, not access denied.
	if len(tg.sentTexts) != 1 {
		t.Fatalf("expected 1 message, got %d", len(tg.sentTexts))
	}
}

func TestOriginUsername(t *testing.T) {
	tests := []struct {
		name   string
		origin telego.MessageOrigin
		want   string
	}{
		{
			name:   "user with username",
			origin: &telego.MessageOriginUser{SenderUser: telego.User{Username: "alice"}},
			want:   "alice",
		},
		{
			name:   "user with name only",
			origin: &telego.MessageOriginUser{SenderUser: telego.User{FirstName: "Bob", LastName: "Smith"}},
			want:   "Bob Smith",
		},
		{
			name:   "user with ID only",
			origin: &telego.MessageOriginUser{SenderUser: telego.User{ID: 42}},
			want:   "User42",
		},
		{
			name:   "hidden user",
			origin: &telego.MessageOriginHiddenUser{SenderUserName: "hidden_bob"},
			want:   "hidden_bob",
		},
		{
			name:   "channel with signature",
			origin: &telego.MessageOriginChannel{AuthorSignature: "Editor", Chat: telego.Chat{Title: "News"}},
			want:   "Editor",
		},
		{
			name:   "channel without signature",
			origin: &telego.MessageOriginChannel{Chat: telego.Chat{Title: "News"}},
			want:   "News",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := originUsername(tt.origin)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandlePrivateChatInfo(t *testing.T) {
	b, database, tg := newTestBot(t, &fakeSummarizer{})
	defer func() { _ = database.Close() }()

	update := telego.Update{
		Message: &telego.Message{
			Text: "hello",
			Chat: telego.Chat{ID: 123, Type: "private"},
			From: &telego.User{ID: 123},
		},
	}

	b.handlePrivateChatInfo(update)

	if len(tg.sentTexts) != 1 {
		t.Fatalf("expected 1 message, got %d", len(tg.sentTexts))
	}
	if !strings.Contains(tg.sentTexts[0], "summarize") {
		t.Fatalf("expected info about summarize, got: %q", tg.sentTexts[0])
	}
}
