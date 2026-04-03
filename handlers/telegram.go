package handlers

import (
	"context"

	"telegram_summarize_bot/logger"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

func (b *Bot) sendMessage(chatID int64, text string) int64 {
	defer b.metrics.TelegramSend.Start()()
	msg, err := b.telegram.SendMessage(tu.Message(
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

func (b *Bot) editOrSend(chatID, msgID int64, text string) {
	if editErr := b.editMessage(chatID, msgID, text); editErr != nil {
		b.sendMessage(chatID, text)
	}
}

func (b *Bot) editMessage(chatID, messageID int64, text string) error {
	defer b.metrics.TelegramEdit.Start()()
	_, err := b.telegram.EditMessageText(&telego.EditMessageTextParams{
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

func (b *Bot) sendFormatted(chatID int64, text string) {
	defer b.metrics.TelegramSend.Start()()
	_, err := b.telegram.SendMessage(tu.Message(
		tu.ID(chatID),
		text,
	).WithParseMode("MarkdownV2"))
	if err != nil {
		logger.Error().Err(err).Int64("chat_id", chatID).Msg("failed to send formatted message")
		b.metrics.RecordError("telegram_send", err.Error())
	}
}

func (b *Bot) editFormatted(chatID, messageID int64, text string) error {
	defer b.metrics.TelegramEdit.Start()()
	_, err := b.telegram.EditMessageText(&telego.EditMessageTextParams{
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

func (b *Bot) editOrSendFormatted(chatID, msgID int64, text string) {
	if editErr := b.editFormatted(chatID, msgID, text); editErr != nil {
		b.sendFormatted(chatID, text)
	}
}

func (b *Bot) NotifyUsers(ctx context.Context, text string) (sent, failed int) {
	attempted := len(b.cfg.AdminUserIDs)
	if attempted == 0 {
		return 0, 0
	}

	for _, userID := range b.cfg.AdminUserIDs {
		if b.sendMessage(userID, text) == 0 {
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
