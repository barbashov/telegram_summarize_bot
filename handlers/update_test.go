package handlers

import (
	"context"
	"strings"
	"testing"
	"time"

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

func TestForwardOriginHandle(t *testing.T) {
	const groupID int64 = -100
	salt := []byte("test-salt")

	userOrigin := &telego.MessageOriginUser{SenderUser: telego.User{ID: 42, Username: "alice", FirstName: "Bob"}}
	hiddenOrigin := &telego.MessageOriginHiddenUser{SenderUserName: "hidden_bob"}
	chatOrigin := &telego.MessageOriginChat{SenderChat: telego.Chat{ID: -77, Title: "Editorial"}}
	channelOrigin := &telego.MessageOriginChannel{Chat: telego.Chat{ID: -123, Title: "News"}, AuthorSignature: "Editor"}

	// Each branch produces a "kind:hash" handle.
	cases := []struct {
		name       string
		origin     telego.MessageOrigin
		wantPrefix string
	}{
		{"user", userOrigin, "user:"},
		{"hidden user", hiddenOrigin, "hidden:"},
		{"chat", chatOrigin, "chat:"},
		{"channel", channelOrigin, "channel:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := forwardOriginHandle(tc.origin, groupID, salt)
			if !strings.HasPrefix(got, tc.wantPrefix) {
				t.Fatalf("got %q, want prefix %q", got, tc.wantPrefix)
			}
			rest := strings.TrimPrefix(got, tc.wantPrefix)
			if len(rest) != 8 {
				t.Fatalf("hash portion = %q (len %d), want 8 hex chars", rest, len(rest))
			}
			// Must NOT contain the raw username, real name, or title.
			for _, leak := range []string{"alice", "Bob", "hidden_bob", "Editorial", "News", "Editor"} {
				if strings.Contains(got, leak) {
					t.Fatalf("handle %q leaked plaintext %q", got, leak)
				}
			}
		})
	}

	// Stability: same input → same output.
	a := forwardOriginHandle(userOrigin, groupID, salt)
	b := forwardOriginHandle(userOrigin, groupID, salt)
	if a != b {
		t.Fatalf("expected stable handle, got %q vs %q", a, b)
	}

	// Group-scoped: different group → different output.
	other := forwardOriginHandle(userOrigin, groupID+1, salt)
	if a == other {
		t.Fatalf("expected group-scoped variation, got identical %q", a)
	}

	// Empty hidden sender → empty handle (no stable identifier).
	if h := forwardOriginHandle(&telego.MessageOriginHiddenUser{}, groupID, salt); h != "" {
		t.Fatalf("empty hidden sender: got %q, want empty", h)
	}
}

// TestHandleUpdatePhotoOnlyMessagePersists exercises the relaxed empty-text
// guard: a message with no Text but with attached photos must persist a row
// AND attach photo metadata. Today's bot dropped such messages entirely.
func TestHandleUpdatePhotoOnlyMessagePersists(t *testing.T) {
	b, database, _ := newTestBot(t, &fakeSummarizer{})
	defer func() { _ = database.Close() }()

	const groupID int64 = -1001234567890
	if err := database.AddAllowedGroup(context.Background(), groupID, 0); err != nil {
		t.Fatalf("AddAllowedGroup: %v", err)
	}

	update := telego.Update{Message: &telego.Message{
		MessageID: 7,
		Chat:      telego.Chat{ID: groupID, Type: "supergroup", Title: "g"},
		From:      &telego.User{ID: 42},
		Photo: []telego.PhotoSize{
			{FileID: "fid", FileUniqueID: "uniq", Width: 800, Height: 600},
		},
	}}

	b.handleUpdate(context.Background(), update)

	msgs, err := database.GetMessages(context.Background(), groupID, time.Now().Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 stored message, got %d", len(msgs))
	}
	if msgs[0].Text != "" {
		t.Errorf("expected empty text, got %q", msgs[0].Text)
	}

	photos, err := database.GetPhotosForMessages(context.Background(), []int64{msgs[0].ID})
	if err != nil {
		t.Fatalf("GetPhotosForMessages: %v", err)
	}
	if got := photos[msgs[0].ID]; len(got) != 1 || got[0].FileUniqueID != "uniq" {
		t.Errorf("expected photo 'uniq' attached, got %+v", got)
	}
}

// TestHandleUpdateCaptionFallback ensures a photo with a caption stores the
// caption text rather than dropping it on the floor.
func TestHandleUpdateCaptionFallback(t *testing.T) {
	b, database, _ := newTestBot(t, &fakeSummarizer{})
	defer func() { _ = database.Close() }()

	const groupID int64 = -1001111111111
	if err := database.AddAllowedGroup(context.Background(), groupID, 0); err != nil {
		t.Fatalf("AddAllowedGroup: %v", err)
	}

	update := telego.Update{Message: &telego.Message{
		MessageID: 11,
		Chat:      telego.Chat{ID: groupID, Type: "supergroup", Title: "g"},
		From:      &telego.User{ID: 42},
		Caption:   "look at this 🔥",
		Photo:     []telego.PhotoSize{{FileID: "f", FileUniqueID: "u", Width: 10, Height: 10}},
	}}

	b.handleUpdate(context.Background(), update)

	msgs, err := database.GetMessages(context.Background(), groupID, time.Now().Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Text != "look at this 🔥" {
		t.Fatalf("expected caption stored as text, got %+v", msgs)
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

	b.handlePrivateChatInfo(context.Background(), update)

	if len(tg.sentTexts) != 1 {
		t.Fatalf("expected 1 message, got %d", len(tg.sentTexts))
	}
	if !strings.Contains(tg.sentTexts[0], "summarize") {
		t.Fatalf("expected info about summarize, got: %q", tg.sentTexts[0])
	}
}
