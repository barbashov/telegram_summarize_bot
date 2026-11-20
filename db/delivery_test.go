package db

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"telegram_summarize_bot/metrics"
)

func TestBatchContinuesThroughSameSecondAndLateArrival(t *testing.T) {
	ctx := t.Context()
	store := newTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	add := func(id int64, at time.Time) {
		t.Helper()
		if err := store.AddMessage(ctx, &Message{GroupID: 42, TgMessageID: id, Timestamp: at, Text: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	add(1, now)
	add(2, now)
	batch, err := store.GetSummaryBatch(ctx, 42, now.Add(-time.Hour), 1)
	if err != nil || len(batch) != 1 {
		t.Fatalf("batch: %v, %v", batch, err)
	}
	d := Delivery{Key: "test", ChatID: 42, CursorID: batch[0].ID, Checkpoint: now, Parts: []DeliveryPart{{Text: "test"}}}
	if err := store.SaveDelivery(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmPart(ctx, &d); err != nil {
		t.Fatal(err)
	}
	add(3, now.Add(-time.Minute))
	batch, err = store.GetSummaryBatch(ctx, 42, now.Add(-time.Hour), 2)
	if err != nil || len(batch) != 2 || batch[0].TgMessageID != 2 || batch[1].TgMessageID != 3 {
		t.Fatalf("batch after cursor: %v, %v", batch, err)
	}
}

func TestRevokedLastGroupStaysRevokedAfterRestart(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "bot.db")
	first, err := New(path, metrics.New())
	if err != nil {
		t.Fatal(err)
	}
	if err := first.SeedAllowedGroupsIfEmpty(ctx, []int64{42}); err != nil {
		t.Fatal(err)
	}
	if err := first.RemoveAllowedGroup(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := New(path, metrics.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if err := second.SeedAllowedGroupsIfEmpty(ctx, []int64{42}); err != nil {
		t.Fatal(err)
	}
	if allowed, err := second.IsGroupAllowed(ctx, 42); err != nil || allowed {
		t.Fatalf("allowed=%t err=%v", allowed, err)
	}
}

func TestFailedBackupRollsBackDeletion(t *testing.T) {
	ctx := t.Context()
	store := newTestDB(t)
	id, err := store.AddMessageReturningID(ctx, &Message{GroupID: 42, TgMessageID: 1, Timestamp: time.Now(), Text: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddMessagePhotos(ctx, id, []PhotoRecord{{FileUniqueID: "img", FileID: "file-id"}}); err != nil {
		t.Fatal(err)
	}
	n, err := store.DeleteMessagesWithBackup(ctx, []int64{id}, func(photos map[int64][]PhotoRecord) error {
		if len(photos[id]) != 1 {
			t.Fatal("backup missing photo")
		}
		return errors.New("backup failed")
	})
	if n != 0 || err == nil {
		t.Fatalf("deleted=%d err=%v", n, err)
	}
	stored, err := store.GetMessageByTgID(ctx, 42, 1)
	if stored == nil || err != nil {
		t.Fatalf("stored=%v err=%v", stored, err)
	}
}

func TestLegacyCursorIncludesBoundarySecondAndLateNewRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	ctx := t.Context()
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	boundary := time.Now().UTC().Truncate(time.Second)
	for _, query := range []string{
		`CREATE TABLE messages(id INTEGER PRIMARY KEY AUTOINCREMENT, group_id INTEGER NOT NULL,
			user_hash TEXT NOT NULL DEFAULT '', text TEXT NOT NULL, timestamp DATETIME NOT NULL,
			forwarded_from TEXT, tg_message_id INTEGER, reply_to_tg_id INTEGER)`,
		`CREATE TABLE last_summarize(group_id INTEGER PRIMARY KEY, timestamp DATETIME NOT NULL)`,
		`CREATE TABLE allowed_groups(group_id INTEGER PRIMARY KEY, added_at DATETIME NOT NULL, added_by INTEGER)`,
	} {
		if _, err := conn.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO last_summarize VALUES (42,?)`, boundary.Add(500*time.Millisecond).String()); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO messages(group_id,text,timestamp,tg_message_id) VALUES
		(42,'old',?,1),(42,'boundary',?,2)`, boundary.Add(-time.Second).String(), boundary.String()); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := New(path, metrics.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SeedAllowedGroupsIfEmpty(ctx, []int64{42}); err != nil {
		t.Fatal(err)
	}
	if allowed, err := store.IsGroupAllowed(ctx, 42); err != nil || allowed {
		t.Fatalf("legacy allowlist: %v, %v", allowed, err)
	}
	if err := store.AddMessage(ctx, &Message{GroupID: 42, TgMessageID: 3, Text: "late", Timestamp: boundary.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetSummaryBatch(ctx, 42, boundary.Add(-time.Hour), 10)
	if err != nil || len(got) != 2 || got[0].TgMessageID != 2 || got[1].TgMessageID != 3 {
		t.Fatalf("batch=%+v err=%v", got, err)
	}
}
