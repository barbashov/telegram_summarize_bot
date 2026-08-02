package handlers

import (
	"testing"
	"time"
)

func TestRateLimiterAllowThenLimited(t *testing.T) {
	r := NewRateLimiter(60)

	allowed, grant := r.Allow(42)
	if !allowed {
		t.Fatal("first Allow should succeed")
	}
	if grant.IsZero() {
		t.Fatal("successful Allow must return its grant timestamp")
	}
	if allowed, _ := r.Allow(42); allowed {
		t.Fatal("second Allow within the limit should be rejected")
	}
	// Другая группа не задета.
	if allowed, _ := r.Allow(43); !allowed {
		t.Fatal("Allow for a different group should succeed")
	}
}

func TestRateLimiterReleaseFreesSlot(t *testing.T) {
	r := NewRateLimiter(60)

	_, grant := r.Allow(42)
	r.Release(42, grant)
	if allowed, _ := r.Allow(42); !allowed {
		t.Fatal("Allow after Release should succeed")
	}
}

func TestRateLimiterStaleReleaseDoesNotFreeNewerGrant(t *testing.T) {
	r := NewRateLimiter(60)

	// A slow request takes the slot, then a newer request claims it after the
	// window elapses. The old request's failure-path Release must not free the
	// newer request's entry.
	_, oldGrant := r.Allow(42)
	r.entries[r.key(42)] = time.Now().Add(-2 * time.Minute) // window elapsed
	allowed, _ := r.Allow(42)
	if !allowed {
		t.Fatal("Allow after the window should succeed")
	}

	r.Release(42, oldGrant) // stale release from the failed slow request

	if allowed, _ := r.Allow(42); allowed {
		t.Fatal("stale Release must not free the newer request's slot")
	}
}
