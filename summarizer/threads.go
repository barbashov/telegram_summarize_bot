package summarizer

import "telegram_summarize_bot/db"

// maxAncestryDepth is the hard ceiling on how many messages a reply-chain walk
// will include, regardless of the requested depth. It bounds prompt size and
// the number of DB lookups for pathological threads.
const maxAncestryDepth = 25

// AncestorLookup resolves the message with the given Telegram message_id, or
// returns (nil, nil) when it cannot be resolved (retention-pruned, outside the
// batch, or never ingested). A non-nil error aborts the walk.
type AncestorLookup func(tgMessageID int64) (*db.Message, error)

// WalkAncestry returns the reply chain ending at target, ordered root→target
// (target is always the last element, so the result is never empty). It follows
// target.ReplyToTgID via lookup, stopping at the root (ReplyToTgID == 0), a
// lookup miss, a cycle, or once the chain reaches maxDepth messages. maxDepth is
// clamped to (0, maxAncestryDepth]; values <= 0 use the ceiling.
func WalkAncestry(target db.Message, lookup AncestorLookup, maxDepth int) ([]db.Message, error) {
	if maxDepth <= 0 || maxDepth > maxAncestryDepth {
		maxDepth = maxAncestryDepth
	}

	chain := []db.Message{target}
	seen := map[int64]bool{}
	if target.TgMessageID != 0 {
		seen[target.TgMessageID] = true
	}

	cur := target
	for cur.ReplyToTgID != 0 && len(chain) < maxDepth {
		if seen[cur.ReplyToTgID] { // cycle
			break
		}
		parent, err := lookup(cur.ReplyToTgID)
		if err != nil {
			return nil, err
		}
		if parent == nil { // pruned / missing: stop, return what we have
			break
		}
		if parent.TgMessageID != 0 {
			seen[parent.TgMessageID] = true
		}
		chain = append(chain, *parent)
		cur = *parent
	}

	// chain is target→…→root; reverse to root→…→target.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}
