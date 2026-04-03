package handlers

import (
	"testing"
	"time"
)

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30 секунд"},
		{0, "0 секунд"},
		{59 * time.Second, "59 секунд"},
		{60 * time.Second, "1 минут"},
		{90 * time.Second, "1 минут"},
		{5 * time.Minute, "5 минут"},
	}

	for _, tt := range tests {
		t.Run(tt.d.String(), func(t *testing.T) {
			got := formatDuration(tt.d)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
