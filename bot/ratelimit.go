package bot

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

func (r *RateLimiter) Allow(userID int64, groupID int64) bool {
	key := r.key(groupID)
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	if lastTime, exists := r.entries[key]; exists {
		if now.Sub(lastTime) < r.limit {
			remaining := r.limit - now.Sub(lastTime)
			logger.Info().
				Int64("user_id", userID).
				Int64("group_id", groupID).
				Dur("remaining", remaining).
				Msg("rate limited")
			return false
		}
	}

	r.entries[key] = now
	return true
}

func (r *RateLimiter) Release(groupID int64) {
	key := r.key(groupID)
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, key)
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
