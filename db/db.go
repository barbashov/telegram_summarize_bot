package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/glebarez/sqlite"
	"telegram_summarize_bot/logger"
	"telegram_summarize_bot/metrics"
)

type DB struct {
	conn    *sql.DB
	dbPath  string
	metrics *metrics.Metrics
}

type Message struct {
	ID            int64
	GroupID       int64
	UserID        int64
	Username      string
	Text          string
	Timestamp     time.Time
	ForwardedFrom string // original author name when message was forwarded; empty otherwise
	TgMessageID   int64  // Telegram's native message_id; 0 = unknown
	ReplyToTgID   int64  // Telegram message_id of parent; 0 = not a reply
}

type KnownGroup struct {
	GroupID  int64
	Title    string
	Username string
	LastSeen time.Time
	Allowed  bool // true when group_id exists in allowed_groups
}

type GroupSchedule struct {
	GroupID          int64
	Enabled          bool
	Hour             int // UTC hour 0-23
	Minute           int // UTC minute 0-59
	LastDailySummary *time.Time
}

func New(dbPath string, m *metrics.Metrics) (*DB, error) {
	dir := filepath.Dir(dbPath)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create db directory: %w", err)
		}
	}

	conn, err := sql.Open("sqlite", dbPath+"?_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("failed to open db: %w", err)
	}

	if err := conn.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping db: %w", err)
	}

	db := &DB{conn: conn, dbPath: dbPath, metrics: m}
	if err := db.migrate(); err != nil {
		return nil, fmt.Errorf("failed to migrate db: %w", err)
	}

	return db, nil
}

func (db *DB) migrate() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			group_id INTEGER NOT NULL,
			user_id INTEGER NOT NULL,
			username TEXT,
			text TEXT NOT NULL,
			timestamp DATETIME NOT NULL,
			forwarded_from TEXT,
			UNIQUE(group_id, id)
		)`,
		`DROP TABLE IF EXISTS admins`,
		`CREATE INDEX IF NOT EXISTS idx_messages_group_timestamp ON messages(group_id, timestamp)`,
		`CREATE TABLE IF NOT EXISTS last_summarize (
			group_id INTEGER PRIMARY KEY,
			timestamp DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS group_schedules (
			group_id INTEGER PRIMARY KEY,
			enabled INTEGER NOT NULL DEFAULT 0,
			hour INTEGER NOT NULL DEFAULT 7,
			minute INTEGER NOT NULL DEFAULT 0,
			last_daily_summary DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS known_groups (
			group_id INTEGER PRIMARY KEY,
			title TEXT NOT NULL DEFAULT '',
			last_seen DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS allowed_groups (
			group_id INTEGER PRIMARY KEY,
			added_at DATETIME NOT NULL,
			added_by INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS bot_metrics (
			id              INTEGER PRIMARY KEY CHECK (id = 1),
			messages_stored INTEGER NOT NULL DEFAULT 0,
			summarize_ok    INTEGER NOT NULL DEFAULT 0,
			summarize_fail  INTEGER NOT NULL DEFAULT 0,
			rate_limit_hits INTEGER NOT NULL DEFAULT 0,
			error_counts    TEXT    NOT NULL DEFAULT '{}',
			latency_json    TEXT    NOT NULL DEFAULT '{}',
			saved_at        DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS bot_error_log (
			id  INTEGER PRIMARY KEY AUTOINCREMENT,
			ts  DATETIME NOT NULL,
			key TEXT     NOT NULL,
			msg TEXT     NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_bot_error_log_ts ON bot_error_log(ts)`,
	}

	for _, q := range queries {
		if _, err := db.conn.Exec(q); err != nil {
			return err
		}
	}

	// Additive migrations: add columns that may not exist in older databases.
	additiveMigrations := []struct{ table, column, colDef string }{
		{"messages", "forwarded_from", "TEXT"},
		{"known_groups", "username", "TEXT NOT NULL DEFAULT ''"},
		{"messages", "tg_message_id", "INTEGER"},
		{"messages", "reply_to_tg_id", "INTEGER"},
	}
	for _, m := range additiveMigrations {
		if err := db.addColumnIfNotExists(m.table, m.column, m.colDef); err != nil {
			return err
		}
	}

	return nil
}

func (db *DB) addColumnIfNotExists(table, column, colDef string) error {
	_, err := db.conn.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, colDef))
	if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("failed to migrate %s.%s: %w", table, column, err)
	}
	return nil
}

func nullableInt64(v int64) sql.NullInt64 {
	if v == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Valid: true, Int64: v}
}

func (db *DB) Close() error {
	return db.conn.Close()
}

func (db *DB) AddMessage(ctx context.Context, msg *Message) error {
	if msg.Text == "" {
		return nil
	}

	defer db.metrics.DBAdd.Start()()
	_, err := db.conn.ExecContext(ctx,
		`INSERT OR IGNORE INTO messages (group_id, user_id, username, text, timestamp, forwarded_from, tg_message_id, reply_to_tg_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.GroupID, msg.UserID, msg.Username, msg.Text, msg.Timestamp, msg.ForwardedFrom,
		nullableInt64(msg.TgMessageID), nullableInt64(msg.ReplyToTgID),
	)
	return err
}

func (db *DB) GetMessages(ctx context.Context, groupID int64, since time.Time, limit int) ([]Message, error) {
	defer db.metrics.DBGet.Start()()
	rows, err := db.conn.QueryContext(ctx,
		`SELECT id, group_id, user_id, username, text, timestamp, forwarded_from, tg_message_id, reply_to_tg_id
		 FROM messages
		 WHERE group_id = ? AND timestamp > ?
		 ORDER BY timestamp ASC
		 LIMIT ?`,
		groupID, since, limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var messages []Message
	for rows.Next() {
		var msg Message
		var forwardedFrom sql.NullString
		var tgMessageID, replyToTgID sql.NullInt64
		if err := rows.Scan(&msg.ID, &msg.GroupID, &msg.UserID, &msg.Username, &msg.Text, &msg.Timestamp, &forwardedFrom, &tgMessageID, &replyToTgID); err != nil {
			logger.Error().Err(err).Msg("failed to scan message")
			continue
		}
		msg.ForwardedFrom = forwardedFrom.String
		msg.TgMessageID = tgMessageID.Int64
		msg.ReplyToTgID = replyToTgID.Int64
		messages = append(messages, msg)
	}

	return messages, rows.Err()
}

func (db *DB) CleanupOldMessages(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	result, err := db.conn.ExecContext(ctx,
		`DELETE FROM messages WHERE timestamp < ?`,
		cutoff,
	)
	if err != nil {
		return 0, err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return rowsAffected, nil
}

func (db *DB) GetLastSummarizeTime(ctx context.Context, groupID int64) (*time.Time, error) {
	var t time.Time
	err := db.conn.QueryRowContext(ctx,
		`SELECT timestamp FROM last_summarize WHERE group_id = ?`,
		groupID,
	).Scan(&t)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (db *DB) SetLastSummarizeTime(ctx context.Context, groupID int64, t time.Time) error {
	_, err := db.conn.ExecContext(ctx,
		`INSERT OR REPLACE INTO last_summarize (group_id, timestamp) VALUES (?, ?)`,
		groupID, t,
	)
	return err
}

func (db *DB) GetGroupSchedule(ctx context.Context, groupID int64) (*GroupSchedule, error) {
	var s GroupSchedule
	var lastDailySummary sql.NullTime
	err := db.conn.QueryRowContext(ctx,
		`SELECT group_id, enabled, hour, minute, last_daily_summary FROM group_schedules WHERE group_id = ?`,
		groupID,
	).Scan(&s.GroupID, &s.Enabled, &s.Hour, &s.Minute, &lastDailySummary)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if lastDailySummary.Valid {
		s.LastDailySummary = &lastDailySummary.Time
	}
	return &s, nil
}

func (db *DB) SetGroupSchedule(ctx context.Context, s *GroupSchedule) error {
	enabledInt := 0
	if s.Enabled {
		enabledInt = 1
	}
	_, err := db.conn.ExecContext(ctx,
		`INSERT OR REPLACE INTO group_schedules (group_id, enabled, hour, minute, last_daily_summary) VALUES (?, ?, ?, ?, ?)`,
		s.GroupID, enabledInt, s.Hour, s.Minute, s.LastDailySummary,
	)
	return err
}

func (db *DB) GetEnabledSchedules(ctx context.Context) ([]GroupSchedule, error) {
	rows, err := db.conn.QueryContext(ctx,
		`SELECT group_id, enabled, hour, minute, last_daily_summary FROM group_schedules WHERE enabled = 1`,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var schedules []GroupSchedule
	for rows.Next() {
		var s GroupSchedule
		var lastDailySummary sql.NullTime
		if err := rows.Scan(&s.GroupID, &s.Enabled, &s.Hour, &s.Minute, &lastDailySummary); err != nil {
			logger.Error().Err(err).Msg("failed to scan group schedule")
			continue
		}
		if lastDailySummary.Valid {
			s.LastDailySummary = &lastDailySummary.Time
		}
		schedules = append(schedules, s)
	}
	return schedules, rows.Err()
}

func (db *DB) UpdateLastDailySummary(ctx context.Context, groupID int64, t time.Time) error {
	_, err := db.conn.ExecContext(ctx,
		`UPDATE group_schedules SET last_daily_summary = ? WHERE group_id = ?`,
		t, groupID,
	)
	return err
}

func (db *DB) UpsertKnownGroup(ctx context.Context, groupID int64, title, username string) error {
	_, err := db.conn.ExecContext(ctx,
		`INSERT INTO known_groups (group_id, title, username, last_seen) VALUES (?, ?, ?, ?)
		 ON CONFLICT(group_id) DO UPDATE SET title = excluded.title, username = excluded.username, last_seen = excluded.last_seen`,
		groupID, title, username, time.Now(),
	)
	return err
}

func (db *DB) GetKnownGroups(ctx context.Context) ([]KnownGroup, error) {
	rows, err := db.conn.QueryContext(ctx,
		`SELECT kg.group_id, kg.title, kg.username, kg.last_seen, ag.group_id IS NOT NULL AS allowed
		 FROM known_groups kg
		 LEFT JOIN allowed_groups ag ON kg.group_id = ag.group_id
		 ORDER BY kg.title`,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var groups []KnownGroup
	for rows.Next() {
		var g KnownGroup
		var allowedInt int
		if err := rows.Scan(&g.GroupID, &g.Title, &g.Username, &g.LastSeen, &allowedInt); err != nil {
			logger.Error().Err(err).Msg("failed to scan known group")
			continue
		}
		g.Allowed = allowedInt != 0
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

func (db *DB) IsGroupAllowed(ctx context.Context, groupID int64) (bool, error) {
	var count int
	err := db.conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM allowed_groups WHERE group_id = ?`,
		groupID,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (db *DB) AddAllowedGroup(ctx context.Context, groupID, addedBy int64) error {
	_, err := db.conn.ExecContext(ctx,
		`INSERT OR IGNORE INTO allowed_groups (group_id, added_at, added_by) VALUES (?, ?, ?)`,
		groupID, time.Now(), addedBy,
	)
	return err
}

func (db *DB) RemoveAllowedGroup(ctx context.Context, groupID int64) error {
	_, err := db.conn.ExecContext(ctx,
		`DELETE FROM allowed_groups WHERE group_id = ?`,
		groupID,
	)
	return err
}

func (db *DB) GetAllowedGroupIDs(ctx context.Context) ([]int64, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT group_id FROM allowed_groups ORDER BY group_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (db *DB) SaveMetrics(ctx context.Context, s metrics.PersistableSnapshot) error {
	ecJSON, err := json.Marshal(s.ErrorCounts)
	if err != nil {
		return fmt.Errorf("failed to marshal error counts: %w", err)
	}

	latencyMap := map[string]metrics.LatencyRawState{
		"telegram_send": s.TelegramSend,
		"telegram_edit": s.TelegramEdit,
		"llm_cluster":   s.LLMCluster,
		"llm_summarize": s.LLMSummarize,
		"db_add":        s.DBAdd,
		"db_get":        s.DBGet,
	}
	latencyJSON, err := json.Marshal(latencyMap)
	if err != nil {
		return fmt.Errorf("failed to marshal latency: %w", err)
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO bot_metrics (id, messages_stored, summarize_ok, summarize_fail, rate_limit_hits, error_counts, latency_json, saved_at)
		 VALUES (1, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   messages_stored = excluded.messages_stored,
		   summarize_ok    = excluded.summarize_ok,
		   summarize_fail  = excluded.summarize_fail,
		   rate_limit_hits = excluded.rate_limit_hits,
		   error_counts    = excluded.error_counts,
		   latency_json    = excluded.latency_json,
		   saved_at        = excluded.saved_at`,
		s.MessagesStored, s.SummarizeOK, s.SummarizeFail, s.RateLimitHits,
		string(ecJSON), string(latencyJSON), time.Now(),
	)
	if err != nil {
		return fmt.Errorf("failed to upsert bot_metrics: %w", err)
	}

	if _, err = tx.ExecContext(ctx, `DELETE FROM bot_error_log`); err != nil {
		return fmt.Errorf("failed to delete bot_error_log: %w", err)
	}

	for _, e := range s.RecentErrors {
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO bot_error_log (ts, key, msg) VALUES (?, ?, ?)`,
			e.Ts, e.Key, e.Msg,
		); err != nil {
			return fmt.Errorf("failed to insert bot_error_log entry: %w", err)
		}
	}

	return tx.Commit()
}

func (db *DB) LoadMetrics(ctx context.Context, retentionCutoff time.Time) (metrics.PersistableSnapshot, error) {
	var s metrics.PersistableSnapshot
	var ecJSON, latencyJSON string

	err := db.conn.QueryRowContext(ctx,
		`SELECT messages_stored, summarize_ok, summarize_fail, rate_limit_hits, error_counts, latency_json
		 FROM bot_metrics WHERE id = 1`,
	).Scan(&s.MessagesStored, &s.SummarizeOK, &s.SummarizeFail, &s.RateLimitHits, &ecJSON, &latencyJSON)
	if err == sql.ErrNoRows {
		return metrics.PersistableSnapshot{}, nil
	}
	if err != nil {
		return s, fmt.Errorf("failed to query bot_metrics: %w", err)
	}

	s.ErrorCounts = make(map[string]int64)
	if err := json.Unmarshal([]byte(ecJSON), &s.ErrorCounts); err != nil {
		return s, fmt.Errorf("failed to unmarshal error_counts: %w", err)
	}

	var lj struct {
		TelegramSend metrics.LatencyRawState `json:"telegram_send"`
		TelegramEdit metrics.LatencyRawState `json:"telegram_edit"`
		LLMCluster   metrics.LatencyRawState `json:"llm_cluster"`
		LLMSummarize metrics.LatencyRawState `json:"llm_summarize"`
		DBAdd        metrics.LatencyRawState `json:"db_add"`
		DBGet        metrics.LatencyRawState `json:"db_get"`
	}
	if err := json.Unmarshal([]byte(latencyJSON), &lj); err != nil {
		return s, fmt.Errorf("failed to unmarshal latency_json: %w", err)
	}
	s.TelegramSend = lj.TelegramSend
	s.TelegramEdit = lj.TelegramEdit
	s.LLMCluster = lj.LLMCluster
	s.LLMSummarize = lj.LLMSummarize
	s.DBAdd = lj.DBAdd
	s.DBGet = lj.DBGet

	rows, err := db.conn.QueryContext(ctx,
		`SELECT ts, key, msg FROM bot_error_log WHERE ts >= ? ORDER BY ts ASC`,
		retentionCutoff,
	)
	if err != nil {
		return s, fmt.Errorf("failed to query bot_error_log: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var e metrics.ErrorEntry
		if err := rows.Scan(&e.Ts, &e.Key, &e.Msg); err != nil {
			logger.Error().Err(err).Msg("failed to scan bot_error_log row")
			continue
		}
		s.RecentErrors = append(s.RecentErrors, e)
	}

	return s, rows.Err()
}

func (db *DB) SeedAllowedGroupsIfEmpty(ctx context.Context, groupIDs []int64) error {
	if len(groupIDs) == 0 {
		return nil
	}

	var count int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM allowed_groups`).Scan(&count); err != nil {
		return fmt.Errorf("failed to count allowed groups: %w", err)
	}
	if count > 0 {
		return nil
	}

	for _, id := range groupIDs {
		if _, err := db.conn.ExecContext(ctx,
			`INSERT OR IGNORE INTO allowed_groups (group_id, added_at, added_by) VALUES (?, ?, 0)`,
			id, time.Now(),
		); err != nil {
			return fmt.Errorf("failed to seed allowed group %d: %w", id, err)
		}
	}
	return nil
}
