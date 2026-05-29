package tgutil

import (
	"testing"
	"time"
)

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"seconds", 5 * time.Second, "5 секунд"},
		{"exactly a minute", 60 * time.Second, "1 минут"},
		{"minutes", 150 * time.Second, "2 минут"},
		{"sub-second rounds down", 500 * time.Millisecond, "0 секунд"},
		{"zero", 0, "0 секунд"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatDuration(tc.d); got != tc.want {
				t.Errorf("FormatDuration(%v) = %q, want %q", tc.d, got, tc.want)
			}
		})
	}
}
