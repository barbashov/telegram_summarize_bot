package db

import (
	"context"
	"errors"
	"time"
)

var ErrUpdateQueueFull = errors.New("telegram command queue is full")

type UpdateJob struct {
	ID      int64
	Payload []byte
}

func (db *DB) QueueUpdate(ctx context.Context, id int64, payload []byte) error {
	result, err := db.conn.ExecContext(ctx, `INSERT OR IGNORE INTO update_jobs
		(update_id, payload, created_at)
		SELECT ?, ?, ? WHERE (SELECT COUNT(*) FROM update_jobs WHERE payload IS NOT NULL) < 512`,
		id, payload, time.Now().Unix())
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 0 {
		return err
	}
	var exists bool
	if err := db.conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM update_jobs WHERE update_id=?)`, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrUpdateQueueFull
	}
	return nil
}

func (db *DB) PendingUpdates(ctx context.Context, limit int) ([]UpdateJob, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT update_id, payload FROM update_jobs
		WHERE payload IS NOT NULL ORDER BY created_at, update_id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var jobs []UpdateJob
	for rows.Next() {
		var job UpdateJob
		if err := rows.Scan(&job.ID, &job.Payload); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (db *DB) FinishUpdate(ctx context.Context, id int64) error {
	_, err := db.conn.ExecContext(ctx, `UPDATE update_jobs SET payload=NULL WHERE update_id=?`, id)
	return err
}

func (db *DB) PruneUpdates(ctx context.Context) error {
	_, err := db.conn.ExecContext(ctx, `DELETE FROM update_jobs WHERE created_at < ?`, time.Now().Add(-48*time.Hour).Unix())
	return err
}

func (db *DB) ApplyMessageEdit(ctx context.Context, groupID, messageID int64, text string, date, updateID int64) error {
	_, err := db.conn.ExecContext(ctx, `UPDATE messages SET text=?, edit_date=?, edit_update_id=?
		WHERE group_id=? AND tg_message_id=? AND (edit_date, edit_update_id) < (?, ?)
		AND EXISTS(SELECT 1 FROM allowed_groups WHERE group_id=?)`,
		text, date, updateID, groupID, messageID, date, updateID, groupID)
	return err
}
