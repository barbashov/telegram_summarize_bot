package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"telegram_summarize_bot/db"
)

type tailFailureTelegram struct {
	*fakeTelegram
	fail bool
}

func (tg *tailFailureTelegram) SendMessage(ctx context.Context, params *telego.SendMessageParams) (*telego.Message, error) {
	if tg.fail && params.ParseMode != "" {
		return nil, errors.New("temporary send failure")
	}
	return tg.fakeTelegram.SendMessage(ctx, params)
}

func TestDeliveryResumesAfterTailFailure(t *testing.T) {
	ctx := t.Context()
	b, database, fake := newTestBot(t, &fakeSummarizer{})
	t.Cleanup(func() { _ = database.Close() })
	tg := &tailFailureTelegram{fakeTelegram: fake, fail: true}
	b.telegram = tg
	now := time.Now().UTC()
	d := db.Delivery{Key: "manual:42", ChatID: 42, StatusID: 1, Group: true, CursorID: 123, Checkpoint: now}
	if b.deliver(ctx, d, "**"+strings.Repeat("a", 4200)+"**") {
		t.Fatal("partial delivery must not succeed")
	}
	pending, err := database.GetDelivery(ctx, d.Key)
	if err != nil || pending == nil || pending.NextPart != 1 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	last, err := database.GetLastSummarizeTime(ctx, 42)
	if err != nil || last != nil {
		t.Fatalf("checkpoint after failure: %v, %v", last, err)
	}

	if err := database.DelayDelivery(ctx, d.Key, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	tg.fail = false
	restarted := &Bot{db: database, telegram: tg, metrics: b.metrics}
	if !restarted.resumeDelivery(ctx, d.Key) {
		t.Fatal("pending delivery did not resume")
	}
	if len(tg.editTexts) != 1 || len(tg.sentTexts) != 1 {
		t.Fatalf("edits=%d sent=%d", len(tg.editTexts), len(tg.sentTexts))
	}
	last, err = database.GetLastSummarizeTime(ctx, 42)
	if err != nil || last == nil {
		t.Fatalf("checkpoint after recovery: %v, %v", last, err)
	}
}
