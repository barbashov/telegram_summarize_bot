package handlers

import (
	"context"
	"time"

	"telegram_summarize_bot/logger"

	telegramify "github.com/barbashov/telegramify-markdown-go"
	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

// renderMarkdown converts a Markdown string to Telegram MarkdownV2 and splits it
// into sendable chunks (respecting Telegram's length limit). Used for all
// LLM/summary output so **bold**, lists and links render instead of leaking as
// literal markers.
func renderMarkdown(md string) []string {
	return telegramify.Split(telegramify.Markdownify(md), telegramMessageLimit)
}

const (
	editRetries    = 3
	editRetryDelay = 2 * time.Second
)

func (b *Bot) sendMessage(ctx context.Context, chatID int64, text string) int64 {
	defer b.metrics.TelegramSend.Start()()
	msg, err := b.telegram.SendMessage(ctx, tu.Message(
		tu.ID(chatID),
		text,
	))
	if err != nil {
		logger.Error().Err(err).Int64("chat_id", chatID).Msg("failed to send message")
		b.metrics.RecordError("telegram_send", err.Error())
		return 0
	}
	return int64(msg.MessageID)
}

// sendMessageReply sends a plain-text message as a reply to replyToMsgID and
// returns the new message's ID (0 on failure). Editing this message later
// preserves the reply linkage, so a status message sent this way keeps the
// final summary visually attached to the post it summarizes.
func (b *Bot) sendMessageReply(ctx context.Context, chatID, replyToMsgID int64, text string) int64 {
	defer b.metrics.TelegramSend.Start()()
	msg, err := b.telegram.SendMessage(ctx, tu.Message(
		tu.ID(chatID),
		text,
	).WithReplyParameters(&telego.ReplyParameters{MessageID: int(replyToMsgID)}))
	if err != nil {
		logger.Error().Err(err).Int64("chat_id", chatID).Msg("failed to send reply message")
		b.metrics.RecordError("telegram_send", err.Error())
		return 0
	}
	return int64(msg.MessageID)
}

func (b *Bot) editWithRetry(ctx context.Context, chatID, msgID int64, text string) {
	// A zero msgID means the status message was never sent (send failed), so
	// editing it can never succeed — send a new message instead so the result
	// still reaches the chat.
	if msgID == 0 {
		b.sendMessage(ctx, chatID, text)
		return
	}
	for range editRetries {
		if err := b.editMessage(ctx, chatID, msgID, text); err == nil {
			return
		}
		if !sleepCtx(ctx, editRetryDelay) {
			return
		}
	}
}

func (b *Bot) editMessage(ctx context.Context, chatID, messageID int64, text string) error {
	defer b.metrics.TelegramEdit.Start()()
	_, err := b.telegram.EditMessageText(ctx, &telego.EditMessageTextParams{
		ChatID:    tu.ID(chatID),
		MessageID: int(messageID),
		Text:      text,
	})
	if err != nil {
		logger.Error().Err(err).Int64("chat_id", chatID).Int64("message_id", messageID).Msg("failed to edit message")
		b.metrics.RecordError("telegram_edit", err.Error())
	}
	return err
}

func (b *Bot) sendFormatted(ctx context.Context, chatID int64, text string) {
	defer b.metrics.TelegramSend.Start()()
	_, err := b.telegram.SendMessage(ctx, tu.Message(
		tu.ID(chatID),
		text,
	).WithParseMode("MarkdownV2"))
	if err != nil {
		logger.Error().Err(err).Int64("chat_id", chatID).Msg("failed to send formatted message")
		b.metrics.RecordError("telegram_send", err.Error())
	}
}

func (b *Bot) editFormatted(ctx context.Context, chatID, messageID int64, text string) error {
	defer b.metrics.TelegramEdit.Start()()
	_, err := b.telegram.EditMessageText(ctx, &telego.EditMessageTextParams{
		ChatID:    tu.ID(chatID),
		MessageID: int(messageID),
		Text:      text,
		ParseMode: "MarkdownV2",
	})
	if err != nil {
		logger.Error().Err(err).Int64("chat_id", chatID).Int64("message_id", messageID).Msg("failed to edit formatted message")
		b.metrics.RecordError("telegram_edit", err.Error())
	}
	return err
}

func (b *Bot) editFormattedWithRetry(ctx context.Context, chatID, msgID int64, text string) {
	// See editWithRetry: a zero msgID can never be edited; send instead.
	if msgID == 0 {
		b.sendFormatted(ctx, chatID, text)
		return
	}
	for range editRetries {
		if err := b.editFormatted(ctx, chatID, msgID, text); err == nil {
			return
		}
		if !sleepCtx(ctx, editRetryDelay) {
			return
		}
	}
}

// editFormattedFinal is like editFormattedWithRetry but returns the last error
// so the caller can decide whether the delivery succeeded.
func (b *Bot) editFormattedFinal(ctx context.Context, chatID, msgID int64, text string) error {
	// A zero msgID means the status message was never sent; editing it can never
	// succeed and would silently drop the (already paid-for) summary. Send a new
	// message instead. sendFormatted is best-effort and swallows its own error,
	// matching the fire-and-forget delivery of the remaining chunks; report
	// success so the caller proceeds (e.g. commits the rate-limit slot).
	if msgID == 0 {
		b.sendFormatted(ctx, chatID, text)
		return nil
	}
	var lastErr error
	for range editRetries {
		if lastErr = b.editFormatted(ctx, chatID, msgID, text); lastErr == nil {
			return nil
		}
		if !sleepCtx(ctx, editRetryDelay) {
			// lastErr is non-nil here: the lastErr == nil case returned above.
			return lastErr
		}
	}
	return lastErr
}

// sleepCtx waits for d to elapse or ctx to be cancelled. Returns true on
// timer completion, false on cancellation.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (b *Bot) NotifyUsers(ctx context.Context, text string) (sent, failed int) {
	attempted := len(b.cfg.AdminUserIDs)
	if attempted == 0 {
		return 0, 0
	}

	for _, userID := range b.cfg.AdminUserIDs {
		if b.sendMessage(ctx, userID, text) == 0 {
			failed++
			continue
		}
		sent++
	}

	logger.Info().
		Int("attempted", attempted).
		Int("sent", sent).
		Int("failed", failed).
		Msg("alert notifications sent")

	return sent, failed
}
