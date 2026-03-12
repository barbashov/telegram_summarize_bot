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
)

type DB struct {
	conn   *sql.DB
	dbPath string
}

type Message struct {
	ID        int64
	GroupID   int64
	UserID    int64
	Username  string
	Text      string
	Timestamp time.Time
}

type RateLimitEntry struct {
	UserID    int64
	GroupID   int64
	Timestamp time.Time
}

func New(dbPath string) (*DB, error) {
	dir := filepath.Dir(dbPath)
	if dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
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

	db := &DB{conn: conn, dbPath: dbPath}
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
			UNIQUE(group_id, id)
		)`,
		`DROP TABLE IF EXISTS admins`,
		`CREATE INDEX IF NOT EXISTS idx_messages_group_timestamp ON messages(group_id, timestamp)`,
		`CREATE TABLE IF NOT EXISTS last_summarize (
			group_id INTEGER PRIMARY KEY,
			timestamp DATETIME NOT NULL
		)`,
	}

	for _, q := range queries {
		if _, err := db.conn.Exec(q); err != nil {
			return err
		}
	}

	return nil
}

func (db *DB) Close() error {
	return db.conn.Close()
}

func (db *DB) AddMessage(ctx context.Context, msg *Message) error {
	if msg.Text == "" || len(msg.Text) > 4096 {
		return nil
	}
	if len(msg.Text) > 4096 {
		msg.Text = msg.Text[:4096]
	}

	_, err := db.conn.ExecContext(ctx,
		`INSERT OR IGNORE INTO messages (group_id, user_id, username, text, timestamp) VALUES (?, ?, ?, ?, ?)`,
		msg.GroupID, msg.UserID, msg.Username, msg.Text, msg.Timestamp,
	)
	return err
}

func (db *DB) GetMessages(ctx context.Context, groupID int64, since time.Time, limit int) ([]Message, error) {
	rows, err := db.conn.QueryContext(ctx,
		`SELECT id, group_id, user_id, username, text, timestamp 
		 FROM messages 
		 WHERE group_id = ? AND timestamp > ? 
		 ORDER BY timestamp ASC 
		 LIMIT ?`,
		groupID, since, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		if err := rows.Scan(&msg.ID, &msg.GroupID, &msg.UserID, &msg.Username, &msg.Text, &msg.Timestamp); err != nil {
			logger.Error().Err(err).Msg("failed to scan message")
			continue
		}
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
		sb.WriteString(fmt.Sprintf("[%s] %s: %s\n", timeStr, username, msg.Text))
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
