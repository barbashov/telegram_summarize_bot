package storage

import (
	"context"
	"database/sql"
	"time"
)

// Message represents a Telegram message stored in SQLite.
type Message struct {
	ChannelID int64
	MessageID int64
	SenderID  int64
	Username  sql.NullString
	Text      string
	Timestamp time.Time // always stored in UTC
}

// Store defines the persistence operations used by the service.
type Store interface {
	InsertMessage(ctx context.Context, msg Message) error
	GetMessagesInRange(ctx context.Context, channelID int64, from, to time.Time, limit int) ([]Message, error)
}

// SQLiteStore is a concrete implementation of Store backed by SQLite.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore constructs a new SQLiteStore.
func NewSQLiteStore(db *sql.DB) *SQLiteStore {
	return &SQLiteStore{db: db}
}

// InitSchema creates the required tables if they do not already exist.
// This function is idempotent and safe to call on every startup.
func InitSchema(db *sql.DB) error {
	// We keep the schema intentionally simple. Indexes are added for efficient
	// range queries by channel and timestamp.
	const schema = `
CREATE TABLE IF NOT EXISTS messages (
    channel_id INTEGER NOT NULL,
    message_id INTEGER NOT NULL,
    sender_id  INTEGER NOT NULL,
    username   TEXT,
    text       TEXT NOT NULL,
    ts_utc     INTEGER NOT NULL,
    PRIMARY KEY(channel_id, message_id)
);

CREATE INDEX IF NOT EXISTS idx_messages_channel_ts
    ON messages(channel_id, ts_utc);
`
	_, err := db.Exec(schema)
	return err
}

// InsertMessage stores a single message.
func (s *SQLiteStore) InsertMessage(ctx context.Context, msg Message) error {
	_, err := s.db.ExecContext(
		ctx,
		`INSERT OR IGNORE INTO messages(channel_id, message_id, sender_id, username, text, ts_utc)
		 VALUES(?, ?, ?, ?, ?, ?)`,
		msg.ChannelID,
		msg.MessageID,
		msg.SenderID,
		msg.Username,
		msg.Text,
		msg.Timestamp.UTC().Unix(),
	)
	return err
}

// GetMessagesInRange returns messages for a channel between from and to
// (inclusive of from, exclusive of to) ordered by timestamp ascending.
// A hard limit is applied to avoid unbounded memory usage.
func (s *SQLiteStore) GetMessagesInRange(ctx context.Context, channelID int64, from, to time.Time, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 1000
	}

	rows, err := s.db.QueryContext(
		ctx,
		`SELECT channel_id, message_id, sender_id, username, text, ts_utc
		 FROM messages
		 WHERE channel_id = ? AND ts_utc >= ? AND ts_utc < ?
		 ORDER BY ts_utc ASC
		 LIMIT ?`,
		channelID,
		from.UTC().Unix(),
		to.UTC().Unix(),
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var m Message
		var ts int64
		if err := rows.Scan(&m.ChannelID, &m.MessageID, &m.SenderID, &m.Username, &m.Text, &ts); err != nil {
			return nil, err
		}
		m.Timestamp = time.Unix(ts, 0).UTC()
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return msgs, nil
}
