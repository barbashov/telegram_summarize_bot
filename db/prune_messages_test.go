package db

import (
	"context"
	"testing"
	"time"
)

func TestGetMessagesInTgIDRangeAndDelete(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	now := time.Now()
	const group = int64(-100123456789)
	msgs := []*Message{
		{GroupID: group, UserHash: "aaaaaaaa", Text: "before range", Timestamp: now, TgMessageID: 100},
		{GroupID: group, UserHash: "bbbbbbbb", Text: "in range 1", Timestamp: now, TgMessageID: 200},
		{GroupID: group, UserHash: "cccccccc", Text: "in range 2", Timestamp: now, TgMessageID: 201},
		{GroupID: group, UserHash: "dddddddd", Text: "in range 3", Timestamp: now, TgMessageID: 202},
		{GroupID: group, UserHash: "eeeeeeee", Text: "after range", Timestamp: now, TgMessageID: 300},
		{GroupID: group, UserHash: "ffffffff", Text: "no tg id", Timestamp: now, TgMessageID: 0},
		{GroupID: -999, UserHash: "99999999", Text: "other group", Timestamp: now, TgMessageID: 201},
	}
	for _, m := range msgs {
		if err := db.AddMessage(ctx, m); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
	}

	got, err := db.GetMessagesInTgIDRange(ctx, group, 200, 202)
	if err != nil {
		t.Fatalf("GetMessagesInTgIDRange: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 messages in range, got %d", len(got))
	}
	// Ordered by tg_message_id, scoped to the group, excludes NULL/out-of-range/other-group.
	wantIDs := []int64{200, 201, 202}
	for i, m := range got {
		if m.TgMessageID != wantIDs[i] {
			t.Errorf("got[%d].TgMessageID = %d, want %d", i, m.TgMessageID, wantIDs[i])
		}
	}

	// Delete the two middle messages by internal id.
	ids := []int64{got[0].ID, got[2].ID}
	deleted, err := db.DeleteMessagesByIDs(ctx, ids)
	if err != nil {
		t.Fatalf("DeleteMessagesByIDs: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("expected 2 deleted, got %d", deleted)
	}

	after, err := db.GetMessagesInTgIDRange(ctx, group, 200, 202)
	if err != nil {
		t.Fatalf("GetMessagesInTgIDRange after delete: %v", err)
	}
	if len(after) != 1 || after[0].TgMessageID != 201 {
		t.Fatalf("expected only tg_message_id 201 to remain, got %+v", after)
	}
}

func TestDeleteMessagesByIDsEmpty(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	deleted, err := db.DeleteMessagesByIDs(ctx, nil)
	if err != nil {
		t.Fatalf("DeleteMessagesByIDs(nil): %v", err)
	}
	if deleted != 0 {
		t.Fatalf("expected 0 deleted, got %d", deleted)
	}
}
