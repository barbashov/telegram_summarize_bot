package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/mymmrac/telego"
)

// TestHandleEditedMessageOverwritesText verifies that an edited_message update
// overwrites the stored text in place (same row, no duplicate).
func TestHandleEditedMessageOverwritesText(t *testing.T) {
	b, database, _ := newTestBot(t, &fakeSummarizer{})
	defer func() { _ = database.Close() }()

	const groupID int64 = -1001234567890
	if err := database.AddAllowedGroup(context.Background(), groupID, 0); err != nil {
		t.Fatalf("AddAllowedGroup: %v", err)
	}

	chat := telego.Chat{ID: groupID, Type: "supergroup", Title: "g"}
	from := &telego.User{ID: 42}

	// Store the original message.
	b.handleUpdate(context.Background(), telego.Update{Message: &telego.Message{
		MessageID: 7,
		Chat:      chat,
		From:      from,
		Text:      "original",
	}})

	// Edit it.
	b.handleUpdate(context.Background(), telego.Update{EditedMessage: &telego.Message{
		MessageID: 7,
		Chat:      chat,
		From:      from,
		Text:      "edited",
	}})

	msgs, err := database.GetMessages(context.Background(), groupID, time.Now().Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 stored message (no duplicate), got %d", len(msgs))
	}
	if msgs[0].Text != "edited" {
		t.Errorf("expected text %q, got %q", "edited", msgs[0].Text)
	}
}

// TestHandleEditedMessageCaptionFallback verifies that editing a photo caption
// updates the stored text (caption falls back like in handleUpdate).
func TestHandleEditedMessageCaptionFallback(t *testing.T) {
	b, database, _ := newTestBot(t, &fakeSummarizer{})
	defer func() { _ = database.Close() }()

	const groupID int64 = -1001111111111
	if err := database.AddAllowedGroup(context.Background(), groupID, 0); err != nil {
		t.Fatalf("AddAllowedGroup: %v", err)
	}

	chat := telego.Chat{ID: groupID, Type: "supergroup", Title: "g"}
	from := &telego.User{ID: 42}

	b.handleUpdate(context.Background(), telego.Update{Message: &telego.Message{
		MessageID: 11,
		Chat:      chat,
		From:      from,
		Caption:   "before",
		Photo:     []telego.PhotoSize{{FileID: "f", FileUniqueID: "u", Width: 10, Height: 10}},
	}})

	b.handleUpdate(context.Background(), telego.Update{EditedMessage: &telego.Message{
		MessageID: 11,
		Chat:      chat,
		From:      from,
		Caption:   "after",
		Photo:     []telego.PhotoSize{{FileID: "f", FileUniqueID: "u", Width: 10, Height: 10}},
	}})

	msgs, err := database.GetMessages(context.Background(), groupID, time.Now().Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 stored message, got %d", len(msgs))
	}
	if msgs[0].Text != "after" {
		t.Errorf("expected text %q, got %q", "after", msgs[0].Text)
	}
}

// shortEditRetryDelay shrinks the edit-retry backoff for the duration of a test.
func shortEditRetryDelay(t *testing.T) {
	t.Helper()
	orig := editedMsgRetryDelay
	editedMsgRetryDelay = 200 * time.Millisecond
	t.Cleanup(func() { editedMsgRetryDelay = orig })
}

// TestHandleEditedMessageUnknownID verifies that editing a message that was
// never stored inserts nothing and does not panic.
func TestHandleEditedMessageUnknownID(t *testing.T) {
	b, database, _ := newTestBot(t, &fakeSummarizer{})
	defer func() { _ = database.Close() }()
	shortEditRetryDelay(t)

	const groupID int64 = -1002222222222
	if err := database.AddAllowedGroup(context.Background(), groupID, 0); err != nil {
		t.Fatalf("AddAllowedGroup: %v", err)
	}

	b.handleUpdate(context.Background(), telego.Update{EditedMessage: &telego.Message{
		MessageID: 999,
		Chat:      telego.Chat{ID: groupID, Type: "supergroup", Title: "g"},
		From:      &telego.User{ID: 42},
		Text:      "edited",
	}})

	msgs, err := database.GetMessages(context.Background(), groupID, time.Now().Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected 0 stored messages, got %d", len(msgs))
	}
}

// TestHandleEditedMessageRetriesAfterInsertRace verifies the edit-vs-insert
// ordering race fix: when the edit arrives before the original message's
// insert (parallel dispatch), the edit retries once and applies.
func TestHandleEditedMessageRetriesAfterInsertRace(t *testing.T) {
	b, database, _ := newTestBot(t, &fakeSummarizer{})
	defer func() { _ = database.Close() }()
	shortEditRetryDelay(t)

	const groupID int64 = -1003333333333
	if err := database.AddAllowedGroup(context.Background(), groupID, 0); err != nil {
		t.Fatalf("AddAllowedGroup: %v", err)
	}

	chat := telego.Chat{ID: groupID, Type: "supergroup", Title: "g"}
	from := &telego.User{ID: 42}

	// The original message lands only while the edit handler is waiting to retry.
	inserted := make(chan struct{})
	go func() {
		defer close(inserted)
		// Well before the retry fires, leaving a wide margin on slow CI.
		time.Sleep(editedMsgRetryDelay / 10)
		b.handleUpdate(context.Background(), telego.Update{Message: &telego.Message{
			MessageID: 7,
			Chat:      chat,
			From:      from,
			Text:      "original",
		}})
	}()

	b.handleUpdate(context.Background(), telego.Update{EditedMessage: &telego.Message{
		MessageID: 7,
		Chat:      chat,
		From:      from,
		Text:      "edited",
	}})
	<-inserted

	msgs, err := database.GetMessages(context.Background(), groupID, time.Now().Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 stored message, got %d", len(msgs))
	}
	if msgs[0].Text != "edited" {
		t.Errorf("expected retried edit to apply, got text %q", msgs[0].Text)
	}
}
