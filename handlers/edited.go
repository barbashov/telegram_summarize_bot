package handlers

import (
	"context"

	"telegram_summarize_bot/logger"

	"github.com/mymmrac/telego"
)

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
		logger.Debug().
			Int64("group_id", msg.Chat.ID).
			Int64("tg_message_id", tgMessageID).
			Msg("edited message not found, skipped")
	}
}
