package handlers

import (
	"context"
	"time"

	"telegram_summarize_bot/logger"
	"telegram_summarize_bot/metrics"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

const metricsRetention = 30 * 24 * time.Hour

func (b *Bot) statsCacheLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.refreshStatsCache()
		}
	}
}

func (b *Bot) refreshStatsCache() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	since := time.Now().Add(-metricsRetention)
	metricNames := []string{"telegram_send", "telegram_edit", "llm_cluster", "llm_summarize", "db_add", "db_get"}

	latency := make(map[string]metrics.LatencyDetailSnapshot, len(metricNames))
	for _, name := range metricNames {
		events, err := b.db.QueryBotEvents(ctx, name, since)
		if err != nil {
			logger.Error().Err(err).Str("metric", name).Msg("failed to query bot events")
			continue
		}
		timestamps := make([]time.Time, len(events))
		durations := make([]time.Duration, len(events))
		for i, e := range events {
			timestamps[i] = e.Timestamp
			durations[i] = time.Duration(e.DurationNS)
		}
		latency[name] = metrics.ComputeDetailSnapshot(timestamps, durations)
	}

	// Derive counters from DB.
	messagesStored, _ := b.db.CountBotEvents(ctx, "db_add", since)
	summarizeOK, _ := b.db.CountBotEvents(ctx, "llm_summarize", since)
	summarizeFail, _ := b.db.CountErrors(ctx, since, "llm_cluster", "llm_summarize")
	rateLimitHits, _ := b.db.CountBotEvents(ctx, "rate_limit", since)
	errorCounts, _ := b.db.QueryErrorCounts(ctx, since)
	if errorCounts == nil {
		errorCounts = make(map[string]int64)
	}

	b.metrics.UpdateCache(latency, metrics.CachedCounters{
		MessagesStored: messagesStored,
		SummarizeOK:    summarizeOK,
		SummarizeFail:  summarizeFail,
		RateLimitHits:  rateLimitHits,
		ErrorCounts:    errorCounts,
	})
}

func (b *Bot) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			deleted, err := b.db.CleanupOldMessages(ctx, b.cfg.RetentionDuration())
			if err != nil {
				logger.Error().Err(err).Msg("failed to cleanup old messages")
			} else if deleted > 0 {
				logger.Info().Int64("deleted", deleted).Msg("cleaned up old messages")
			}
			cutoff := time.Now().Add(-metricsRetention)
			if purged, err := b.db.PurgeOldBotEvents(ctx, cutoff); err != nil {
				logger.Error().Err(err).Msg("failed to purge old bot events")
			} else if purged > 0 {
				logger.Info().Int64("purged", purged).Msg("purged old bot events")
			}
			if purged, err := b.db.PurgeOldErrors(ctx, cutoff); err != nil {
				logger.Error().Err(err).Msg("failed to purge old errors")
			} else if purged > 0 {
				logger.Info().Int64("purged", purged).Msg("purged old error log entries")
			}
		}
	}
}

func (b *Bot) rateLimitCleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(rateLimitCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.rateLimiter.ClearOldEntries()
		}
	}
}

func (b *Bot) scanKnownGroups(ctx context.Context) {
	ids, err := b.db.GetAllowedGroupIDs(ctx)
	if err != nil {
		logger.Error().Err(err).Msg("scanKnownGroups: failed to get allowed group IDs")
		return
	}
	for _, id := range ids {
		title := ""
		username := ""
		info, err := b.telegram.GetChat(&telego.GetChatParams{ChatID: tu.ID(id)})
		if err != nil || info == nil {
			logger.Warn().Err(err).Int64("group_id", id).Msg("scanKnownGroups: failed to get chat info")
		} else {
			title = info.Title
			username = info.Username
		}
		if err := b.db.UpsertKnownGroup(ctx, id, title, username); err != nil {
			logger.Error().Err(err).Int64("group_id", id).Msg("scanKnownGroups: failed to upsert known group")
		} else {
			logger.Info().Int64("group_id", id).Str("title", title).Str("username", username).Msg("scanKnownGroups: upserted known group")
		}
	}
}
