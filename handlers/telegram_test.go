package handlers

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"telegram_summarize_bot/metrics"
)

type editTrackingTelegram struct {
	fakeTelegram
	editCalls int
	editErr   error
}

func (f *editTrackingTelegram) EditMessageText(_ context.Context, params *telego.EditMessageTextParams) (*telego.Message, error) {
	f.editCalls++
	if f.editErr != nil {
		return nil, f.editErr
	}
	f.editTexts = append(f.editTexts, params.Text)
	return &telego.Message{MessageID: params.MessageID}, nil
}

func TestEditWithRetrySucceedsFirstAttempt(t *testing.T) {
	tg := &editTrackingTelegram{}
	b := &Bot{telegram: tg, metrics: newTestMetrics()}

	b.editWithRetry(context.Background(), 1, 1, "hello")

	if tg.editCalls != 1 {
		t.Fatalf("expected 1 edit call, got %d", tg.editCalls)
	}
	if len(tg.editTexts) != 1 || tg.editTexts[0] != "hello" {
		t.Fatalf("unexpected edit texts: %v", tg.editTexts)
	}
}

func TestEditWithRetrySucceedsAfterFailures(t *testing.T) {
	failCount := 0
	tg := &countingEditTelegram{
		failUntil: 2,
		failErr:   fmt.Errorf("timeout"),
	}
	_ = failCount
	b := &Bot{telegram: tg, metrics: newTestMetrics()}

	b.editWithRetry(context.Background(), 1, 1, "hello")

	if tg.editCalls != 3 {
		t.Fatalf("expected 3 edit calls, got %d", tg.editCalls)
	}
	if len(tg.editTexts) != 1 || tg.editTexts[0] != "hello" {
		t.Fatalf("unexpected edit texts: %v", tg.editTexts)
	}
}

func TestEditWithRetryNoFallbackSend(t *testing.T) {
	tg := &editTrackingTelegram{
		editErr: fmt.Errorf("timeout"),
	}
	b := &Bot{telegram: tg, metrics: newTestMetrics()}

	b.editWithRetry(context.Background(), 1, 1, "hello")

	if tg.editCalls != editRetries {
		t.Fatalf("expected %d edit calls, got %d", editRetries, tg.editCalls)
	}
	if len(tg.sentTexts) != 0 {
		t.Fatalf("expected no send fallback, got %d sends", len(tg.sentTexts))
	}
}

func TestEditFormattedWithRetryNoFallbackSend(t *testing.T) {
	tg := &editTrackingTelegram{
		editErr: fmt.Errorf("timeout"),
	}
	b := &Bot{telegram: tg, metrics: newTestMetrics()}

	b.editFormattedWithRetry(context.Background(), 1, 1, "hello")

	if tg.editCalls != editRetries {
		t.Fatalf("expected %d edit calls, got %d", editRetries, tg.editCalls)
	}
	if len(tg.sentTexts) != 0 {
		t.Fatalf("expected no send fallback, got %d sends", len(tg.sentTexts))
	}
}

// countingEditTelegram fails edit calls until failUntil attempts have been made.
type countingEditTelegram struct {
	fakeTelegram
	editCalls int
	failUntil int
	failErr   error
}

func (f *countingEditTelegram) EditMessageText(_ context.Context, params *telego.EditMessageTextParams) (*telego.Message, error) {
	f.editCalls++
	if f.editCalls <= f.failUntil {
		return nil, f.failErr
	}
	f.editTexts = append(f.editTexts, params.Text)
	return &telego.Message{MessageID: params.MessageID}, nil
}

func newTestMetrics() *metrics.Metrics {
	return metrics.New()
}

func TestSleepCtx(t *testing.T) {
	t.Run("returns true after timer fires", func(t *testing.T) {
		if !sleepCtx(context.Background(), 5*time.Millisecond) {
			t.Fatal("expected true after timer fired")
		}
	})

	t.Run("returns false when context already cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if sleepCtx(ctx, time.Second) {
			t.Fatal("expected false for cancelled context")
		}
	})

	t.Run("returns false when cancelled mid-wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(5 * time.Millisecond)
			cancel()
		}()
		start := time.Now()
		ok := sleepCtx(ctx, 5*time.Second)
		elapsed := time.Since(start)
		if ok {
			t.Fatal("expected false on mid-wait cancellation")
		}
		if elapsed > 500*time.Millisecond {
			t.Fatalf("waited %v; should bail near 5ms", elapsed)
		}
	})
}
