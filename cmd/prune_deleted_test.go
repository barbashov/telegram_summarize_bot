package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"telegram_summarize_bot/config"
	"telegram_summarize_bot/db"
	"telegram_summarize_bot/metrics"
)

// newTestDB opens a temp-file SQLite DB for prune-deleted tests.
func newTestDB(t *testing.T) (*config.Config, *db.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.New(dbPath, metrics.New())
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return &config.Config{DBPath: dbPath}, database
}

// writeExportFile writes an export JSON file and returns its path.
func writeExportFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "export.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write export: %v", err)
	}
	return path
}

func TestRunPruneDeleted_RejectsBasicGroup(t *testing.T) {
	cfg, _ := newTestDB(t)
	path := writeExportFile(t, `{"messages":[{"id":1,"type":"message"}]}`)

	// A basic-group ID (above the supergroup floor) must be rejected.
	err := runPruneDeleted(context.Background(), cfg, path, -123456789, false)
	if err == nil {
		t.Fatal("expected error for basic-group ID")
	}
	if !strings.Contains(err.Error(), "supergroup") {
		t.Errorf("error = %q, want mention of supergroup", err.Error())
	}
}

func TestRunPruneDeleted_RejectsMismatchedExport(t *testing.T) {
	cfg, _ := newTestDB(t)
	path := writeExportFile(t, `{"name":"Other Chat","id":999,"messages":[{"id":1,"type":"message"}]}`)

	// Export id 999 -> Bot API -1000000000999, but we target a different group.
	err := runPruneDeleted(context.Background(), cfg, path, -1000123456789, false)
	if err == nil {
		t.Fatal("expected error for mismatched export chat id")
	}
	if !strings.Contains(err.Error(), "Other Chat") || !strings.Contains(err.Error(), "different chat") {
		t.Errorf("error = %q, want mention of the export chat name and a refusal", err.Error())
	}
}

func TestRunPruneDeleted_MissingIdWarnsAndContinues(t *testing.T) {
	cfg, _ := newTestDB(t)
	// No top-level id => identity cannot be verified, but it should continue.
	path := writeExportFile(t, `{"name":"NoID","messages":[{"id":1,"type":"message"}]}`)

	err := runPruneDeleted(context.Background(), cfg, path, -1000123456789, false)
	if err != nil {
		t.Fatalf("expected success (warning, not error), got %v", err)
	}
}

func TestRunPruneDeleted_HappyPathMatchingID(t *testing.T) {
	cfg, database := newTestDB(t)
	const groupID = int64(-1000123456789)

	// Two stored messages; only id 1 survives in the export, so id 2 is deleted.
	ctx := context.Background()
	for _, tgID := range []int64{1, 2} {
		if err := database.AddMessage(ctx, &db.Message{
			GroupID:     groupID,
			UserHash:    "abcd1234",
			Text:        "msg",
			Timestamp:   time.Now(),
			TgMessageID: tgID,
		}); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
	}

	// Export id 123456789 -> Bot API -1000123456789, matching groupID.
	path := writeExportFile(t, `{"name":"Chat","id":123456789,"messages":[{"id":1,"type":"message"}]}`)

	// Dry run: must succeed without deleting.
	if err := runPruneDeleted(ctx, cfg, path, groupID, false); err != nil {
		t.Fatalf("dry-run runPruneDeleted: %v", err)
	}
	stored, err := database.GetMessagesInTgIDRange(ctx, groupID, 1, 2)
	if err != nil {
		t.Fatalf("GetMessagesInTgIDRange: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("dry-run should not delete; stored = %d, want 2", len(stored))
	}
}

func TestParseExport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "export.json")
	content := `{
		"name": "Chat",
		"messages": [
			{"id": 200, "type": "message", "text": "a"},
			{"id": 0, "type": "service", "action": "pin_message"},
			{"id": 205, "type": "message", "text": "b"},
			{"id": 202, "type": "message", "text": "c"}
		]
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write export: %v", err)
	}

	live, minID, maxID, export, err := parseExport(path)
	if err != nil {
		t.Fatalf("parseExport: %v", err)
	}
	if minID != 200 || maxID != 205 {
		t.Errorf("range = [%d..%d], want [200..205]", minID, maxID)
	}
	if len(live) != 3 {
		t.Errorf("live count = %d, want 3 (id 0 skipped)", len(live))
	}
	if !live[200] || !live[202] || !live[205] || live[0] {
		t.Errorf("unexpected live set: %v", live)
	}
	if export.Name != "Chat" {
		t.Errorf("export name = %q, want %q", export.Name, "Chat")
	}
	if export.ID != 0 {
		t.Errorf("export id = %d, want 0 (absent)", export.ID)
	}
}

func TestParseExportReadsChatIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "export.json")
	content := `{
		"name": "My Supergroup",
		"id": 123456789,
		"messages": [
			{"id": 1, "type": "message", "text": "a"}
		]
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write export: %v", err)
	}
	_, _, _, export, err := parseExport(path)
	if err != nil {
		t.Fatalf("parseExport: %v", err)
	}
	if export.ID != 123456789 {
		t.Errorf("export id = %d, want 123456789", export.ID)
	}
	if export.Name != "My Supergroup" {
		t.Errorf("export name = %q, want %q", export.Name, "My Supergroup")
	}
}

func TestBotAPIGroupID(t *testing.T) {
	if got := botAPIGroupID(123456789); got != -1000123456789 {
		t.Errorf("botAPIGroupID(123456789) = %d, want -1000123456789", got)
	}
}

func liveSet(ids ...int64) map[int64]bool {
	m := make(map[int64]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func tgIDs(msgs []db.Message) []int64 {
	out := make([]int64, len(msgs))
	for i, m := range msgs {
		out[i] = m.TgMessageID
	}
	return out
}

func TestSelectDeletedMessages_AllAlive(t *testing.T) {
	stored := []db.Message{
		{ID: 1, TgMessageID: 100},
		{ID: 2, TgMessageID: 101},
		{ID: 3, TgMessageID: 102},
	}
	got := selectDeletedMessages(liveSet(100, 101, 102), stored)
	if len(got) != 0 {
		t.Fatalf("expected no deletions, got %v", tgIDs(got))
	}
}

func TestSelectDeletedMessages_SomeMissing(t *testing.T) {
	stored := []db.Message{
		{ID: 1, TgMessageID: 100},
		{ID: 2, TgMessageID: 101}, // deleted in Telegram
		{ID: 3, TgMessageID: 102},
		{ID: 4, TgMessageID: 103}, // deleted in Telegram
	}
	got := selectDeletedMessages(liveSet(100, 102), stored)
	want := []int64{101, 103}
	if gotIDs := tgIDs(got); len(gotIDs) != len(want) || gotIDs[0] != want[0] || gotIDs[1] != want[1] {
		t.Fatalf("expected deleted %v, got %v", want, gotIDs)
	}
}

func TestSelectDeletedMessages_SkipsNullTgID(t *testing.T) {
	stored := []db.Message{
		{ID: 1, TgMessageID: 0}, // no Telegram id — cannot reconcile, must be skipped
		{ID: 2, TgMessageID: 101},
	}
	got := selectDeletedMessages(liveSet(101), stored)
	if len(got) != 0 {
		t.Fatalf("expected message without tg_message_id to be skipped, got %v", tgIDs(got))
	}
}
