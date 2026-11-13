package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

type RunRecord struct {
	ID         string
	Mode       string
	Fetched    int
	RawURL     int
	UniqURL    int
	Dedup      int
	New        int
	Revised    int
	Failed     int
	Failures   map[string]int
	Watermark  string
	Rows       int
	StartedAt  time.Time
	FinishedAt time.Time
}

func (s *Store) SaveRun(ctx context.Context, r RunRecord) error {
	failuresJSON, err := json.Marshal(r.Failures)
	if err != nil {
		return err
	}
	if r.ID == "" {
		r.ID = time.Now().UTC().Format("20060102T150405.000000000") + "-" + r.Mode
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO runs
(id, mode, fetched, raw_url, uniq_url, dedup, new_count, revised, failed, failures_json,
 watermark, rows, started_at, finished_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
 fetched=excluded.fetched, raw_url=excluded.raw_url, uniq_url=excluded.uniq_url,
 dedup=excluded.dedup, new_count=excluded.new_count, revised=excluded.revised,
 failed=excluded.failed, failures_json=excluded.failures_json,
 watermark=excluded.watermark, rows=excluded.rows,
 finished_at=excluded.finished_at`,
		r.ID, r.Mode, r.Fetched, r.RawURL, r.UniqURL, r.Dedup, r.New, r.Revised, r.Failed,
		string(failuresJSON), r.Watermark, r.Rows, formatTime(r.StartedAt), formatTime(r.FinishedAt))
	if err != nil {
		return fmt.Errorf("save run: %w", err)
	}
	return nil
}

func (s *Store) LastRun(ctx context.Context) (RunRecord, error) {
	var r RunRecord
	var failuresJSON, started, finished string
	err := s.db.QueryRowContext(ctx, `
SELECT id, mode, fetched, raw_url, uniq_url, dedup, new_count, revised, failed, failures_json,
       watermark, rows, started_at, finished_at
FROM runs ORDER BY finished_at DESC, id DESC LIMIT 1`).
		Scan(&r.ID, &r.Mode, &r.Fetched, &r.RawURL, &r.UniqURL, &r.Dedup, &r.New, &r.Revised,
			&r.Failed, &failuresJSON, &r.Watermark, &r.Rows, &started, &finished)
	if err == sql.ErrNoRows {
		return RunRecord{}, sql.ErrNoRows
	}
	if err != nil {
		return RunRecord{}, err
	}
	if err := json.Unmarshal([]byte(failuresJSON), &r.Failures); err != nil {
		return RunRecord{}, err
	}
	r.StartedAt, _ = time.Parse(time.RFC3339, started)
	r.FinishedAt, _ = time.Parse(time.RFC3339, finished)
	return r, nil
}
