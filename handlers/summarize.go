package handlers

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"telegram_summarize_bot/db"
	"time"

	"telegram_summarize_bot/logger"
	"telegram_summarize_bot/summarizer"
	"telegram_summarize_bot/tgutil"

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

	if _, busy := b.manualSummaries.LoadOrStore(groupID, struct{}{}); busy {
		b.sendMessage(ctx, groupID, "Сводка уже готовится.")
		return
	}
	defer b.manualSummaries.Delete(groupID)
	key := fmt.Sprintf("manual:%d", groupID)
	pending, err := b.db.GetDelivery(ctx, key)
	if err != nil {
		b.sendMessage(ctx, groupID, "Ошибка получения сообщений.")
		return
	}
	if pending != nil {
		b.resumeDelivery(ctx, key)
		return
	}
	since := time.Now().Add(-time.Duration(hours) * time.Hour)
	messages, err := b.db.GetSummaryBatch(ctx, groupID, since, b.cfg.MaxMessages+1)
	if err != nil {
		logger.Error().Err(err).Msg("failed to get messages")
		b.sendMessage(ctx, groupID, "Ошибка получения сообщений.")
		return
	}
	if len(messages) == 0 {
		b.sendMessage(ctx, groupID, fmt.Sprintf("Нет новых сообщений за последние %d ч.", hours))
		return
	}
	truncated := len(messages) > b.cfg.MaxMessages
	if truncated {
		messages = messages[:b.cfg.MaxMessages]
	}
	cursor := messages[len(messages)-1].ID
	sort.SliceStable(messages, func(i, j int) bool { return messages[i].Timestamp.Before(messages[j].Timestamp) })

	allowed, grant := b.rateLimiter.Allow(groupID)
	if !allowed {
		b.metrics.RateLimit.Record(0)
		remaining := b.rateLimiter.RemainingTime(groupID)
		b.sendMessage(ctx, groupID, "Подождите "+tgutil.FormatDuration(remaining)+" перед следующим запросом суммаризации.")
		return
	}

	committed := false
	defer func() {
		if !committed {
			b.rateLimiter.Release(groupID, grant)
		}
	}()

	logger.Info().Int("count", len(messages)).Msg("Summarizing messages")

	statusMsgID := b.sendMessage(ctx, groupID, fmt.Sprintf("Собираю сообщения за последние %d часов...", hours))

	instructions := b.loadGroupSummaryInstructions(ctx, groupID)
	summary, err := b.summarizer.SummarizeByTopics(ctx, messages, b.cfg.TopicMax, instructions)
	if err != nil {
		_ = b.db.InsertBotEvent(ctx, "summary_failed", time.Now(), 0)
		logger.Error().Err(err).Msg("failed to summarize")
		b.editWithRetry(ctx, groupID, statusMsgID, "Ошибка суммаризации. Попробуйте позже.")
		return
	}

	raw := summarizer.FormatTelegramSummary(summary, groupID)
	if truncated {
		raw += fmt.Sprintf("\n\n⚠️ В периоде больше %d сообщений. Запустите суммаризацию ещё раз для продолжения.", b.cfg.MaxMessages)
	}
	checkpoint := messages[len(messages)-1].Timestamp
	committed = b.deliver(ctx, db.Delivery{Key: key, ChatID: groupID, StatusID: statusMsgID,
		Group: true, CursorID: cursor, Checkpoint: checkpoint}, raw)
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
