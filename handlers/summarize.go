package handlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
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
			b.sendMessage(groupID, "Неверный формат. Используйте: @bot summarize [часы]\nПример: @bot summarize 12")
			return
		}
		if parsed > b.cfg.SummaryHours {
			b.sendMessage(groupID, fmt.Sprintf("Максимальный период суммаризации — %d часов.", b.cfg.SummaryHours))
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
		b.sendMessage(groupID, "Ошибка получения сообщений.")
		return
	}

	if len(messages) == 0 {
		if lastSummarize != nil {
			b.sendMessage(groupID, "Нет новых сообщений с последней суммаризации.")
		} else {
			b.sendMessage(groupID, "Нет сообщений за последние 24 часа.")
		}
		return
	}

	if !b.rateLimiter.Allow(groupID) {
		b.metrics.RateLimit.Record(0)
		remaining := b.rateLimiter.RemainingTime(groupID)
		b.sendMessage(groupID, "Подождите "+formatDuration(remaining)+" перед следующим запросом суммаризации.")
		return
	}

	committed := false
	defer func() {
		if !committed {
			b.rateLimiter.Release(groupID)
		}
	}()

	logger.Info().Int("count", len(messages)).Msg("Summarizing messages")

	statusMsgID := b.sendMessage(groupID, fmt.Sprintf("Собираю сообщения за последние %d часов...", hours))

	summary, err := b.summarizer.SummarizeByTopics(ctx, messages, b.cfg.TopicMax)
	if err != nil {
		logger.Error().Err(err).Msg("failed to summarize")
		b.editWithRetry(groupID, statusMsgID, "Ошибка суммаризации. Попробуйте позже.")
		return
	}

	if !b.sendSummary(groupID, statusMsgID, summary) {
		return
	}

	committed = true

	if err := b.db.SetLastSummarizeTime(ctx, groupID, upperBound); err != nil {
		logger.Error().Err(err).Msg("failed to set last summarize time")
	}
}

func (b *Bot) sendSummary(chatID, statusMsgID int64, summary *summarizer.StructuredSummary) bool {
	chunks := splitTelegramMessage(summarizer.FormatTelegramSummary(summary, chatID), telegramMessageLimit)
	if len(chunks) == 0 {
		chunks = []string{"📝 *Суммаризация:*\n\nНет данных для суммаризации\\."}
	}

	if err := b.editFormattedFinal(chatID, statusMsgID, chunks[0]); err != nil {
		logger.Error().Err(err).Int64("chat_id", chatID).Msg("failed to send summary to Telegram")
		return false
	}
	for _, chunk := range chunks[1:] {
		b.sendFormatted(chatID, chunk)
	}
	return true
}

func splitTelegramMessage(text string, limit int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if limit <= 0 || len(text) <= limit {
		return []string{text}
	}

	lines := strings.Split(text, "\n")
	var chunks []string
	var current strings.Builder

	appendChunk := func() {
		chunk := strings.TrimSpace(current.String())
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
		current.Reset()
	}

	for _, line := range lines {
		line = strings.TrimRight(line, " ")
		candidateLen := len(line)
		if current.Len() > 0 {
			candidateLen = current.Len() + 1 + len(line)
		}

		if candidateLen <= limit {
			if current.Len() > 0 {
				current.WriteString("\n")
			}
			current.WriteString(line)
			continue
		}

		if current.Len() > 0 {
			appendChunk()
		}

		for len(line) > limit {
			chunks = append(chunks, strings.TrimSpace(line[:limit]))
			line = line[limit:]
		}
		if strings.TrimSpace(line) != "" {
			current.WriteString(line)
		}
	}

	appendChunk()
	return chunks
}
