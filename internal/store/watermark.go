package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrNoWatermark = errors.New("no watermark")

type Watermark struct {
	PublishTime time.Time `json:"publish_time"`
	BusinessKey string    `json:"business_key"`
}

func (w Watermark) String() string {
	if w.PublishTime.IsZero() {
		return ""
	}
	return w.PublishTime.UTC().Format(time.RFC3339) + "|" + w.BusinessKey
}

func (w Watermark) IsZero() bool {
	return w.PublishTime.IsZero() && w.BusinessKey == ""
}

// ReadWatermark reads the composite (publish_time, business_key) cursor.
func (s *Store) ReadWatermark(ctx context.Context, scope string) (Watermark, error) {
	if scope == "" {
		scope = "incr"
	}
	var publish, key string
	err := s.db.QueryRowContext(ctx, `SELECT publish_time, business_key FROM watermarks WHERE scope = ?`, scope).
		Scan(&publish, &key)
	if err == sql.ErrNoRows {
		return Watermark{}, ErrNoWatermark
	}
	if err != nil {
		return Watermark{}, fmt.Errorf("read watermark: %w", err)
	}
	t, err := time.Parse(time.RFC3339, publish)
	if err != nil {
		return Watermark{}, fmt.Errorf("parse watermark time %q: %w", publish, err)
	}
	return Watermark{PublishTime: t.UTC(), BusinessKey: key}, nil
}

func (s *Store) RecomputeWatermark(ctx context.Context) (Watermark, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Watermark{}, err
	}
	defer tx.Rollback()
	t, key, err := maxAnnouncementTimeKeyTx(ctx, tx)
	if err != nil {
		return Watermark{}, err
	}
	if t.IsZero() {
		return Watermark{}, ErrNoWatermark
	}
	if err := setWatermarkTx(ctx, tx, "incr", t, key, nowUTC()); err != nil {
		return Watermark{}, err
	}
	if err := setWatermarkTx(ctx, tx, "full", t, key, nowUTC()); err != nil {
		return Watermark{}, err
	}
	if err := tx.Commit(); err != nil {
		return Watermark{}, err
	}
	return Watermark{PublishTime: t, BusinessKey: key}, nil
}

func setWatermarkTx(ctx context.Context, tx *sql.Tx, scope string, t time.Time, key string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO watermarks (scope, publish_time, business_key, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(scope) DO UPDATE SET
 publish_time = excluded.publish_time,
 business_key = excluded.business_key,
 updated_at = excluded.updated_at`,
		scope, formatTime(t), key, formatTime(now))
	if err != nil {
		return fmt.Errorf("write watermark %q: %w", scope, err)
	}
	return nil
}

func getWatermarkTx(ctx context.Context, tx *sql.Tx, scope string) (time.Time, string, error) {
	var publish, key string
	err := tx.QueryRowContext(ctx, `SELECT publish_time, business_key FROM watermarks WHERE scope = ?`, scope).
		Scan(&publish, &key)
	if err == sql.ErrNoRows {
		return time.Time{}, "", ErrNoWatermark
	}
	if err != nil {
		return time.Time{}, "", err
	}
	t, err := time.Parse(time.RFC3339, publish)
	if err != nil {
		return time.Time{}, "", err
	}
	return t.UTC(), key, nil
}

func compareTuple(t1 time.Time, k1 string, t2 time.Time, k2 string) int {
	if t1.After(t2) {
		return 1
	}
	if t1.Before(t2) {
		return -1
	}
	if k1 > k2 {
		return 1
	}
	if k1 < k2 {
		return -1
	}
	return 0
}
