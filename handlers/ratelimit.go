package handlers

import (
	"fmt"
	"sync"
	"time"

	"telegram_summarize_bot/logger"
)

type RateLimiter struct {
	entries map[string]time.Time
	mu      sync.RWMutex
	limit   time.Duration
}

func NewRateLimiter(limitSeconds int) *RateLimiter {
	return &RateLimiter{
		entries: make(map[string]time.Time),
		limit:   time.Duration(limitSeconds) * time.Second,
	}
}

// Allow reports whether the group may run a summarize now. On success it
// returns the grant timestamp it stored; pass it back to Release so a failed
// slow request can only free its own slot.
func (r *RateLimiter) Allow(groupID int64) (bool, time.Time) {
	key := r.key(groupID)
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	if lastTime, exists := r.entries[key]; exists {
		if now.Sub(lastTime) < r.limit {
			remaining := r.limit - now.Sub(lastTime)
			logger.Info().
				Int64("group_id", groupID).
				Dur("remaining", remaining).
				Msg("rate limited")
			return false, time.Time{}
		}
	}

	r.entries[key] = now
	return true, now
}

// Release frees the slot taken by Allow, but only if the stored entry is still
// the given grant — an unconditional delete would let a failed slow request
// free an entry that a newer request has since claimed.
func (r *RateLimiter) Release(groupID int64, grant time.Time) {
	key := r.key(groupID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if stored, exists := r.entries[key]; exists && stored.Equal(grant) {
		delete(r.entries, key)
	}
}

func (r *RateLimiter) ClearOldEntries() {
	r.mu.Lock()
	defer r.mu.Unlock()

	cutoff := time.Now().Add(-r.limit * 2)
	for key, t := range r.entries {
		if t.Before(cutoff) {
			delete(r.entries, key)
		}
	}
}

func (r *RateLimiter) RemainingTime(groupID int64) time.Duration {
	key := r.key(groupID)
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := time.Now()
	if lastTime, exists := r.entries[key]; exists {
		remaining := r.limit - now.Sub(lastTime)
		if remaining > 0 {
			return remaining
		}
	}
	return 0
}

func (r *RateLimiter) key(groupID int64) string {
	return fmt.Sprintf("%d", groupID)
}
