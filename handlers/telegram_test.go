package handlers

import (
	"fmt"
	"testing"

	"github.com/mymmrac/telego"
	"telegram_summarize_bot/metrics"
)

type editTrackingTelegram struct {
	fakeTelegram
	editCalls int
	editErr   error
}

func (f *editTrackingTelegram) EditMessageText(params *telego.EditMessageTextParams) (*telego.Message, error) {
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

	b.editWithRetry(1, 1, "hello")

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

	b.editWithRetry(1, 1, "hello")

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

	b.editWithRetry(1, 1, "hello")

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

	b.editFormattedWithRetry(1, 1, "hello")

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

func (f *countingEditTelegram) EditMessageText(params *telego.EditMessageTextParams) (*telego.Message, error) {
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
