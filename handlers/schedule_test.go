package handlers

import (
	"context"
	"strings"
	"testing"

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
