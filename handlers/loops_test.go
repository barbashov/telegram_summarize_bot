package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/mymmrac/telego"
)

// failingGetChatTelegram embeds fakeTelegram but makes GetChat fail, simulating
// a group the bot can no longer query (e.g. kicked, or transient API error).
type failingGetChatTelegram struct {
	fakeTelegram
}

func (f *failingGetChatTelegram) GetChat(_ context.Context, _ *telego.GetChatParams) (*telego.ChatFullInfo, error) {
	return nil, errors.New("forbidden")
}

// TestScanKnownGroupsSkipsUpsertOnGetChatFailure verifies that when GetChat
// fails, scanKnownGroups skips the upsert instead of blanking a previously good
// title (Fix 7).
func TestScanKnownGroupsSkipsUpsertOnGetChatFailure(t *testing.T) {
	b, database, _ := newTestBot(t, &fakeSummarizer{})
	defer func() { _ = database.Close() }()

	b.telegram = &failingGetChatTelegram{}

	ctx := context.Background()
	const groupID int64 = -100777

	// Seed a known group with a good title and mark it allowed so scan picks it up.
	if err := database.UpsertKnownGroup(ctx, groupID, "Good Title", "goodgroup"); err != nil {
		t.Fatalf("UpsertKnownGroup: %v", err)
	}
	if err := database.AddAllowedGroup(ctx, groupID, 0); err != nil {
		t.Fatalf("AddAllowedGroup: %v", err)
	}

	b.scanKnownGroups(ctx)

	groups, err := database.GetKnownGroups(ctx)
	if err != nil {
		t.Fatalf("GetKnownGroups: %v", err)
	}
	var found bool
	for _, g := range groups {
		if g.GroupID == groupID {
			found = true
			if g.Title != "Good Title" || g.Username != "goodgroup" {
				t.Fatalf("title clobbered on GetChat failure: %+v", g)
			}
		}
	}
	if !found {
		t.Fatal("expected known group to remain present")
	}
}
