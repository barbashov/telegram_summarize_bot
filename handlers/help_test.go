package handlers

import (
	"strings"
	"testing"

	"github.com/mymmrac/telego"
)

func TestHandleHelp(t *testing.T) {
	b, database, tg := newTestBot(t, &fakeSummarizer{})
	defer func() { _ = database.Close() }()

	update := telego.Update{
		Message: &telego.Message{
			Text: "@testbot help",
			Chat: telego.Chat{ID: 42, Type: "group"},
			From: &telego.User{ID: 7, Username: "alice"},
		},
	}

	b.handleHelp(update)

	if len(tg.sentTexts) != 1 {
		t.Fatalf("expected 1 message, got %d", len(tg.sentTexts))
	}
	if !strings.Contains(tg.sentTexts[0], "summarize") {
		t.Fatalf("expected help text with summarize, got: %q", tg.sentTexts[0])
	}
}
