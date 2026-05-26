package provider

import (
	"net/http"
	"testing"
	"time"
)

func TestParseCodexRateLimitsResetAfterSeconds(t *testing.T) {
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "16")
	h.Set("x-codex-primary-window-minutes", "300")
	h.Set("x-codex-primary-reset-after-seconds", "3600")
	h.Set("x-codex-secondary-used-percent", "28.5")
	h.Set("x-codex-secondary-window-minutes", "10080")
	h.Set("x-codex-secondary-reset-after-seconds", "86400")
	h.Set("x-codex-plan-type", "plus")

	snap, ok := ParseCodexRateLimits(h)
	if !ok {
		t.Fatal("expected ok")
	}
	if snap.PlanType != "plus" {
		t.Errorf("plan = %q, want plus", snap.PlanType)
	}
	if snap.Primary == nil || snap.Primary.UsedPercent != 16 || snap.Primary.WindowMinutes != 300 {
		t.Errorf("primary = %+v", snap.Primary)
	}
	if snap.Secondary == nil || snap.Secondary.UsedPercent != 28.5 || snap.Secondary.WindowMinutes != 10080 {
		t.Errorf("secondary = %+v", snap.Secondary)
	}
	if got := time.Until(snap.Primary.ResetsAt); got < 50*time.Minute || got > 61*time.Minute {
		t.Errorf("primary reset ~1h, got %v", got)
	}
}

func TestParseCodexRateLimitsResetAt(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "10")
	h.Set("x-codex-primary-window-minutes", "300")
	h.Set("x-codex-primary-reset-at", reset.Format(time.RFC3339))

	snap, ok := ParseCodexRateLimits(h)
	if !ok {
		t.Fatal("expected ok")
	}
	if !snap.Primary.ResetsAt.Equal(reset) {
		t.Errorf("reset = %v, want %v", snap.Primary.ResetsAt, reset)
	}
	if snap.Secondary != nil {
		t.Errorf("secondary should be nil, got %+v", snap.Secondary)
	}
}

func TestParseCodexRateLimitsNoHeaders(t *testing.T) {
	if _, ok := ParseCodexRateLimits(http.Header{}); ok {
		t.Error("expected ok=false for empty headers")
	}
}
