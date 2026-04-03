package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"telegram_summarize_bot/metrics"
)

// newTestDB creates a fresh DB in a temp directory for testing.
func newTestDB(t *testing.T) *DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	m := metrics.New()
	db, err := New(dbPath, m)
	if err != nil {
		t.Fatalf("New(%q): %v", dbPath, err)
	}
	m.InitLatencyStats(db)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestNew(t *testing.T) {
	t.Run("creates database and migrates", func(t *testing.T) {
		db := newTestDB(t)
		if db == nil {
			t.Fatal("expected non-nil DB")
		}
	})

	t.Run("creates parent directories", func(t *testing.T) {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "sub", "dir", "test.db")
		m := metrics.New()
		db, err := New(dbPath, m)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_ = db.Close()
	})

	t.Run("idempotent migration", func(t *testing.T) {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "test.db")
		m := metrics.New()
		db1, err := New(dbPath, m)
		if err != nil {
			t.Fatalf("first New: %v", err)
		}
		_ = db1.Close()

		db2, err := New(dbPath, m)
		if err != nil {
			t.Fatalf("second New: %v", err)
		}
		_ = db2.Close()
	})
}

func TestAddMessageAndGetMessages(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	now := time.Now()
	msgs := []*Message{
		{GroupID: -100, UserHash: "aabbccdd", Text: "hello", Timestamp: now.Add(-3 * time.Hour)},
		{GroupID: -100, UserHash: "11223344", Text: "world", Timestamp: now.Add(-2 * time.Hour)},
		{GroupID: -100, UserHash: "aabbccdd", Text: "latest", Timestamp: now.Add(-1 * time.Hour)},
	}
	for _, m := range msgs {
		if err := db.AddMessage(ctx, m); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
	}

	t.Run("returns messages in chronological order", func(t *testing.T) {
		got, err := db.GetMessages(ctx, -100, now.Add(-4*time.Hour), 100)
		if err != nil {
			t.Fatalf("GetMessages: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("expected 3 messages, got %d", len(got))
		}
		if got[0].Text != "hello" || got[1].Text != "world" || got[2].Text != "latest" {
			t.Errorf("unexpected order: %v, %v, %v", got[0].Text, got[1].Text, got[2].Text)
		}
	})

	t.Run("respects since filter", func(t *testing.T) {
		got, err := db.GetMessages(ctx, -100, now.Add(-90*time.Minute), 100)
		if err != nil {
			t.Fatalf("GetMessages: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 message, got %d", len(got))
		}
		if got[0].Text != "latest" {
			t.Errorf("expected 'latest', got %q", got[0].Text)
		}
	})

	t.Run("respects limit", func(t *testing.T) {
		got, err := db.GetMessages(ctx, -100, now.Add(-4*time.Hour), 2)
		if err != nil {
			t.Fatalf("GetMessages: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("expected 2 messages, got %d", len(got))
		}
		// Limit applied to DESC query then reversed, so we get the 2 most recent in chrono order.
		if got[0].Text != "world" || got[1].Text != "latest" {
			t.Errorf("unexpected messages: %v, %v", got[0].Text, got[1].Text)
		}
	})

	t.Run("filters by group", func(t *testing.T) {
		if err := db.AddMessage(ctx, &Message{GroupID: -200, UserHash: "aabb", Text: "other group", Timestamp: now}); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
		got, err := db.GetMessages(ctx, -200, now.Add(-1*time.Hour), 100)
		if err != nil {
			t.Fatalf("GetMessages: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1, got %d", len(got))
		}
	})

	t.Run("empty text is not stored", func(t *testing.T) {
		if err := db.AddMessage(ctx, &Message{GroupID: -300, UserHash: "xx", Text: "", Timestamp: now}); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
		got, err := db.GetMessages(ctx, -300, now.Add(-1*time.Hour), 100)
		if err != nil {
			t.Fatalf("GetMessages: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("expected 0 messages for empty text, got %d", len(got))
		}
	})

	t.Run("no results returns empty slice", func(t *testing.T) {
		got, err := db.GetMessages(ctx, -999, now.Add(-1*time.Hour), 100)
		if err != nil {
			t.Fatalf("GetMessages: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil slice, got %v", got)
		}
	})
}

func TestAddMessageFields(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	now := time.Now()
	msg := &Message{
		GroupID:       -100,
		UserHash:      "aabbccdd",
		Text:          "forwarded text",
		Timestamp:     now,
		ForwardedFrom: "OriginalUser",
		TgMessageID:   42,
		ReplyToTgID:   10,
	}
	if err := db.AddMessage(ctx, msg); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	got, err := db.GetMessages(ctx, -100, now.Add(-1*time.Hour), 100)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got))
	}
	if got[0].ForwardedFrom != "OriginalUser" {
		t.Errorf("ForwardedFrom: got %q, want %q", got[0].ForwardedFrom, "OriginalUser")
	}
	if got[0].TgMessageID != 42 {
		t.Errorf("TgMessageID: got %d, want 42", got[0].TgMessageID)
	}
	if got[0].ReplyToTgID != 10 {
		t.Errorf("ReplyToTgID: got %d, want 10", got[0].ReplyToTgID)
	}
}

func TestAddMessageDeduplication(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	now := time.Now()
	msg := &Message{
		GroupID:     -100,
		UserHash:    "aabb",
		Text:        "first",
		Timestamp:   now,
		TgMessageID: 123,
	}
	if err := db.AddMessage(ctx, msg); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	// Insert duplicate with same group+tg_message_id: should be ignored (INSERT OR IGNORE).
	dup := &Message{
		GroupID:     -100,
		UserHash:    "aabb",
		Text:        "duplicate",
		Timestamp:   now,
		TgMessageID: 123,
	}
	if err := db.AddMessage(ctx, dup); err != nil {
		t.Fatalf("AddMessage dup: %v", err)
	}

	got, err := db.GetMessages(ctx, -100, now.Add(-1*time.Hour), 100)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 message after dedup, got %d", len(got))
	}
	if got[0].Text != "first" {
		t.Errorf("expected 'first', got %q", got[0].Text)
	}
}

func TestCleanupOldMessages(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	now := time.Now()
	if err := db.AddMessage(ctx, &Message{GroupID: -1, UserHash: "aa", Text: "old", Timestamp: now.Add(-48 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddMessage(ctx, &Message{GroupID: -1, UserHash: "bb", Text: "new", Timestamp: now}); err != nil {
		t.Fatal(err)
	}

	deleted, err := db.CleanupOldMessages(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("CleanupOldMessages: %v", err)
	}
	if deleted != 1 {
		t.Errorf("expected 1 deleted, got %d", deleted)
	}

	got, err := db.GetMessages(ctx, -1, now.Add(-72*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 remaining, got %d", len(got))
	}
	if got[0].Text != "new" {
		t.Errorf("expected 'new', got %q", got[0].Text)
	}
}

func TestCleanupOldMessagesNothingToDelete(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	deleted, err := db.CleanupOldMessages(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("CleanupOldMessages: %v", err)
	}
	if deleted != 0 {
		t.Errorf("expected 0 deleted, got %d", deleted)
	}
}

func TestGetUserHashSalt(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	t.Run("generates salt on first call", func(t *testing.T) {
		salt, err := db.GetUserHashSalt(ctx)
		if err != nil {
			t.Fatalf("GetUserHashSalt: %v", err)
		}
		if len(salt) != 32 {
			t.Errorf("expected 32-byte salt, got %d bytes", len(salt))
		}
	})

	t.Run("returns same salt on subsequent calls", func(t *testing.T) {
		salt1, err := db.GetUserHashSalt(ctx)
		if err != nil {
			t.Fatal(err)
		}
		salt2, err := db.GetUserHashSalt(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if string(salt1) != string(salt2) {
			t.Errorf("salts differ: %x vs %x", salt1, salt2)
		}
	})
}

func TestUserHash(t *testing.T) {
	salt := []byte("test-salt-32-bytes-long-00000000")

	t.Run("returns 8-char hex", func(t *testing.T) {
		h := UserHash(123, -100, salt)
		if len(h) != 8 {
			t.Errorf("expected 8-char hash, got %d chars: %q", len(h), h)
		}
	})

	t.Run("deterministic", func(t *testing.T) {
		h1 := UserHash(123, -100, salt)
		h2 := UserHash(123, -100, salt)
		if h1 != h2 {
			t.Errorf("hashes differ: %q vs %q", h1, h2)
		}
	})

	t.Run("different users produce different hashes", func(t *testing.T) {
		h1 := UserHash(123, -100, salt)
		h2 := UserHash(456, -100, salt)
		if h1 == h2 {
			t.Errorf("different users produced same hash: %q", h1)
		}
	})

	t.Run("different groups produce different hashes", func(t *testing.T) {
		h1 := UserHash(123, -100, salt)
		h2 := UserHash(123, -200, salt)
		if h1 == h2 {
			t.Errorf("different groups produced same hash: %q", h1)
		}
	})

	t.Run("different salts produce different hashes", func(t *testing.T) {
		salt2 := []byte("other-salt-32-bytes-long-0000000")
		h1 := UserHash(123, -100, salt)
		h2 := UserHash(123, -100, salt2)
		if h1 == h2 {
			t.Errorf("different salts produced same hash: %q", h1)
		}
	})
}

func TestAllowedGroups(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	t.Run("initially no groups allowed", func(t *testing.T) {
		allowed, err := db.IsGroupAllowed(ctx, -100)
		if err != nil {
			t.Fatal(err)
		}
		if allowed {
			t.Error("expected group not allowed")
		}

		ids, err := db.GetAllowedGroupIDs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 0 {
			t.Errorf("expected empty list, got %v", ids)
		}
	})

	t.Run("add and check", func(t *testing.T) {
		if err := db.AddAllowedGroup(ctx, -100, 42); err != nil {
			t.Fatal(err)
		}
		allowed, err := db.IsGroupAllowed(ctx, -100)
		if err != nil {
			t.Fatal(err)
		}
		if !allowed {
			t.Error("expected group allowed after add")
		}
	})

	t.Run("add is idempotent", func(t *testing.T) {
		if err := db.AddAllowedGroup(ctx, -100, 42); err != nil {
			t.Fatal(err)
		}
		ids, err := db.GetAllowedGroupIDs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 1 {
			t.Errorf("expected 1 group, got %d", len(ids))
		}
	})

	t.Run("remove", func(t *testing.T) {
		if err := db.RemoveAllowedGroup(ctx, -100); err != nil {
			t.Fatal(err)
		}
		allowed, err := db.IsGroupAllowed(ctx, -100)
		if err != nil {
			t.Fatal(err)
		}
		if allowed {
			t.Error("expected group not allowed after remove")
		}
	})

	t.Run("remove non-existent is not an error", func(t *testing.T) {
		if err := db.RemoveAllowedGroup(ctx, -999); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("multiple groups sorted", func(t *testing.T) {
		if err := db.AddAllowedGroup(ctx, -300, 1); err != nil {
			t.Fatal(err)
		}
		if err := db.AddAllowedGroup(ctx, -100, 1); err != nil {
			t.Fatal(err)
		}
		if err := db.AddAllowedGroup(ctx, -200, 1); err != nil {
			t.Fatal(err)
		}
		ids, err := db.GetAllowedGroupIDs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 3 {
			t.Fatalf("expected 3 groups, got %d", len(ids))
		}
		if ids[0] != -300 || ids[1] != -200 || ids[2] != -100 {
			t.Errorf("expected sorted order [-300 -200 -100], got %v", ids)
		}
	})
}

func TestKnownGroups(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	t.Run("upsert and get", func(t *testing.T) {
		if err := db.UpsertKnownGroup(ctx, -100, "Test Group", "testgroup"); err != nil {
			t.Fatal(err)
		}

		groups, err := db.GetKnownGroups(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(groups) != 1 {
			t.Fatalf("expected 1 group, got %d", len(groups))
		}
		g := groups[0]
		if g.GroupID != -100 {
			t.Errorf("GroupID: got %d, want -100", g.GroupID)
		}
		if g.Title != "Test Group" {
			t.Errorf("Title: got %q, want %q", g.Title, "Test Group")
		}
		if g.Username != "testgroup" {
			t.Errorf("Username: got %q, want %q", g.Username, "testgroup")
		}
		if g.Allowed {
			t.Error("expected Allowed=false when not in allowed_groups")
		}
	})

	t.Run("upsert updates existing", func(t *testing.T) {
		if err := db.UpsertKnownGroup(ctx, -100, "Updated Title", "newuser"); err != nil {
			t.Fatal(err)
		}
		groups, err := db.GetKnownGroups(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(groups) != 1 {
			t.Fatalf("expected 1 group, got %d", len(groups))
		}
		if groups[0].Title != "Updated Title" {
			t.Errorf("Title: got %q, want %q", groups[0].Title, "Updated Title")
		}
		if groups[0].Username != "newuser" {
			t.Errorf("Username: got %q, want %q", groups[0].Username, "newuser")
		}
	})

	t.Run("allowed flag joins with allowed_groups", func(t *testing.T) {
		if err := db.AddAllowedGroup(ctx, -100, 1); err != nil {
			t.Fatal(err)
		}
		groups, err := db.GetKnownGroups(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !groups[0].Allowed {
			t.Error("expected Allowed=true after adding to allowed_groups")
		}
	})

	t.Run("ordered by title", func(t *testing.T) {
		if err := db.UpsertKnownGroup(ctx, -200, "Alpha", ""); err != nil {
			t.Fatal(err)
		}
		if err := db.UpsertKnownGroup(ctx, -300, "Zeta", ""); err != nil {
			t.Fatal(err)
		}
		groups, err := db.GetKnownGroups(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(groups) != 3 {
			t.Fatalf("expected 3 groups, got %d", len(groups))
		}
		if groups[0].Title != "Alpha" || groups[2].Title != "Zeta" {
			t.Errorf("expected alphabetical order, got %q, %q, %q", groups[0].Title, groups[1].Title, groups[2].Title)
		}
	})
}

func TestGroupSchedule(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	t.Run("returns nil for non-existent", func(t *testing.T) {
		s, err := db.GetGroupSchedule(ctx, -100)
		if err != nil {
			t.Fatal(err)
		}
		if s != nil {
			t.Errorf("expected nil, got %+v", s)
		}
	})

	t.Run("set and get", func(t *testing.T) {
		sched := &GroupSchedule{
			GroupID: -100,
			Enabled: true,
			Hour:    9,
			Minute:  30,
		}
		if err := db.SetGroupSchedule(ctx, sched); err != nil {
			t.Fatal(err)
		}

		got, err := db.GetGroupSchedule(ctx, -100)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			t.Fatal("expected schedule, got nil")
		}
		if got.GroupID != -100 || !got.Enabled || got.Hour != 9 || got.Minute != 30 {
			t.Errorf("unexpected schedule: %+v", got)
		}
		if got.LastDailySummary != nil {
			t.Errorf("expected nil LastDailySummary, got %v", got.LastDailySummary)
		}
	})

	t.Run("update replaces", func(t *testing.T) {
		sched := &GroupSchedule{
			GroupID: -100,
			Enabled: false,
			Hour:    14,
			Minute:  0,
		}
		if err := db.SetGroupSchedule(ctx, sched); err != nil {
			t.Fatal(err)
		}
		got, err := db.GetGroupSchedule(ctx, -100)
		if err != nil {
			t.Fatal(err)
		}
		if got.Enabled || got.Hour != 14 {
			t.Errorf("unexpected schedule after update: %+v", got)
		}
	})

	t.Run("update last daily summary", func(t *testing.T) {
		// Re-enable so it appears in GetEnabledSchedules.
		sched := &GroupSchedule{GroupID: -100, Enabled: true, Hour: 9, Minute: 0}
		if err := db.SetGroupSchedule(ctx, sched); err != nil {
			t.Fatal(err)
		}

		now := time.Now().Truncate(time.Second)
		if err := db.UpdateLastDailySummary(ctx, -100, now); err != nil {
			t.Fatal(err)
		}

		got, err := db.GetGroupSchedule(ctx, -100)
		if err != nil {
			t.Fatal(err)
		}
		if got.LastDailySummary == nil {
			t.Fatal("expected LastDailySummary, got nil")
		}
		if !got.LastDailySummary.Truncate(time.Second).Equal(now) {
			t.Errorf("LastDailySummary: got %v, want %v", got.LastDailySummary, now)
		}
	})
}

func TestGetEnabledSchedules(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	// Add one enabled and one disabled.
	if err := db.SetGroupSchedule(ctx, &GroupSchedule{GroupID: -100, Enabled: true, Hour: 8, Minute: 0}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetGroupSchedule(ctx, &GroupSchedule{GroupID: -200, Enabled: false, Hour: 12, Minute: 0}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetGroupSchedule(ctx, &GroupSchedule{GroupID: -300, Enabled: true, Hour: 20, Minute: 15}); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetEnabledSchedules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 enabled schedules, got %d", len(got))
	}
	for _, s := range got {
		if !s.Enabled {
			t.Errorf("expected only enabled schedules, got disabled: %+v", s)
		}
	}
}

func TestBotEvents(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	now := time.Now()

	t.Run("insert and query", func(t *testing.T) {
		if err := db.InsertBotEvent(ctx, "test_metric", now.Add(-2*time.Hour), 1000000); err != nil {
			t.Fatal(err)
		}
		if err := db.InsertBotEvent(ctx, "test_metric", now.Add(-1*time.Hour), 2000000); err != nil {
			t.Fatal(err)
		}
		if err := db.InsertBotEvent(ctx, "other_metric", now, 500000); err != nil {
			t.Fatal(err)
		}

		events, err := db.QueryBotEvents(ctx, "test_metric", now.Add(-3*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 2 {
			t.Fatalf("expected 2 events, got %d", len(events))
		}
		if events[0].DurationNS != 1000000 || events[1].DurationNS != 2000000 {
			t.Errorf("unexpected durations: %d, %d", events[0].DurationNS, events[1].DurationNS)
		}
	})

	t.Run("count", func(t *testing.T) {
		count, err := db.CountBotEvents(ctx, "test_metric", now.Add(-3*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if count != 2 {
			t.Errorf("expected count 2, got %d", count)
		}
	})

	t.Run("count with since filter", func(t *testing.T) {
		count, err := db.CountBotEvents(ctx, "test_metric", now.Add(-90*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("expected count 1, got %d", count)
		}
	})

	t.Run("purge old events", func(t *testing.T) {
		deleted, err := db.PurgeOldBotEvents(ctx, now.Add(-90*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if deleted != 1 {
			t.Errorf("expected 1 purged, got %d", deleted)
		}

		remaining, err := db.QueryBotEvents(ctx, "test_metric", now.Add(-3*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if len(remaining) != 1 {
			t.Errorf("expected 1 remaining, got %d", len(remaining))
		}
	})
}

func TestSeedAllowedGroupsIfEmpty(t *testing.T) {
	ctx := context.Background()

	t.Run("seeds when empty", func(t *testing.T) {
		db := newTestDB(t)
		if err := db.SeedAllowedGroupsIfEmpty(ctx, []int64{-100, -200}); err != nil {
			t.Fatal(err)
		}
		ids, err := db.GetAllowedGroupIDs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 2 {
			t.Errorf("expected 2 groups, got %d", len(ids))
		}
	})

	t.Run("does not seed when already populated", func(t *testing.T) {
		db := newTestDB(t)
		if err := db.AddAllowedGroup(ctx, -50, 0); err != nil {
			t.Fatal(err)
		}
		if err := db.SeedAllowedGroupsIfEmpty(ctx, []int64{-100, -200}); err != nil {
			t.Fatal(err)
		}
		ids, err := db.GetAllowedGroupIDs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 1 {
			t.Errorf("expected 1 group (seed skipped), got %d", len(ids))
		}
		if ids[0] != -50 {
			t.Errorf("expected -50, got %d", ids[0])
		}
	})

	t.Run("empty list is no-op", func(t *testing.T) {
		db := newTestDB(t)
		if err := db.SeedAllowedGroupsIfEmpty(ctx, nil); err != nil {
			t.Fatal(err)
		}
		ids, err := db.GetAllowedGroupIDs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 0 {
			t.Errorf("expected 0, got %d", len(ids))
		}
	})

	t.Run("idempotent when called twice", func(t *testing.T) {
		db := newTestDB(t)
		if err := db.SeedAllowedGroupsIfEmpty(ctx, []int64{-100}); err != nil {
			t.Fatal(err)
		}
		if err := db.SeedAllowedGroupsIfEmpty(ctx, []int64{-200, -300}); err != nil {
			t.Fatal(err)
		}
		ids, err := db.GetAllowedGroupIDs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 1 {
			t.Errorf("expected 1 group (second seed skipped), got %d", len(ids))
		}
	})
}

func TestLastSummarizeTime(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	t.Run("returns nil when not set", func(t *testing.T) {
		got, err := db.GetLastSummarizeTime(ctx, -100)
		if err != nil {
			t.Fatal(err)
		}
		if got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("set and get", func(t *testing.T) {
		now := time.Now().Truncate(time.Second)
		if err := db.SetLastSummarizeTime(ctx, -100, now); err != nil {
			t.Fatal(err)
		}
		got, err := db.GetLastSummarizeTime(ctx, -100)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			t.Fatal("expected time, got nil")
		}
		if !got.Truncate(time.Second).Equal(now) {
			t.Errorf("got %v, want %v", got, now)
		}
	})

	t.Run("update replaces", func(t *testing.T) {
		t1 := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
		t2 := time.Now().Truncate(time.Second)
		if err := db.SetLastSummarizeTime(ctx, -200, t1); err != nil {
			t.Fatal(err)
		}
		if err := db.SetLastSummarizeTime(ctx, -200, t2); err != nil {
			t.Fatal(err)
		}
		got, err := db.GetLastSummarizeTime(ctx, -200)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Truncate(time.Second).Equal(t2) {
			t.Errorf("got %v, want %v", got, t2)
		}
	})

	t.Run("independent per group", func(t *testing.T) {
		t1 := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
		t2 := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
		if err := db.SetLastSummarizeTime(ctx, -300, t1); err != nil {
			t.Fatal(err)
		}
		if err := db.SetLastSummarizeTime(ctx, -400, t2); err != nil {
			t.Fatal(err)
		}
		got1, _ := db.GetLastSummarizeTime(ctx, -300)
		got2, _ := db.GetLastSummarizeTime(ctx, -400)
		if got1.Truncate(time.Second).Equal(got2.Truncate(time.Second)) {
			t.Error("expected different times for different groups")
		}
	})
}

func TestClearAllMetrics(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	now := time.Now()
	if err := db.InsertBotEvent(ctx, "m1", now, 100); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertErrorLog(ctx, now, "err_key", "msg"); err != nil {
		t.Fatal(err)
	}

	if err := db.ClearAllMetrics(ctx); err != nil {
		t.Fatal(err)
	}

	count, err := db.CountBotEvents(ctx, "m1", now.Add(-1*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 events after clear, got %d", count)
	}

	errCounts, err := db.QueryErrorCounts(ctx, now.Add(-1*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(errCounts) != 0 {
		t.Errorf("expected 0 error counts after clear, got %v", errCounts)
	}
}

func TestErrorLog(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	now := time.Now()

	if err := db.InsertErrorLog(ctx, now.Add(-2*time.Hour), "llm", "timeout"); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertErrorLog(ctx, now.Add(-1*time.Hour), "llm", "rate limited"); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertErrorLog(ctx, now, "telegram", "send failed"); err != nil {
		t.Fatal(err)
	}

	t.Run("query error counts", func(t *testing.T) {
		counts, err := db.QueryErrorCounts(ctx, now.Add(-3*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if counts["llm"] != 2 {
			t.Errorf("expected llm=2, got %d", counts["llm"])
		}
		if counts["telegram"] != 1 {
			t.Errorf("expected telegram=1, got %d", counts["telegram"])
		}
	})

	t.Run("count errors with keys", func(t *testing.T) {
		count, err := db.CountErrors(ctx, now.Add(-3*time.Hour), "llm")
		if err != nil {
			t.Fatal(err)
		}
		if count != 2 {
			t.Errorf("expected 2, got %d", count)
		}
	})

	t.Run("count errors with multiple keys", func(t *testing.T) {
		count, err := db.CountErrors(ctx, now.Add(-3*time.Hour), "llm", "telegram")
		if err != nil {
			t.Fatal(err)
		}
		if count != 3 {
			t.Errorf("expected 3, got %d", count)
		}
	})

	t.Run("count errors with no keys", func(t *testing.T) {
		count, err := db.CountErrors(ctx, now.Add(-3*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("expected 0, got %d", count)
		}
	})

	t.Run("purge old errors", func(t *testing.T) {
		deleted, err := db.PurgeOldErrors(ctx, now.Add(-90*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if deleted != 1 {
			t.Errorf("expected 1 purged, got %d", deleted)
		}
	})
}
