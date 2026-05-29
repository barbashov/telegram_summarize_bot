package tgutil

import (
	"fmt"
	"time"
)

// FormatDuration renders a coarse, user-facing Russian duration ("N секунд" under
// a minute, otherwise "N минут"). Shared by the group and admin handlers for
// rate-limit wait messages.
func FormatDuration(d time.Duration) string {
	seconds := int(d.Seconds())
	if seconds < 60 {
		return fmt.Sprintf("%d секунд", seconds)
	}
	minutes := seconds / 60
	return fmt.Sprintf("%d минут", minutes)
}
