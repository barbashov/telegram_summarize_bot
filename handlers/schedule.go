package handlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"telegram_summarize_bot/db"
	"telegram_summarize_bot/logger"
	"telegram_summarize_bot/summarizer"

	"github.com/mymmrac/telego"
)

func (b *Bot) handleSchedule(ctx context.Context, update telego.Update, args []string) {
	msg := update.Message
	groupID := msg.Chat.ID

	if len(args) == 0 {
		// Show current schedule to anyone.
		s, err := b.db.GetGroupSchedule(ctx, groupID)
		if err != nil {
			logger.Error().Err(err).Msg("failed to get group schedule")
			b.sendMessage(groupID, "Ошибка получения расписания.")
			return
		}
		if s == nil || !s.Enabled {
			b.sendFormatted(groupID, "⏰ Ежедневная сводка *отключена*\\.")
		} else {
			b.sendFormatted(groupID, fmt.Sprintf("⏰ Ежедневная сводка *включена*, время: *%02d:%02d UTC*\\.", s.Hour, s.Minute))
		}
		return
	}

	// Mutating operations require admin privileges.
	if !b.isGroupAdmin(groupID, msg.From.ID) {
		b.sendMessage(groupID, "Только администраторы группы могут изменять расписание.")
		return
	}

	arg := strings.ToLower(args[0])

	// "now" triggers an immediate unscheduled summary.
	if arg == "now" {
		b.sendFormatted(groupID, "🔄 Запускаю внеплановую сводку\\.\\.\\.")
		b.runScheduledSummary(ctx, groupID, time.Now())
		return
	}

	// Validate HH:MM format early (before DB fetch) so we can return fast on bad input.
	var parsedHour, parsedMinute int
	isTime := false
	if arg != "on" && arg != "off" {
		parts := strings.SplitN(arg, ":", 2)
		if len(parts) != 2 {
			b.sendFormatted(groupID, "Неверный формат\\. Используйте: `schedule on`, `schedule off`, `schedule now` или `schedule ЧЧ:ММ`\\.")
			return
		}
		h, err1 := strconv.Atoi(parts[0])
		m, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			b.sendFormatted(groupID, "Неверное время\\. Используйте формат ЧЧ:ММ, например `07:00`\\.")
			return
		}
		parsedHour, parsedMinute, isTime = h, m, true
	}

	// Get or create the schedule record.
	s, err := b.db.GetGroupSchedule(ctx, groupID)
	if err != nil {
		logger.Error().Err(err).Msg("failed to get group schedule")
		b.sendMessage(groupID, "Ошибка получения расписания.")
		return
	}
	if s == nil {
		s = &db.GroupSchedule{GroupID: groupID, Hour: b.cfg.DailySummaryHour}
	}

	// Mutate schedule based on subcommand.
	switch {
	case arg == "off":
		s.Enabled = false
	case arg == "on":
		s.Enabled = true
	case isTime:
		s.Enabled = true
		s.Hour = parsedHour
		s.Minute = parsedMinute
	}

	if err := b.db.SetGroupSchedule(ctx, s); err != nil {
		logger.Error().Err(err).Msg("failed to set group schedule")
		b.sendMessage(groupID, "Ошибка сохранения расписания.")
		return
	}

	if s.Enabled {
		b.sendFormatted(groupID, fmt.Sprintf("⏰ Ежедневная сводка *включена*, время: *%02d:%02d UTC*\\.", s.Hour, s.Minute))
	} else {
		b.sendFormatted(groupID, "⏰ Ежедневная сводка *отключена*\\.")
	}
}

func (b *Bot) schedulerLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			now = now.UTC()
			schedules, err := b.db.GetEnabledSchedules(ctx)
			if err != nil {
				logger.Error().Err(err).Msg("failed to get enabled schedules")
				continue
			}
			for _, s := range schedules {
				if s.Hour != now.Hour() || s.Minute != now.Minute() {
					continue
				}
				today := now.Truncate(24 * time.Hour)
				if s.LastDailySummary != nil && !s.LastDailySummary.UTC().Truncate(24*time.Hour).Before(today) {
					continue
				}
				groupID := s.GroupID
				go b.runScheduledSummary(ctx, groupID, now)
			}
		}
	}
}

func (b *Bot) runScheduledSummary(ctx context.Context, groupID int64, now time.Time) {
	since := now.UTC().Add(-24 * time.Hour)
	messages, err := b.db.GetMessages(ctx, groupID, since, b.cfg.MaxMessages)
	if err != nil {
		logger.Error().Err(err).Int64("group_id", groupID).Msg("scheduled summary: failed to get messages")
		return
	}
	if len(messages) == 0 {
		logger.Info().Int64("group_id", groupID).Msg("scheduled summary: no messages, skipping")
		return
	}

	logger.Info().Int64("group_id", groupID).Int("count", len(messages)).Msg("running scheduled summary")

	summary, err := b.summarizer.SummarizeByTopics(ctx, messages, b.cfg.TopicMax)
	if err != nil {
		logger.Error().Err(err).Int64("group_id", groupID).Msg("scheduled summary: failed to summarize")
		return
	}

	preamble := "🌅 *Утренняя \\#сводка за последние 24 часа:*"
	chunks := splitTelegramMessage(summarizer.FormatTelegramSummary(summary, groupID), telegramMessageLimit)
	if len(chunks) == 0 {
		return
	}
	b.sendFormatted(groupID, preamble+"\n\n"+chunks[0])
	for _, chunk := range chunks[1:] {
		b.sendFormatted(groupID, chunk)
	}

	if err := b.db.UpdateLastDailySummary(ctx, groupID, now); err != nil {
		logger.Error().Err(err).Int64("group_id", groupID).Msg("failed to update last daily summary")
	}
}
