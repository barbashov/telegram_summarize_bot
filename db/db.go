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

	return nil
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
		`INSERT OR IGNORE INTO messages (group_id, user_id, username, text, timestamp, forwarded_from) VALUES (?, ?, ?, ?, ?, ?)`,
		msg.GroupID, msg.UserID, msg.Username, msg.Text, msg.Timestamp, msg.ForwardedFrom,
	)
	return err
}

func (db *DB) GetMessages(ctx context.Context, groupID int64, since time.Time, limit int) ([]Message, error) {
	defer db.metrics.DBGet.Start()()
	rows, err := db.conn.QueryContext(ctx,
		`SELECT id, group_id, user_id, username, text, timestamp, forwarded_from
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
		if err := rows.Scan(&msg.ID, &msg.GroupID, &msg.UserID, &msg.Username, &msg.Text, &msg.Timestamp, &forwardedFrom); err != nil {
			logger.Error().Err(err).Msg("failed to scan message")
			continue
		}
		msg.ForwardedFrom = forwardedFrom.String
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
