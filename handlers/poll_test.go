package handlers

import (
	"context"
	"testing"

	"github.com/mymmrac/telego"
)

type failingPersistencePoller struct {
	*fakeTelegram
	offsets []int
	stop    context.CancelFunc
}

func (tg *failingPersistencePoller) GetUpdates(_ context.Context, params *telego.GetUpdatesParams) ([]telego.Update, error) {
	tg.offsets = append(tg.offsets, params.Offset)
	if len(tg.offsets) > 1 {
		tg.stop()
		return nil, nil
	}
	return []telego.Update{{UpdateID: 123, Message: &telego.Message{
		MessageID: 5, From: &telego.User{ID: 7}, Chat: telego.Chat{ID: 42, Type: "group"}, Text: "test",
	}}}, nil
}

func TestPollDoesNotAcknowledgeFailedPersistence(t *testing.T) {
	b, store, fake := newTestBot(t, &fakeSummarizer{})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	poller := &failingPersistencePoller{fakeTelegram: fake, stop: stop}
	b.telegram = poller
	if err := b.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(poller.offsets) != 2 || poller.offsets[1] != 0 {
		t.Fatalf("failed update was acknowledged: %v", poller.offsets)
	}
}

func TestQueuedEditsRespectVersion(t *testing.T) {
	b, store, _ := newTestBot(t, &fakeSummarizer{})
	t.Cleanup(func() { _ = store.Close() })
	update := telego.Update{UpdateID: 1, Message: &telego.Message{
		MessageID: 5, From: &telego.User{ID: 7}, Chat: telego.Chat{ID: 42, Type: "group"}, Text: "original",
	}}
	if err := b.queueUpdate(t.Context(), update); err != nil {
		t.Fatal(err)
	}
	edit := *update.Message
	edit.EditDate, edit.Text = 12345, "new"
	if err := b.queueUpdate(t.Context(), telego.Update{UpdateID: 3, EditedMessage: &edit}); err != nil {
		t.Fatal(err)
	}
	edit.Text = "old"
	if err := b.queueUpdate(t.Context(), telego.Update{UpdateID: 2, EditedMessage: &edit}); err != nil {
		t.Fatal(err)
	}
	msg, err := store.GetMessageByTgID(t.Context(), 42, 5)
	if err != nil || msg == nil || msg.Text != "new" {
		t.Fatalf("stored message=%v err=%v", msg, err)
	}
}
