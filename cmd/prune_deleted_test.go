package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"telegram_summarize_bot/db"
)

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

	live, minID, maxID, err := parseExport(path)
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
