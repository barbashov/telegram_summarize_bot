package db

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
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
	UserHash      string // 8-char HMAC-SHA256 hex digest; stable per user+group, non-reversible
	Text          string
	Timestamp     time.Time
	ForwardedFrom string // original author name when message was forwarded; empty otherwise
	TgMessageID   int64  // Telegram's native message_id; 0 = unknown
	ReplyToTgID   int64  // Telegram message_id of parent; 0 = not a reply
}

// UserHash returns an 8-char hex string derived from HMAC-SHA256(userID‖groupID, salt).
// It is stable across restarts (salt is persisted), non-reversible, and group-scoped.
func UserHash(userID, groupID int64, salt []byte) string {
	mac := hmac.New(sha256.New, salt)
	_ = binary.Write(mac, binary.LittleEndian, userID)
	_ = binary.Write(mac, binary.LittleEndian, groupID)
	return hex.EncodeToString(mac.Sum(nil))[:8]
}

// GetUserHashSalt loads the HMAC salt from bot_config, generating and persisting one on first call.
func (db *DB) GetUserHashSalt(ctx context.Context) ([]byte, error) {
	hexSalt, err := db.getOrCreateConfig(ctx, "user_hash_salt", func() string {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			panic("crypto/rand unavailable: " + err.Error())
		}
		return hex.EncodeToString(b)
	})
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(hexSalt)
}

func (db *DB) getOrCreateConfig(ctx context.Context, key string, defaultFn func() string) (string, error) {
	var val string
	err := db.conn.QueryRowContext(ctx, `SELECT value FROM bot_config WHERE key = ?`, key).Scan(&val)
	if err == nil {
		return val, nil
	}
	if err != sql.ErrNoRows {
		return "", fmt.Errorf("failed to query bot_config %q: %w", key, err)
	}
	val = defaultFn()
	if _, err := db.conn.ExecContext(ctx, `INSERT OR IGNORE INTO bot_config (key, value) VALUES (?, ?)`, key, val); err != nil {
		return "", fmt.Errorf("failed to insert bot_config %q: %w", key, err)
	}
	// Re-read in case another writer raced us.
	if err := db.conn.QueryRowContext(ctx, `SELECT value FROM bot_config WHERE key = ?`, key).Scan(&val); err != nil {
		return "", fmt.Errorf("failed to re-read bot_config %q: %w", key, err)
	}
	return val, nil
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
			user_hash TEXT NOT NULL DEFAULT '',
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
		`DROP TABLE IF EXISTS bot_metrics`,
		`CREATE TABLE IF NOT EXISTS bot_events (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			metric      TEXT     NOT NULL,
			timestamp   DATETIME NOT NULL,
			duration_ns INTEGER  NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_bot_events_metric_ts ON bot_events(metric, timestamp)`,
		`CREATE TABLE IF NOT EXISTS bot_error_log (
			id  INTEGER PRIMARY KEY AUTOINCREMENT,
			ts  DATETIME NOT NULL,
			key TEXT     NOT NULL,
			msg TEXT     NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_bot_error_log_ts ON bot_error_log(ts)`,
		`CREATE TABLE IF NOT EXISTS bot_config (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
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

	// Deduplication index on Telegram message identity.
	if _, err := db.conn.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_dedup
		ON messages(group_id, tg_message_id)
		WHERE tg_message_id IS NOT NULL`); err != nil {
		return err
	}

	// PII removal: drop user_id and username, add user_hash.
	for _, col := range []string{"user_id", "username"} {
		if err := db.dropColumnIfExists("messages", col); err != nil {
			return err
		}
	}
	if err := db.addColumnIfNotExists("messages", "user_hash", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
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

func (db *DB) dropColumnIfExists(table, column string) error {
	rows, err := db.conn.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return fmt.Errorf("failed to query table_info(%s): %w", table, err)
	}

	exists := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, colType string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			_ = rows.Close()
			return err
		}
		if name == column {
			exists = true
			break
		}
	}
	rowsErr := rows.Err()
	_ = rows.Close() // close before any write to avoid SQLITE_BUSY
	if rowsErr != nil {
		return rowsErr
	}
	if !exists {
		return nil
	}
	_, err = db.conn.Exec(fmt.Sprintf(`ALTER TABLE %s DROP COLUMN %s`, table, column))
	return err
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
		`INSERT OR IGNORE INTO messages (group_id, user_hash, text, timestamp, forwarded_from, tg_message_id, reply_to_tg_id) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		msg.GroupID, msg.UserHash, msg.Text, msg.Timestamp, msg.ForwardedFrom,
		nullableInt64(msg.TgMessageID), nullableInt64(msg.ReplyToTgID),
	)
	return err
}

func (db *DB) GetMessages(ctx context.Context, groupID int64, since time.Time, limit int) ([]Message, error) {
	defer db.metrics.DBGet.Start()()
	rows, err := db.conn.QueryContext(ctx,
		`SELECT id, group_id, user_hash, text, timestamp, forwarded_from, tg_message_id, reply_to_tg_id
		 FROM messages
		 WHERE group_id = ? AND timestamp > ?
		 ORDER BY timestamp DESC
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
		if err := rows.Scan(&msg.ID, &msg.GroupID, &msg.UserHash, &msg.Text, &msg.Timestamp, &forwardedFrom, &tgMessageID, &replyToTgID); err != nil {
			logger.Error().Err(err).Msg("failed to scan message")
			continue
		}
		msg.ForwardedFrom = forwardedFrom.String
		msg.TgMessageID = tgMessageID.Int64
		msg.ReplyToTgID = replyToTgID.Int64
		messages = append(messages, msg)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Reverse so output is chronological (oldest→newest).
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}

	return messages, nil
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

// InsertErrorLog persists a single error log entry.
func (db *DB) InsertErrorLog(ctx context.Context, ts time.Time, key, msg string) error {
	_, err := db.conn.ExecContext(ctx,
		`INSERT INTO bot_error_log (ts, key, msg) VALUES (?, ?, ?)`,
		ts, key, msg,
	)
	return err
}

// InsertBotEvent persists a single metric event.
func (db *DB) InsertBotEvent(ctx context.Context, metric string, ts time.Time, durationNS int64) error {
	_, err := db.conn.ExecContext(ctx,
		`INSERT INTO bot_events (metric, timestamp, duration_ns) VALUES (?, ?, ?)`,
		metric, ts, durationNS,
	)
	return err
}

// BotEvent is a single metric event row.
type BotEvent struct {
	Metric     string
	Timestamp  time.Time
	DurationNS int64
}

// QueryBotEvents returns all events for a metric since the given time, ordered by timestamp.
func (db *DB) QueryBotEvents(ctx context.Context, metric string, since time.Time) ([]BotEvent, error) {
	rows, err := db.conn.QueryContext(ctx,
		`SELECT metric, timestamp, duration_ns FROM bot_events WHERE metric = ? AND timestamp >= ? ORDER BY timestamp`,
		metric, since,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var events []BotEvent
	for rows.Next() {
		var e BotEvent
		if err := rows.Scan(&e.Metric, &e.Timestamp, &e.DurationNS); err != nil {
			continue
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// CountBotEvents returns the count of events for a metric since the given time.
func (db *DB) CountBotEvents(ctx context.Context, metric string, since time.Time) (int64, error) {
	var count int64
	err := db.conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM bot_events WHERE metric = ? AND timestamp >= ?`,
		metric, since,
	).Scan(&count)
	return count, err
}

// QueryErrorCounts returns error counts grouped by key.
func (db *DB) QueryErrorCounts(ctx context.Context, since time.Time) (map[string]int64, error) {
	rows, err := db.conn.QueryContext(ctx,
		`SELECT key, COUNT(*) FROM bot_error_log WHERE ts >= ? GROUP BY key`,
		since,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	counts := make(map[string]int64)
	for rows.Next() {
		var key string
		var count int64
		if err := rows.Scan(&key, &count); err != nil {
			continue
		}
		counts[key] = count
	}
	return counts, rows.Err()
}

// CountErrors returns the count of errors with given keys since a time.
func (db *DB) CountErrors(ctx context.Context, since time.Time, keys ...string) (int64, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	placeholders := make([]string, len(keys))
	args := []any{since}
	for i, k := range keys {
		placeholders[i] = "?"
		args = append(args, k)
	}
	var count int64
	err := db.conn.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM bot_error_log WHERE ts >= ? AND key IN (%s)`,
			strings.Join(placeholders, ",")),
		args...,
	).Scan(&count)
	return count, err
}

// PurgeOldBotEvents deletes events older than the given time.
func (db *DB) PurgeOldBotEvents(ctx context.Context, before time.Time) (int64, error) {
	result, err := db.conn.ExecContext(ctx, `DELETE FROM bot_events WHERE timestamp < ?`, before)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// PurgeOldErrors deletes error log entries older than the given time.
func (db *DB) PurgeOldErrors(ctx context.Context, before time.Time) (int64, error) {
	result, err := db.conn.ExecContext(ctx, `DELETE FROM bot_error_log WHERE ts < ?`, before)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// ClearAllMetrics removes all bot events and error log entries.
func (db *DB) ClearAllMetrics(ctx context.Context) error {
	if _, err := db.conn.ExecContext(ctx, `DELETE FROM bot_events`); err != nil {
		return err
	}
	_, err := db.conn.ExecContext(ctx, `DELETE FROM bot_error_log`)
	return err
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
