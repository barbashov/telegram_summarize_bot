package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"telegram_summarize_bot/logger"
	"telegram_summarize_bot/provider"
)

// codexRateLimitsKey is the bot_config key holding the latest Codex quota snapshot.
const codexRateLimitsKey = "codex_rate_limits"

// recorderWriteTimeout bounds the best-effort background writes for usage/quota.
const recorderWriteTimeout = 2 * time.Second

// TokenUsageTotals aggregates token counts over a period.
type TokenUsageTotals struct {
	PromptTokens     int64
	CachedTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	Calls            int64
}

// TokenUsageGroup is a labeled aggregation row (by model or operation).
type TokenUsageGroup struct {
	Label       string
	TotalTokens int64
	Calls       int64
}

// InsertTokenUsage records token usage for a single LLM call.
func (db *DB) InsertTokenUsage(ctx context.Context, model, operation string, prompt, cached, completion, total int) error {
	_, err := db.conn.ExecContext(ctx,
		`INSERT INTO token_usage (ts, model, operation, prompt_tokens, cached_tokens, completion_tokens, total_tokens)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		time.Now(), model, operation, prompt, cached, completion, total,
	)
	return err
}

// SumTokenUsageSince returns aggregate totals since the given time, excluding
// throwaway probe calls.
func (db *DB) SumTokenUsageSince(ctx context.Context, since time.Time) (TokenUsageTotals, error) {
	var t TokenUsageTotals
	err := db.conn.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(cached_tokens), 0),
		        COALESCE(SUM(completion_tokens), 0), COALESCE(SUM(total_tokens), 0), COUNT(*)
		 FROM token_usage WHERE ts >= ? AND operation != ?`,
		since, provider.OpProbe,
	).Scan(&t.PromptTokens, &t.CachedTokens, &t.CompletionTokens, &t.TotalTokens, &t.Calls)
	return t, err
}

// TokenUsageByModelSince returns total tokens grouped by model since a time.
func (db *DB) TokenUsageByModelSince(ctx context.Context, since time.Time) ([]TokenUsageGroup, error) {
	return db.scanGroups(ctx,
		`SELECT model, COALESCE(SUM(total_tokens), 0), COUNT(*) FROM token_usage
		 WHERE ts >= ? AND operation != ? GROUP BY model ORDER BY SUM(total_tokens) DESC`,
		since)
}

// TokenUsageByOperationSince returns total tokens grouped by operation since a time.
func (db *DB) TokenUsageByOperationSince(ctx context.Context, since time.Time) ([]TokenUsageGroup, error) {
	return db.scanGroups(ctx,
		`SELECT operation, COALESCE(SUM(total_tokens), 0), COUNT(*) FROM token_usage
		 WHERE ts >= ? AND operation != ? GROUP BY operation ORDER BY SUM(total_tokens) DESC`,
		since)
}

func (db *DB) scanGroups(ctx context.Context, query string, since time.Time) ([]TokenUsageGroup, error) {
	rows, err := db.conn.QueryContext(ctx, query, since, provider.OpProbe)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var groups []TokenUsageGroup
	for rows.Next() {
		var g TokenUsageGroup
		if err := rows.Scan(&g.Label, &g.TotalTokens, &g.Calls); err != nil {
			continue
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

// LatestPromptTokens returns the model and prompt-token size of the most recent
// non-probe call, for context-window utilization. Returns ("", 0, nil) if none.
func (db *DB) LatestPromptTokens(ctx context.Context) (model string, promptTokens int, err error) {
	err = db.conn.QueryRowContext(ctx,
		`SELECT model, prompt_tokens FROM token_usage WHERE operation != ? ORDER BY ts DESC LIMIT 1`,
		provider.OpProbe,
	).Scan(&model, &promptTokens)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	return model, promptTokens, err
}

// PurgeOldTokenUsage deletes token usage rows older than the given time.
func (db *DB) PurgeOldTokenUsage(ctx context.Context, before time.Time) (int64, error) {
	result, err := db.conn.ExecContext(ctx, `DELETE FROM token_usage WHERE ts < ?`, before)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// RecordTokenUsage implements provider.Recorder. Best-effort; logs on failure.
func (db *DB) RecordTokenUsage(_ context.Context, model, operation string, u provider.TokenUsage) {
	ctx, cancel := context.WithTimeout(context.Background(), recorderWriteTimeout)
	defer cancel()
	if err := db.InsertTokenUsage(ctx, model, operation,
		u.PromptTokens, u.CachedInputTokens, u.CompletionTokens, u.TotalTokens); err != nil {
		logger.Warn().Err(err).Msg("failed to record token usage")
	}
}

// SaveCodexRateLimits implements provider.Recorder, upserting the latest snapshot.
func (db *DB) SaveCodexRateLimits(_ context.Context, snap provider.RateLimitSnapshot) {
	data, err := json.Marshal(snap)
	if err != nil {
		logger.Warn().Err(err).Msg("failed to marshal codex rate limits")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recorderWriteTimeout)
	defer cancel()
	if _, err := db.conn.ExecContext(ctx,
		`INSERT OR REPLACE INTO bot_config (key, value) VALUES (?, ?)`,
		codexRateLimitsKey, string(data),
	); err != nil {
		logger.Warn().Err(err).Msg("failed to save codex rate limits")
	}
}

// LoadCodexRateLimits returns the last persisted Codex quota snapshot, if any.
func (db *DB) LoadCodexRateLimits(ctx context.Context) (provider.RateLimitSnapshot, bool) {
	var value string
	err := db.conn.QueryRowContext(ctx,
		`SELECT value FROM bot_config WHERE key = ?`, codexRateLimitsKey,
	).Scan(&value)
	if err != nil {
		return provider.RateLimitSnapshot{}, false
	}
	var snap provider.RateLimitSnapshot
	if err := json.Unmarshal([]byte(value), &snap); err != nil {
		return provider.RateLimitSnapshot{}, false
	}
	return snap, true
}
