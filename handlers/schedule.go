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
			b.sendMessage(ctx, groupID, "Ошибка получения расписания.")
			return
		}
		if s == nil || !s.Enabled {
			b.sendFormatted(ctx, groupID, "⏰ Ежедневная сводка *отключена*\\.")
		} else {
			b.sendFormatted(ctx, groupID, fmt.Sprintf("⏰ Ежедневная сводка *включена*, время: *%02d:%02d UTC*\\.", s.Hour, s.Minute))
		}
		return
	}

	// Mutating operations require admin privileges.
	if !b.isGroupAdmin(ctx, groupID, msg.From.ID) {
		b.sendMessage(ctx, groupID, "Только администраторы группы могут изменять расписание.")
		return
	}

	arg := strings.ToLower(args[0])

	// "now" triggers an immediate unscheduled summary. It must not stamp the
	// daily checkpoint, or the regular run would dedup against it and skip
	// that day's digest.
	if arg == "now" {
		b.sendFormatted(ctx, groupID, "🔄 Запускаю внеплановую сводку\\.\\.\\.")
		b.runScheduledSummary(ctx, groupID, time.Now(), true)
		return
	}

	// Validate HH:MM format early (before DB fetch) so we can return fast on bad input.
	var parsedHour, parsedMinute int
	isTime := false
	if arg != "on" && arg != "off" {
		parts := strings.SplitN(arg, ":", 2)
		if len(parts) != 2 {
			b.sendFormatted(ctx, groupID, "Неверный формат\\. Используйте: `schedule on`, `schedule off`, `schedule now` или `schedule ЧЧ:ММ`\\.")
			return
		}
		h, err1 := strconv.Atoi(parts[0])
		m, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			b.sendFormatted(ctx, groupID, "Неверное время\\. Используйте формат ЧЧ:ММ, например `07:00`\\.")
			return
		}
		parsedHour, parsedMinute, isTime = h, m, true
	}

	// Get or create the schedule record.
	s, err := b.db.GetGroupSchedule(ctx, groupID)
	if err != nil {
		logger.Error().Err(err).Msg("failed to get group schedule")
		b.sendMessage(ctx, groupID, "Ошибка получения расписания.")
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
		b.sendMessage(ctx, groupID, "Ошибка сохранения расписания.")
		return
	}

	if s.Enabled {
		b.sendFormatted(ctx, groupID, fmt.Sprintf("⏰ Ежедневная сводка *включена*, время: *%02d:%02d UTC*\\.", s.Hour, s.Minute))
	} else {
		b.sendFormatted(ctx, groupID, "⏰ Ежедневная сводка *отключена*\\.")
	}
}

// scheduleDue reports whether a schedule should fire at now (UTC): due today
// and not yet run today — rather than strict minute equality, so a dropped
// tick around HH:MM (slow DB call, host suspend, restart) can't skip the day.
func scheduleDue(s db.GroupSchedule, now time.Time) bool {
	schedTime := time.Date(now.Year(), now.Month(), now.Day(), s.Hour, s.Minute, 0, 0, time.UTC)
	if now.Before(schedTime) {
		return false
	}
	today := now.Truncate(24 * time.Hour)
	return s.LastDailySummary == nil || s.LastDailySummary.UTC().Truncate(24*time.Hour).Before(today)
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
				if !scheduleDue(s, now) {
					continue
				}
				groupID := s.GroupID
				// Same slot/drain discipline as the update loop: the semaphore
				// bounds the LLM burst when many groups share the default time, and
				// inflight lets shutdown drain these before closing the DB.
				select {
				case b.sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				b.inflight.Add(1)
				go func() {
					defer b.inflight.Done()
					defer func() { <-b.sem }()
					b.runScheduledSummary(ctx, groupID, now, false)
				}()
			}
		}
	}
}

// runScheduledSummary produces the 24h digest for one group. manual marks an
// admin-triggered "schedule now" run: it skips the daily checkpoint so the
// regular scheduled run still fires that day.
func (b *Bot) runScheduledSummary(ctx context.Context, groupID int64, now time.Time, manual bool) {
	// Re-check the allowlist: schedules are stored independently of
	// allowed_groups, so a group revoked via /groups remove would otherwise
	// keep receiving daily digests.
	allowed, err := b.db.IsGroupAllowed(ctx, groupID)
	if err != nil {
		logger.Error().Err(err).Int64("group_id", groupID).Msg("scheduled summary: allowlist check failed")
		return
	}
	if !allowed {
		logger.Info().Int64("group_id", groupID).Msg("scheduled summary: group no longer allowed, skipping")
		return
	}

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

	statusMsgID := b.sendMessage(ctx, groupID, "Готовлю утреннюю сводку...")

	instructions := b.loadGroupSummaryInstructions(ctx, groupID)
	summary, err := b.summarizer.SummarizeByTopics(ctx, messages, b.cfg.TopicMax, instructions)
	if err != nil {
		logger.Error().Err(err).Int64("group_id", groupID).Msg("scheduled summary: failed to summarize")
		b.editWithRetry(ctx, groupID, statusMsgID, "Ошибка суммаризации. Попробуйте позже.")
		return
	}

	preamble := "🌅 **Утренняя #сводка за последние 24 часа:**"
	raw := preamble + "\n\n" + summarizer.FormatTelegramSummary(summary, groupID)
	chunks := renderMarkdown(raw)
	if len(chunks) == 0 {
		return
	}
	if err := b.editFormattedFinal(ctx, groupID, statusMsgID, chunks[0]); err != nil {
		logger.Error().Err(err).Int64("group_id", groupID).Msg("scheduled summary: failed to send to Telegram")
		return
	}
	for _, chunk := range chunks[1:] {
		b.sendFormatted(ctx, groupID, chunk)
	}

	if manual {
		return
	}
	if err := b.db.UpdateLastDailySummary(ctx, groupID, now); err != nil {
		logger.Error().Err(err).Int64("group_id", groupID).Msg("failed to update last daily summary")
	}
}
