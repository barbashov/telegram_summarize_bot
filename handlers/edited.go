package handlers

import (
	"context"
	"time"

	"telegram_summarize_bot/logger"

	"github.com/mymmrac/telego"
)

// editedMsgRetryDelay is how long to wait before retrying an edit whose
// original message row wasn't found yet. A var so tests can shorten it.
var editedMsgRetryDelay = 1 * time.Second

// handleEditedMessage reflects a Telegram message edit by overwriting the
// stored text of the existing row (matched on group_id + tg_message_id) so any
// later summarization uses the current version. Unlike new messages, edits are
// never parsed as commands and never insert a row: only already-stored messages
// are updated, which inherently limits us to allowed groups (only their
// messages are kept). Photos are left untouched — editing a caption does not
// change the photo identity, and vision descriptions are cached by file_unique_id.
func (b *Bot) handleEditedMessage(ctx context.Context, msg *telego.Message) {
	if msg == nil || msg.From == nil {
		return
	}
	if msg.Chat.Type == "private" {
		// Private messages are not stored as group messages.
		return
	}

	// Media messages carry their text in msg.Caption, not msg.Text.
	text := msg.Text
	if text == "" {
		text = msg.Caption
	}
	tgMessageID := int64(msg.MessageID)

	updated, err := b.db.UpdateMessageText(ctx, msg.Chat.ID, tgMessageID, text)
	if err != nil {
		logger.Error().Err(err).
			Int64("group_id", msg.Chat.ID).
			Int64("tg_message_id", tgMessageID).
			Msg("failed to update edited message text")
		return
	}
	if !updated {
		// Updates are dispatched in parallel, so an edit can race ahead of its
		// original message's insert (systematic during backlog replay). Give
		// the insert a moment and retry once before giving up.
		if !sleepCtx(ctx, editedMsgRetryDelay) {
			return
		}
		updated, err = b.db.UpdateMessageText(ctx, msg.Chat.ID, tgMessageID, text)
		if err != nil {
			logger.Error().Err(err).
				Int64("group_id", msg.Chat.ID).
				Int64("tg_message_id", tgMessageID).
				Msg("failed to update edited message text on retry")
			return
		}
		if !updated {
			logger.Debug().
				Int64("group_id", msg.Chat.ID).
				Int64("tg_message_id", tgMessageID).
				Msg("edited message not found, skipped")
		}
	}
}
