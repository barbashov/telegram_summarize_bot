package db

import (
	"context"
	"database/sql"
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
	}

	for _, q := range queries {
		if _, err := db.conn.Exec(q); err != nil {
			return err
		}
	}

	// Additive migration: add forwarded_from column to existing databases that predate it.
	_, err := db.conn.Exec(`ALTER TABLE messages ADD COLUMN forwarded_from TEXT`)
	if err != nil {
		// SQLite returns an error when the column already exists; ignore it.
		if !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("failed to migrate forwarded_from column: %w", err)
		}
	}

	// Additive migration: add username column to known_groups.
	_, err = db.conn.Exec(`ALTER TABLE known_groups ADD COLUMN username TEXT NOT NULL DEFAULT ''`)
	if err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("failed to migrate username column: %w", err)
		}
	}

	// Additive migration: add tg_message_id and reply_to_tg_id columns for reply thread support.
	_, err = db.conn.Exec(`ALTER TABLE messages ADD COLUMN tg_message_id INTEGER`)
	if err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("failed to migrate tg_message_id column: %w", err)
		}
	}

	_, err = db.conn.Exec(`ALTER TABLE messages ADD COLUMN reply_to_tg_id INTEGER`)
	if err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("failed to migrate reply_to_tg_id column: %w", err)
		}
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

func (db *DB) FormatMessagesForSummary(messages []Message) string {
	var sb strings.Builder
	for i, msg := range messages {
		username := msg.Username
		if username == "" {
			username = fmt.Sprintf("User%d", msg.UserID)
		}
		timeStr := msg.Timestamp.Format("15:04")
		if msg.ForwardedFrom != "" {
			fmt.Fprintf(&sb, "[%s] %s (fwd: %s): %s\n", timeStr, username, msg.ForwardedFrom, msg.Text)
		} else {
			fmt.Fprintf(&sb, "[%s] %s: %s\n", timeStr, username, msg.Text)
		}
		if i > 0 && i%50 == 0 {
			sb.WriteString("---\n")
		}
	}
	return sb.String()
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
