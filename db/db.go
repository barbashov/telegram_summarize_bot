package db

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
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

// PhotoSource distinguishes between compressed photos and image-MIME documents.
type PhotoSource string

const (
	PhotoSourcePhoto    PhotoSource = "photo"
	PhotoSourceDocument PhotoSource = "document"
)

// PhotoRecord holds the metadata needed to fetch and describe a photo later.
// file_id is the per-bot, expiring handle used to download from Telegram;
// file_unique_id is stable across forwards/groups/users and is the cache key.
type PhotoRecord struct {
	ID           int64
	MessageID    int64
	FileUniqueID string
	FileID       string
	MIMEType     string
	FileSize     int64
	Width        int
	Height       int
	Source       PhotoSource
}

// ImageDescription is a cached description of one image, keyed by FileUniqueID.
// When Error is non-empty, Description is empty and the row is a negative-cache
// entry with a short TTL (caller must check Error and CreatedAt to decide reuse).
type ImageDescription struct {
	FileUniqueID string
	Description  string
	Model        string
	CreatedAt    time.Time
	LastUsedAt   time.Time
	Error        string
}

// UserHash returns an 8-char hex string derived from HMAC-SHA256(userID‖groupID, salt).
// It is stable across restarts (salt is persisted), non-reversible, and group-scoped.
func UserHash(userID, groupID int64, salt []byte) string {
	mac := hmac.New(sha256.New, salt)
	_ = binary.Write(mac, binary.LittleEndian, userID)
	_ = binary.Write(mac, binary.LittleEndian, groupID)
	return hex.EncodeToString(mac.Sum(nil))[:8]
}

// HashString returns an 8-char hex digest derived from HMAC-SHA256(s‖groupID, salt).
// Used for forwarded-message origins that have no stable numeric ID (hidden senders).
func HashString(s string, groupID int64, salt []byte) string {
	mac := hmac.New(sha256.New, salt)
	mac.Write([]byte(s))
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

const MaxGroupSummaryInstructionsLength = 2000

type GroupSummaryInstructions struct {
	GroupID      int64
	Instructions string
	UpdatedAt    time.Time
	UpdatedBy    int64
}

func New(dbPath string, m *metrics.Metrics) (*DB, error) {
	dir := filepath.Dir(dbPath)
	if dir != "." {
		// The DB holds chat history; keep its directory owner-only. MkdirAll
		// won't tighten an existing dir, so chmod it explicitly (best-effort).
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("failed to create db directory: %w", err)
		}
		// #nosec G302 -- a directory needs the execute bit (0700) to be traversable
		if err := os.Chmod(dir, 0o700); err != nil {
			logger.Warn().Err(err).Str("dir", dir).Msg("failed to chmod db directory to 0700")
		}
	}

	conn, err := sql.Open("sqlite", dbPath+"?_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("failed to open db: %w", err)
	}

	// Serialize all access through a single connection. SQLite + database/sql can
	// otherwise open several handles to the same file concurrently, and concurrent
	// writers then fail with SQLITE_BUSY ("database is locked"). With one pooled
	// connection, writes queue at the Go layer and can never collide; per-connection
	// PRAGMAs below also apply consistently because there is exactly one connection.
	conn.SetMaxOpenConns(1)

	if err := conn.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping db: %w", err)
	}

	// Foreign-key enforcement is per-connection in SQLite and the URL-style
	// query is silently ignored by some drivers. Force it on at the pool
	// level so ON DELETE CASCADE on message_photos actually fires.
	if _, err := conn.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return nil, fmt.Errorf("failed to enable foreign keys: %w", err)
	}

	// WAL improves write latency and crash safety; busy_timeout is cheap insurance
	// (with a single connection there is never contention, but it costs nothing).
	if _, err := conn.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		return nil, fmt.Errorf("failed to enable WAL: %w", err)
	}
	if _, err := conn.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		return nil, fmt.Errorf("failed to set busy_timeout: %w", err)
	}

	db := &DB{conn: conn, dbPath: dbPath, metrics: m}
	if err := db.migrate(); err != nil {
		return nil, fmt.Errorf("failed to migrate db: %w", err)
	}

	// The file now exists (created by Ping/migrate). Restrict it to owner-only
	// since it stores chat history. Best-effort: don't fail startup if the
	// filesystem doesn't support chmod.
	if err := os.Chmod(dbPath, 0o600); err != nil {
		logger.Warn().Err(err).Str("path", dbPath).Msg("failed to chmod db file to 0600")
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
		`CREATE TABLE IF NOT EXISTS group_summary_instructions (
			group_id     INTEGER PRIMARY KEY,
			instructions TEXT NOT NULL,
			updated_at   DATETIME NOT NULL,
			updated_by   INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS message_photos (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id     INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			file_unique_id TEXT    NOT NULL,
			file_id        TEXT    NOT NULL,
			mime_type      TEXT,
			file_size      INTEGER,
			width          INTEGER,
			height         INTEGER,
			source         TEXT    NOT NULL DEFAULT 'photo'
		)`,
		`CREATE INDEX IF NOT EXISTS idx_message_photos_msg ON message_photos(message_id)`,
		`CREATE INDEX IF NOT EXISTS idx_message_photos_unique ON message_photos(file_unique_id)`,
		`CREATE TABLE IF NOT EXISTS image_descriptions (
			file_unique_id TEXT PRIMARY KEY,
			description    TEXT NOT NULL DEFAULT '',
			model          TEXT NOT NULL DEFAULT '',
			created_at     DATETIME NOT NULL,
			last_used_at   DATETIME NOT NULL,
			error          TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_image_desc_last_used ON image_descriptions(last_used_at)`,
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

	// One-shot purge of plaintext forwarded_from values written before the
	// pseudonymization change. New rows use the "kind:hash" form which always
	// contains a colon, so we clear any legacy row without one.
	if err := db.purgeLegacyForwardedFromOnce(); err != nil {
		return err
	}

	return nil
}

func (db *DB) purgeLegacyForwardedFromOnce() error {
	const flagKey = "forwarded_from_pseudonymized"
	var existing string
	err := db.conn.QueryRow(`SELECT value FROM bot_config WHERE key = ?`, flagKey).Scan(&existing)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("failed to read %s flag: %w", flagKey, err)
	}
	if _, err := db.conn.Exec(
		`UPDATE messages SET forwarded_from = NULL
		 WHERE forwarded_from IS NOT NULL
		   AND forwarded_from <> ''
		   AND instr(forwarded_from, ':') = 0`,
	); err != nil {
		return fmt.Errorf("failed to purge legacy forwarded_from: %w", err)
	}
	if _, err := db.conn.Exec(
		`INSERT OR IGNORE INTO bot_config (key, value) VALUES (?, ?)`, flagKey, "1",
	); err != nil {
		return fmt.Errorf("failed to record %s flag: %w", flagKey, err)
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

// AddMessage inserts a message. Callers must ensure the row has something
// useful in it (text or attached photos); the message row itself can have
// empty text — use AddMessageReturningID when you need to link photos.
func (db *DB) AddMessage(ctx context.Context, msg *Message) error {
	_, err := db.AddMessageReturningID(ctx, msg)
	return err
}

// AddMessageReturningID inserts a message and returns its new id. Returns
// (0, nil) when the row was a duplicate (dedup index on group_id+tg_message_id).
func (db *DB) AddMessageReturningID(ctx context.Context, msg *Message) (int64, error) {
	defer db.metrics.DBAdd.Start()()
	res, err := db.conn.ExecContext(ctx,
		`INSERT OR IGNORE INTO messages (group_id, user_hash, text, timestamp, forwarded_from, tg_message_id, reply_to_tg_id) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		msg.GroupID, msg.UserHash, msg.Text, msg.Timestamp, msg.ForwardedFrom,
		nullableInt64(msg.TgMessageID), nullableInt64(msg.ReplyToTgID),
	)
	if err != nil {
		return 0, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if affected == 0 {
		// Duplicate by (group_id, tg_message_id). Fetch the existing id so callers
		// can still attach photos idempotently.
		if msg.TgMessageID != 0 {
			var id int64
			err := db.conn.QueryRowContext(ctx,
				`SELECT id FROM messages WHERE group_id = ? AND tg_message_id = ?`,
				msg.GroupID, msg.TgMessageID,
			).Scan(&id)
			if err == nil {
				return id, nil
			}
			if err != sql.ErrNoRows {
				return 0, err
			}
		}
		return 0, nil
	}
	return res.LastInsertId()
}

// AddMessagePhotos inserts photo metadata linked to a message. Idempotent
// per (message_id, file_unique_id) — callers don't need to dedup themselves.
func (db *DB) AddMessagePhotos(ctx context.Context, messageID int64, photos []PhotoRecord) error {
	if messageID == 0 || len(photos) == 0 {
		return nil
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, p := range photos {
		source := p.Source
		if source == "" {
			source = PhotoSourcePhoto
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO message_photos (message_id, file_unique_id, file_id, mime_type, file_size, width, height, source)
			 SELECT ?, ?, ?, ?, ?, ?, ?, ?
			 WHERE NOT EXISTS (SELECT 1 FROM message_photos WHERE message_id = ? AND file_unique_id = ?)`,
			messageID, p.FileUniqueID, p.FileID, p.MIMEType, p.FileSize, p.Width, p.Height, string(source),
			messageID, p.FileUniqueID,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetPhotosForMessages returns photos grouped by message_id for the given message IDs.
func (db *DB) GetPhotosForMessages(ctx context.Context, messageIDs []int64) (map[int64][]PhotoRecord, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(messageIDs))
	args := make([]any, len(messageIDs))
	for i, id := range messageIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	// #nosec G201 -- interpolated value is a comma-joined list of "?" placeholders; the ids are passed as bound args
	q := fmt.Sprintf(
		`SELECT id, message_id, file_unique_id, file_id, mime_type, file_size, width, height, source
		 FROM message_photos
		 WHERE message_id IN (%s)
		 ORDER BY id`,
		strings.Join(placeholders, ","),
	)
	rows, err := db.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	result := make(map[int64][]PhotoRecord)
	for rows.Next() {
		var p PhotoRecord
		var mime, source sql.NullString
		var size sql.NullInt64
		var width, height sql.NullInt64
		if err := rows.Scan(&p.ID, &p.MessageID, &p.FileUniqueID, &p.FileID, &mime, &size, &width, &height, &source); err != nil {
			logger.Error().Err(err).Msg("failed to scan message photo")
			continue
		}
		p.MIMEType = mime.String
		p.FileSize = size.Int64
		p.Width = int(width.Int64)
		p.Height = int(height.Int64)
		p.Source = PhotoSource(source.String)
		if p.Source == "" {
			p.Source = PhotoSourcePhoto
		}
		result[p.MessageID] = append(result[p.MessageID], p)
	}
	return result, rows.Err()
}

// GetImageDescription returns the cached description for a file_unique_id, or
// (nil, nil) when not cached.
func (db *DB) GetImageDescription(ctx context.Context, fileUniqueID string) (*ImageDescription, error) {
	if fileUniqueID == "" {
		return nil, nil
	}
	var d ImageDescription
	err := db.conn.QueryRowContext(ctx,
		`SELECT file_unique_id, description, model, created_at, last_used_at, error
		 FROM image_descriptions WHERE file_unique_id = ?`,
		fileUniqueID,
	).Scan(&d.FileUniqueID, &d.Description, &d.Model, &d.CreatedAt, &d.LastUsedAt, &d.Error)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// PutImageDescription upserts a cached description. Passing a non-empty error
// produces a negative-cache entry (description should be empty in that case).
func (db *DB) PutImageDescription(ctx context.Context, d ImageDescription) error {
	if d.FileUniqueID == "" {
		return fmt.Errorf("file_unique_id required")
	}
	now := time.Now()
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	if d.LastUsedAt.IsZero() {
		d.LastUsedAt = now
	}
	_, err := db.conn.ExecContext(ctx,
		`INSERT INTO image_descriptions (file_unique_id, description, model, created_at, last_used_at, error)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(file_unique_id) DO UPDATE SET
			description  = excluded.description,
			model        = excluded.model,
			created_at   = excluded.created_at,
			last_used_at = excluded.last_used_at,
			error        = excluded.error`,
		d.FileUniqueID, d.Description, d.Model, d.CreatedAt, d.LastUsedAt, d.Error,
	)
	return err
}

// TouchImageDescription bumps last_used_at on a cache hit.
func (db *DB) TouchImageDescription(ctx context.Context, fileUniqueID string) error {
	if fileUniqueID == "" {
		return nil
	}
	_, err := db.conn.ExecContext(ctx,
		`UPDATE image_descriptions SET last_used_at = ? WHERE file_unique_id = ?`,
		time.Now(), fileUniqueID,
	)
	return err
}

// CleanupOldImageDescriptions deletes cache entries last used before now-olderThan.
func (db *DB) CleanupOldImageDescriptions(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	res, err := db.conn.ExecContext(ctx,
		`DELETE FROM image_descriptions WHERE last_used_at < ?`,
		cutoff,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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

// GetMessageByTgID returns the stored message with the given Telegram message_id
// in the group, or (nil, nil) when it is absent (e.g. retention-pruned, or never
// ingested). Uses the idx_messages_dedup index on (group_id, tg_message_id).
func (db *DB) GetMessageByTgID(ctx context.Context, groupID, tgMessageID int64) (*Message, error) {
	if tgMessageID == 0 {
		return nil, nil
	}
	defer db.metrics.DBGet.Start()()

	var msg Message
	var forwardedFrom sql.NullString
	var dbTgID, replyToTgID sql.NullInt64
	err := db.conn.QueryRowContext(ctx,
		`SELECT id, group_id, user_hash, text, timestamp, forwarded_from, tg_message_id, reply_to_tg_id
		 FROM messages
		 WHERE group_id = ? AND tg_message_id = ?`,
		groupID, tgMessageID,
	).Scan(&msg.ID, &msg.GroupID, &msg.UserHash, &msg.Text, &msg.Timestamp, &forwardedFrom, &dbTgID, &replyToTgID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	msg.ForwardedFrom = forwardedFrom.String
	msg.TgMessageID = dbTgID.Int64
	msg.ReplyToTgID = replyToTgID.Int64
	return &msg, nil
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

func (db *DB) GetGroupSummaryInstructions(ctx context.Context, groupID int64) (*GroupSummaryInstructions, error) {
	var item GroupSummaryInstructions
	err := db.conn.QueryRowContext(ctx,
		`SELECT group_id, instructions, updated_at, updated_by FROM group_summary_instructions WHERE group_id = ?`,
		groupID,
	).Scan(&item.GroupID, &item.Instructions, &item.UpdatedAt, &item.UpdatedBy)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func (db *DB) SetGroupSummaryInstructions(ctx context.Context, groupID, updatedBy int64, instructions string) error {
	instructions = strings.TrimSpace(instructions)
	if instructions == "" {
		return db.ClearGroupSummaryInstructions(ctx, groupID)
	}
	if len([]rune(instructions)) > MaxGroupSummaryInstructionsLength {
		return fmt.Errorf("summary instructions exceed %d characters", MaxGroupSummaryInstructionsLength)
	}

	_, err := db.conn.ExecContext(ctx,
		`INSERT INTO group_summary_instructions (group_id, instructions, updated_at, updated_by)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(group_id) DO UPDATE SET
			instructions = excluded.instructions,
			updated_at = excluded.updated_at,
			updated_by = excluded.updated_by`,
		groupID, instructions, time.Now(), updatedBy,
	)
	return err
}

func (db *DB) ClearGroupSummaryInstructions(ctx context.Context, groupID int64) error {
	_, err := db.conn.ExecContext(ctx,
		`DELETE FROM group_summary_instructions WHERE group_id = ?`,
		groupID,
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
