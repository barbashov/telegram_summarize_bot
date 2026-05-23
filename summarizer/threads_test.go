package summarizer

import (
	"errors"
	"testing"

	"telegram_summarize_bot/db"
)

// mapLookup builds an AncestorLookup over an in-memory set of messages keyed by
// TgMessageID, mirroring how the 24h path looks up within its batch.
func mapLookup(msgs ...db.Message) AncestorLookup {
	byID := map[int64]db.Message{}
	for _, m := range msgs {
		byID[m.TgMessageID] = m
	}
	return func(tgID int64) (*db.Message, error) {
		if m, ok := byID[tgID]; ok {
			return &m, nil
		}
		return nil, nil
	}
}

func ids(chain []db.Message) []int64 {
	out := make([]int64, len(chain))
	for i, m := range chain {
		out[i] = m.TgMessageID
	}
	return out
}

func TestWalkAncestryLinear(t *testing.T) {
	// 1 ← 2 ← 3 (3 is the target/leaf)
	m1 := db.Message{TgMessageID: 1}
	m2 := db.Message{TgMessageID: 2, ReplyToTgID: 1}
	m3 := db.Message{TgMessageID: 3, ReplyToTgID: 2}

	chain, err := WalkAncestry(m3, mapLookup(m1, m2, m3), 25)
	if err != nil {
		t.Fatalf("WalkAncestry: %v", err)
	}
	got := ids(chain)
	want := []int64{1, 2, 3}
	if len(got) != 3 || got[0] != want[0] || got[2] != want[2] {
		t.Fatalf("chain = %v, want root→target %v", got, want)
	}
}

func TestWalkAncestryDepthCap(t *testing.T) {
	m1 := db.Message{TgMessageID: 1}
	m2 := db.Message{TgMessageID: 2, ReplyToTgID: 1}
	m3 := db.Message{TgMessageID: 3, ReplyToTgID: 2}

	chain, err := WalkAncestry(m3, mapLookup(m1, m2, m3), 2)
	if err != nil {
		t.Fatalf("WalkAncestry: %v", err)
	}
	// Capped to 2 messages, target last.
	if got := ids(chain); len(got) != 2 || got[1] != 3 || got[0] != 2 {
		t.Fatalf("capped chain = %v, want [2 3]", got)
	}
}

func TestWalkAncestryLengthOne(t *testing.T) {
	target := db.Message{TgMessageID: 7} // no parent
	chain, err := WalkAncestry(target, mapLookup(target), 25)
	if err != nil {
		t.Fatalf("WalkAncestry: %v", err)
	}
	if len(chain) != 1 || chain[0].TgMessageID != 7 {
		t.Fatalf("chain = %v, want [7]", ids(chain))
	}
}

func TestWalkAncestryMidChainMissReturnsPartial(t *testing.T) {
	// 3 replies to 2, but 2 is missing (pruned); only 2's row absent.
	m3 := db.Message{TgMessageID: 3, ReplyToTgID: 2}
	chain, err := WalkAncestry(m3, mapLookup(m3), 25) // lookup has only the target
	if err != nil {
		t.Fatalf("WalkAncestry: %v", err)
	}
	if got := ids(chain); len(got) != 1 || got[0] != 3 {
		t.Fatalf("partial chain = %v, want [3]", got)
	}
}

func TestWalkAncestryCycleSafe(t *testing.T) {
	// 1 ↔ 2 cycle.
	m1 := db.Message{TgMessageID: 1, ReplyToTgID: 2}
	m2 := db.Message{TgMessageID: 2, ReplyToTgID: 1}
	chain, err := WalkAncestry(m2, mapLookup(m1, m2), 25)
	if err != nil {
		t.Fatalf("WalkAncestry: %v", err)
	}
	// Walk: target 2 → parent 1 → would revisit 2 (seen) → stop. Bounded.
	if len(chain) != 2 {
		t.Fatalf("cycle chain length = %d, want 2 (%v)", len(chain), ids(chain))
	}
}

func TestWalkAncestryLookupError(t *testing.T) {
	boom := errors.New("db down")
	lookup := func(int64) (*db.Message, error) { return nil, boom }
	target := db.Message{TgMessageID: 3, ReplyToTgID: 2}
	if _, err := WalkAncestry(target, lookup, 25); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
}
