package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type DeliveryPart struct {
	Markdown string
	Text     string
}

type Delivery struct {
	Key                string
	ChatID, StatusID   int64
	UpdateID           int64
	Parts              []DeliveryPart
	NextPart, Attempts int
	NextAt             int64
	CursorID           int64
	Checkpoint         time.Time
	Daily              string
	Group              bool
}

func (db *DB) SaveDelivery(ctx context.Context, d Delivery) error {
	if len(d.Parts) == 0 {
		return errors.New("empty delivery")
	}
	parts, err := json.Marshal(d.Parts)
	if err != nil {
		return err
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO summary_deliveries
		(delivery_key, chat_id, status_id, parts, created_at, cursor_id, checkpoint, daily, is_group, attempts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, COALESCE((SELECT attempts FROM daily_runs WHERE group_id=? AND day=?),0))`,
		d.Key, d.ChatID, d.StatusID, parts, time.Now().Unix(), d.CursorID, d.Checkpoint.UTC(), d.Daily, d.Group, d.ChatID, d.Daily); err != nil {
		return err
	}
	if d.UpdateID != 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE update_jobs SET payload=NULL WHERE update_id=?`, d.UpdateID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const deliveryFields = `delivery_key, chat_id, status_id, parts, next_part, attempts, next_at, cursor_id, checkpoint, daily, is_group`

func scanDelivery(row interface{ Scan(...any) error }) (*Delivery, error) {
	var d Delivery
	var raw []byte
	err := row.Scan(&d.Key, &d.ChatID, &d.StatusID, &raw, &d.NextPart,
		&d.Attempts, &d.NextAt, &d.CursorID, &d.Checkpoint, &d.Daily, &d.Group)
	if err != nil {
		return nil, err
	}
	return &d, json.Unmarshal(raw, &d.Parts)
}

func (db *DB) GetDelivery(ctx context.Context, key string) (*Delivery, error) {
	d, err := scanDelivery(db.conn.QueryRowContext(ctx, `SELECT `+deliveryFields+` FROM summary_deliveries WHERE delivery_key=?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return d, err
}

func (db *DB) DueDeliveries(ctx context.Context) ([]string, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT delivery_key FROM summary_deliveries
		WHERE next_at<=? AND (daily='' OR (daily=? AND attempts < 3)) ORDER BY created_at LIMIT 32`,
		time.Now().Unix(), time.Now().UTC().Format(time.DateOnly))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (db *DB) DelayDelivery(ctx context.Context, key string, next time.Time) error {
	_, err := db.conn.ExecContext(ctx, `UPDATE summary_deliveries SET attempts=attempts+1, next_at=? WHERE delivery_key=?`,
		next.Unix(), key)
	return err
}

func (db *DB) FailDelivery(ctx context.Context, d *Delivery) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if d.Daily != "" {
		if err := setDailyRun(ctx, tx, d.ChatID, d.Checkpoint, "failed"); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO bot_events (metric,timestamp,duration_ns) VALUES ('summary_failed',?,0)`, time.Now().UTC()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM summary_deliveries WHERE delivery_key=?`, d.Key); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) ExpireDeliveries(ctx context.Context) error {
	now := time.Now()
	if _, err := db.conn.ExecContext(ctx, `DELETE FROM daily_runs WHERE day < ?`, now.AddDate(0, 0, -7).UTC().Format(time.DateOnly)); err != nil {
		return err
	}
	_, err := db.conn.ExecContext(ctx, `DELETE FROM summary_deliveries WHERE created_at < ? OR (daily<>'' AND daily<?)`,
		now.Add(-48*time.Hour).Unix(), now.UTC().Format(time.DateOnly))
	return err
}

func (db *DB) CancelDelivery(ctx context.Context, key string) error {
	_, err := db.conn.ExecContext(ctx, `DELETE FROM summary_deliveries WHERE delivery_key=?`, key)
	return err
}

func (db *DB) ConfirmPart(ctx context.Context, d *Delivery) error {
	if d.NextPart+1 < len(d.Parts) {
		_, err := db.conn.ExecContext(ctx, `UPDATE summary_deliveries SET next_part=next_part+1
			WHERE delivery_key=? AND next_part=?`, d.Key, d.NextPart)
		return err
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if d.CursorID > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO summary_cursors (group_id,last_id,legacy_max_id)
			VALUES (?,?,0) ON CONFLICT(group_id) DO UPDATE SET last_id=MAX(last_id,excluded.last_id)`,
			d.ChatID, d.CursorID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO last_summarize VALUES (?,?)`,
			d.ChatID, d.Checkpoint.UTC()); err != nil {
			return err
		}
	}
	if d.Daily != "" {
		if err := setDailyRun(ctx, tx, d.ChatID, d.Checkpoint, "delivered"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE group_schedules SET last_daily_summary=? WHERE group_id=?`,
			d.Checkpoint.UTC(), d.ChatID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO bot_events (metric,timestamp,duration_ns) VALUES ('summary_delivered',?,0)`, time.Now().UTC()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM summary_deliveries WHERE delivery_key=?`, d.Key); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) DailyRetryDue(ctx context.Context, groupID int64, now time.Time, delay time.Duration, maxAttempts int) (bool, error) {
	var state string
	var attempts int
	var last int64
	err := db.conn.QueryRowContext(ctx, `SELECT state, attempts, last_at FROM daily_runs
		WHERE group_id=? AND day=?`, groupID, now.UTC().Format(time.DateOnly)).Scan(&state, &attempts, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return state == "retry" && attempts < maxAttempts && !now.Before(time.Unix(last, 0).Add(delay)), nil
}

func setDailyRun(ctx context.Context, conn messageWriter, groupID int64, at time.Time, state string) error {
	attempt := 0
	if state == "retry" {
		attempt = 1
	}
	_, err := conn.ExecContext(ctx, `INSERT INTO daily_runs (group_id, day, attempts, last_at, state)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(group_id,day) DO UPDATE
		SET attempts=attempts+excluded.attempts,last_at=excluded.last_at,state=excluded.state`,
		groupID, at.UTC().Format(time.DateOnly), attempt, at.Unix(), state)
	return err
}

func (db *DB) SetDailyRun(ctx context.Context, groupID int64, at time.Time, state string) error {
	return setDailyRun(ctx, db.conn, groupID, at, state)
}
