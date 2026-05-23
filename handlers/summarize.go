package handlers

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"telegram_summarize_bot/logger"
	"telegram_summarize_bot/summarizer"

	"github.com/mymmrac/telego"
)

func (b *Bot) handleSummarize(ctx context.Context, update telego.Update, args []string) {
	msg := update.Message
	groupID := msg.Chat.ID

	hours := b.cfg.SummaryHours
	if len(args) > 0 {
		parsed, err := strconv.Atoi(args[0])
		if err != nil || parsed <= 0 {
			b.sendMessage(ctx, groupID, "Неверный формат. Используйте: @bot summarize [часы]\nПример: @bot summarize 12")
			return
		}
		if parsed > b.cfg.SummaryHours {
			b.sendMessage(ctx, groupID, fmt.Sprintf("Максимальный период суммаризации — %d часов.", b.cfg.SummaryHours))
			return
		}
		hours = parsed
	}

	lastSummarize, err := b.db.GetLastSummarizeTime(ctx, groupID)
	if err != nil {
		logger.Error().Err(err).Msg("failed to get last summarize time")
	}

	since := time.Now().Add(-time.Duration(hours) * time.Hour)
	if lastSummarize != nil && since.Before(*lastSummarize) {
		since = *lastSummarize
	}
	upperBound := time.Now()

	messages, err := b.db.GetMessages(ctx, groupID, since, b.cfg.MaxMessages)
	if err != nil {
		logger.Error().Err(err).Msg("failed to get messages")
		b.sendMessage(ctx, groupID, "Ошибка получения сообщений.")
		return
	}

	if len(messages) == 0 {
		if lastSummarize != nil {
			b.sendMessage(ctx, groupID, "Нет новых сообщений с последней суммаризации.")
		} else {
			b.sendMessage(ctx, groupID, "Нет сообщений за последние 24 часа.")
		}
		return
	}

	if !b.rateLimiter.Allow(groupID) {
		b.metrics.RateLimit.Record(0)
		remaining := b.rateLimiter.RemainingTime(groupID)
		b.sendMessage(ctx, groupID, "Подождите "+formatDuration(remaining)+" перед следующим запросом суммаризации.")
		return
	}

	committed := false
	defer func() {
		if !committed {
			b.rateLimiter.Release(groupID)
		}
	}()

	logger.Info().Int("count", len(messages)).Msg("Summarizing messages")

	statusMsgID := b.sendMessage(ctx, groupID, fmt.Sprintf("Собираю сообщения за последние %d часов...", hours))

	instructions := b.loadGroupSummaryInstructions(ctx, groupID)
	summary, err := b.summarizer.SummarizeByTopics(ctx, messages, b.cfg.TopicMax, instructions)
	if err != nil {
		logger.Error().Err(err).Msg("failed to summarize")
		b.editWithRetry(ctx, groupID, statusMsgID, "Ошибка суммаризации. Попробуйте позже.")
		return
	}

	if !b.sendSummary(ctx, groupID, statusMsgID, summary) {
		return
	}

	committed = true

	if err := b.db.SetLastSummarizeTime(ctx, groupID, upperBound); err != nil {
		logger.Error().Err(err).Msg("failed to set last summarize time")
	}
}

func (b *Bot) loadGroupSummaryInstructions(ctx context.Context, groupID int64) string {
	item, err := b.db.GetGroupSummaryInstructions(ctx, groupID)
	if err != nil {
		logger.Error().Err(err).Int64("group_id", groupID).Msg("failed to get group summary instructions")
		return ""
	}
	if item == nil {
		return ""
	}
	return item.Instructions
}

func (b *Bot) sendSummary(ctx context.Context, chatID, statusMsgID int64, summary *summarizer.StructuredSummary) bool {
	chunks := renderMarkdown(summarizer.FormatTelegramSummary(summary, chatID))
	if len(chunks) == 0 {
		chunks = renderMarkdown("📝 **Суммаризация:**\n\nНет данных для суммаризации.")
	}

	if err := b.editFormattedFinal(ctx, chatID, statusMsgID, chunks[0]); err != nil {
		logger.Error().Err(err).Int64("chat_id", chatID).Msg("failed to send summary to Telegram")
		return false
	}
	for _, chunk := range chunks[1:] {
		b.sendFormatted(ctx, chatID, chunk)
	}
	return true
}
