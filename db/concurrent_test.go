package db

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestConcurrentWritesNoLock regression-tests the SetMaxOpenConns(1) + WAL +
// busy_timeout hardening: many goroutines writing at once must not surface
// SQLITE_BUSY ("database is locked").
func TestConcurrentWritesNoLock(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if _, err := d.AddMessageReturningID(ctx, &Message{
				GroupID:     1,
				UserHash:    "h",
				Text:        "msg",
				Timestamp:   time.Now(),
				TgMessageID: int64(n + 1),
			}); err != nil {
				t.Errorf("AddMessageReturningID(%d): %v", n, err)
			}
		}(i)
	}
	wg.Wait()
}
