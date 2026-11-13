package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"crawler/internal/dedup"
)

type Store struct {
	db   *sql.DB
	path string
}

type Record struct {
	RawURL       string
	CanonicalURL string
	Site         string
	Title        string
	TitleNorm    string
	ProjectNo    string
	Section      string
	BusinessKey  string
	PublishTime  time.Time
	Body         string
	ContentHash  string
	Status       string
}

type ApplyOptions struct {
	Mode             string
	AdvanceWatermark bool
}

type ApplyResult struct {
	New       int `json:"new"`
	Revised   int `json:"revised"`
	Duplicate int `json:"dedup"`
	Rows      int `json:"rows"`
}

type Failure struct {
	RunID      string
	Mode       string
	Site       string
	URL        string
	Kind       string
	Class      string
	StatusCode int
	Message    string
	OccurredAt time.Time
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("database path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// A single connection keeps transaction and watermark semantics simple
	// and avoids lock upgrades between concurrent pipeline goroutines.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = DELETE",
		"PRAGMA synchronous = NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("sqlite pragma %q: %w", pragma, err)
		}
	}
	s := &Store{db: db, path: path}
	if err := s.Migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) Path() string { return s.path }

func (s *Store) Migrate(ctx context.Context) error {
	ddl := `
CREATE TABLE IF NOT EXISTS announcements (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    business_key      TEXT NOT NULL UNIQUE,
    project_no        TEXT NOT NULL,
    bid_section       TEXT NOT NULL,
    title             TEXT NOT NULL,
    title_norm        TEXT NOT NULL,
    content           TEXT NOT NULL,
    content_hash      TEXT NOT NULL,
    publish_time      TEXT NOT NULL,
    first_source_url  TEXT NOT NULL,
    latest_source_url TEXT NOT NULL,
    source_site       TEXT NOT NULL,
    version_no        INTEGER NOT NULL DEFAULT 1,
    status            TEXT NOT NULL DEFAULT 'active',
    first_seen_at     TEXT NOT NULL,
    last_seen_at      TEXT NOT NULL,
    updated_at        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_announcements_publish ON announcements(publish_time, business_key);
CREATE TABLE IF NOT EXISTS announcement_aliases (
    canonical_url   TEXT PRIMARY KEY,
    announcement_id INTEGER NOT NULL REFERENCES announcements(id) ON DELETE CASCADE,
    raw_url         TEXT NOT NULL,
    site            TEXT NOT NULL,
    first_seen_at   TEXT NOT NULL,
    last_seen_at    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_aliases_announcement ON announcement_aliases(announcement_id);
CREATE TABLE IF NOT EXISTS versions (
    announcement_id INTEGER NOT NULL REFERENCES announcements(id) ON DELETE CASCADE,
    version_no      INTEGER NOT NULL,
    content_hash    TEXT NOT NULL,
    title           TEXT NOT NULL,
    content         TEXT NOT NULL,
    publish_time    TEXT NOT NULL,
    source_url      TEXT NOT NULL,
    site            TEXT NOT NULL,
    observed_at     TEXT NOT NULL,
    PRIMARY KEY (announcement_id, version_no)
);
CREATE INDEX IF NOT EXISTS idx_versions_hash ON versions(announcement_id, content_hash);
CREATE TABLE IF NOT EXISTS failures (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id      TEXT NOT NULL,
    mode        TEXT NOT NULL,
    site        TEXT NOT NULL,
    url         TEXT NOT NULL,
    kind        TEXT NOT NULL,
    class       TEXT NOT NULL,
    status_code INTEGER NOT NULL DEFAULT 0,
    message     TEXT NOT NULL,
    resolved    INTEGER NOT NULL DEFAULT 0,
    occurred_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_failures_class ON failures(class, resolved, id);
CREATE TABLE IF NOT EXISTS watermarks (
    scope        TEXT PRIMARY KEY,
    publish_time TEXT NOT NULL,
    business_key TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS runs (
    id            TEXT PRIMARY KEY,
    mode          TEXT NOT NULL,
    fetched       INTEGER NOT NULL,
    raw_url       INTEGER NOT NULL,
    uniq_url      INTEGER NOT NULL,
    dedup         INTEGER NOT NULL,
    new_count     INTEGER NOT NULL,
    revised       INTEGER NOT NULL,
    failed        INTEGER NOT NULL,
    failures_json TEXT NOT NULL,
    watermark     TEXT NOT NULL,
    rows          INTEGER NOT NULL,
    started_at    TEXT NOT NULL,
    finished_at   TEXT NOT NULL
);
`
	if _, err := s.db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}
	return nil
}

// ApplyBatch writes announcement rows, alias URLs, version chains and the
// watermark in one transaction. Any error rolls the whole batch back.
func (s *Store) ApplyBatch(ctx context.Context, records []Record, opts ApplyOptions) (ApplyResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("begin batch: %w", err)
	}
	defer tx.Rollback()

	result := ApplyResult{}
	now := nowUTC()
	var maxTime time.Time
	var maxKey string
	for _, record := range records {
		if record.BusinessKey == "" || record.CanonicalURL == "" {
			return ApplyResult{}, fmt.Errorf("record %q missing identity", record.RawURL)
		}
		if record.PublishTime.After(maxTime) || (record.PublishTime.Equal(maxTime) && record.BusinessKey > maxKey) {
			maxTime = record.PublishTime.UTC()
			maxKey = record.BusinessKey
		}
		byKey, err := lookupBusinessKeyTx(ctx, tx, record.BusinessKey)
		if err != nil {
			return ApplyResult{}, err
		}
		byURL, err := lookupURLTx(ctx, tx, record.CanonicalURL)
		if err != nil {
			return ApplyResult{}, err
		}
		decision := dedup.Decide(dedup.Candidate{
			BusinessKey:  record.BusinessKey,
			ContentHash:  record.ContentHash,
			CanonicalURL: record.CanonicalURL,
		}, byKey, byURL)

		switch decision.Kind {
		case dedup.KindNew:
			if err := insertNewTx(ctx, tx, record, now); err != nil {
				return ApplyResult{}, err
			}
			result.New++
		case dedup.KindRevision:
			if err := applyRevisionTx(ctx, tx, record, byKey, now); err != nil {
				return ApplyResult{}, err
			}
			result.Revised++
		case dedup.KindDuplicate:
			if err := applyDuplicateTx(ctx, tx, record, byKey, byURL, now); err != nil {
				return ApplyResult{}, err
			}
			result.Duplicate++
		default:
			return ApplyResult{}, fmt.Errorf("unknown dedup decision %q", decision.Kind)
		}
	}

	if opts.AdvanceWatermark {
		if opts.Mode == "full" {
			m, k, err := maxAnnouncementTimeKeyTx(ctx, tx)
			if err != nil {
				return ApplyResult{}, err
			}
			if !m.IsZero() {
				if err := setWatermarkTx(ctx, tx, "incr", m, k, now); err != nil {
					return ApplyResult{}, err
				}
				if err := setWatermarkTx(ctx, tx, "full", m, k, now); err != nil {
					return ApplyResult{}, err
				}
			}
		} else if !maxTime.IsZero() {
			current, currentKey, err := getWatermarkTx(ctx, tx, "incr")
			if err != nil && !errors.Is(err, ErrNoWatermark) {
				return ApplyResult{}, err
			}
			if compareTuple(maxTime, maxKey, current, currentKey) > 0 {
				if err := setWatermarkTx(ctx, tx, "incr", maxTime, maxKey, now); err != nil {
					return ApplyResult{}, err
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return ApplyResult{}, fmt.Errorf("commit batch: %w", err)
	}
	result.Rows, err = s.CountActiveAnnouncements(ctx)
	if err != nil {
		return ApplyResult{}, err
	}
	return result, nil
}

func lookupBusinessKeyTx(ctx context.Context, tx *sql.Tx, key string) (*dedup.Existing, error) {
	var existing dedup.Existing
	var latestURL string
	err := tx.QueryRowContext(ctx, `SELECT id, business_key, content_hash, latest_source_url FROM announcements WHERE business_key = ?`, key).
		Scan(&existing.AnnouncementID, &existing.BusinessKey, &existing.ContentHash, &latestURL)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup business key: %w", err)
	}
	existing.CanonicalURL = latestURL
	existing.KnownHashes = make(map[string]bool)
	rows, err := tx.QueryContext(ctx, `SELECT content_hash FROM versions WHERE announcement_id = ?`, existing.AnnouncementID)
	if err != nil {
		return nil, fmt.Errorf("lookup known versions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		existing.KnownHashes[hash] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &existing, nil
}

func lookupURLTx(ctx context.Context, tx *sql.Tx, canonicalURL string) (*dedup.Existing, error) {
	var existing dedup.Existing
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT announcement_id FROM announcement_aliases WHERE canonical_url = ?`, canonicalURL).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup URL: %w", err)
	}
	existing.AnnouncementID = id
	err = tx.QueryRowContext(ctx, `SELECT business_key, content_hash, latest_source_url FROM announcements WHERE id = ?`, id).
		Scan(&existing.BusinessKey, &existing.ContentHash, &existing.CanonicalURL)
	if err != nil {
		return nil, fmt.Errorf("lookup URL announcement: %w", err)
	}
	return &existing, nil
}

func insertNewTx(ctx context.Context, tx *sql.Tx, r Record, now time.Time) error {
	publish := formatTime(r.PublishTime)
	_, err := tx.ExecContext(ctx, `
INSERT INTO announcements
(business_key, project_no, bid_section, title, title_norm, content, content_hash, publish_time,
 first_source_url, latest_source_url, source_site, version_no, status, first_seen_at, last_seen_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?)`,
		r.BusinessKey, r.ProjectNo, r.Section, r.Title, r.TitleNorm, r.Body, r.ContentHash, publish,
		r.CanonicalURL, r.CanonicalURL, r.Site, recordStatus(r.Status), formatTime(now), formatTime(now), formatTime(now))
	if err != nil {
		return fmt.Errorf("insert announcement %q: %w", r.BusinessKey, err)
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM announcements WHERE business_key = ?`, r.BusinessKey).Scan(&id); err != nil {
		return fmt.Errorf("read inserted announcement: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO versions
(announcement_id, version_no, content_hash, title, content, publish_time, source_url, site, observed_at)
VALUES (?, 1, ?, ?, ?, ?, ?, ?, ?)`,
		id, r.ContentHash, r.Title, r.Body, publish, r.CanonicalURL, r.Site, formatTime(now)); err != nil {
		return fmt.Errorf("insert initial version: %w", err)
	}
	return upsertAliasTx(ctx, tx, r, id, now)
}

func applyRevisionTx(ctx context.Context, tx *sql.Tx, r Record, existing *dedup.Existing, now time.Time) error {
	var currentVersion int
	if err := tx.QueryRowContext(ctx, `SELECT version_no FROM announcements WHERE id = ?`, existing.AnnouncementID).Scan(&currentVersion); err != nil {
		return fmt.Errorf("read current version: %w", err)
	}
	nextVersion := currentVersion + 1
	publish := formatTime(r.PublishTime)
	_, err := tx.ExecContext(ctx, `
UPDATE announcements SET
 title = ?, title_norm = ?, content = ?, content_hash = ?, publish_time = ?,
 latest_source_url = ?, source_site = ?, version_no = ?, status = ?, last_seen_at = ?, updated_at = ?
WHERE id = ?`,
		r.Title, r.TitleNorm, r.Body, r.ContentHash, publish, r.CanonicalURL, r.Site,
		nextVersion, recordStatus(r.Status), formatTime(now), formatTime(now), existing.AnnouncementID)
	if err != nil {
		return fmt.Errorf("update revision: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO versions
(announcement_id, version_no, content_hash, title, content, publish_time, source_url, site, observed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		existing.AnnouncementID, nextVersion, r.ContentHash, r.Title, r.Body, publish,
		r.CanonicalURL, r.Site, formatTime(now))
	if err != nil {
		return fmt.Errorf("insert revision: %w", err)
	}
	return upsertAliasTx(ctx, tx, r, existing.AnnouncementID, now)
}

func applyDuplicateTx(ctx context.Context, tx *sql.Tx, r Record, byKey, byURL *dedup.Existing, now time.Time) error {
	target := byKey
	if target == nil {
		target = byURL
	}
	if target == nil {
		return fmt.Errorf("duplicate decision without existing row for %q", r.RawURL)
	}
	_, err := tx.ExecContext(ctx, `UPDATE announcements SET last_seen_at = ?, updated_at = ? WHERE id = ?`,
		formatTime(now), formatTime(now), target.AnnouncementID)
	if err != nil {
		return fmt.Errorf("touch duplicate: %w", err)
	}
	// A forwarded URL is retained as an alias but never becomes the first
	// source of the announcement.
	return upsertAliasTx(ctx, tx, r, target.AnnouncementID, now)
}

func upsertAliasTx(ctx context.Context, tx *sql.Tx, r Record, announcementID int64, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO announcement_aliases
(canonical_url, announcement_id, raw_url, site, first_seen_at, last_seen_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(canonical_url) DO UPDATE SET last_seen_at = excluded.last_seen_at`,
		r.CanonicalURL, announcementID, r.RawURL, r.Site, formatTime(now), formatTime(now))
	if err != nil {
		return fmt.Errorf("upsert alias %q: %w", r.CanonicalURL, err)
	}
	return nil
}

func maxAnnouncementTimeKeyTx(ctx context.Context, tx *sql.Tx) (time.Time, string, error) {
	var publish, key string
	err := tx.QueryRowContext(ctx, `SELECT publish_time, business_key FROM announcements ORDER BY publish_time DESC, business_key DESC LIMIT 1`).
		Scan(&publish, &key)
	if err == sql.ErrNoRows {
		return time.Time{}, "", nil
	}
	if err != nil {
		return time.Time{}, "", fmt.Errorf("read max announcement: %w", err)
	}
	t, err := time.Parse(time.RFC3339, publish)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("parse stored publish time %q: %w", publish, err)
	}
	return t.UTC(), key, nil
}

func (s *Store) RecordFailures(ctx context.Context, failures []Failure) error {
	if len(failures) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, f := range failures {
		at := f.OccurredAt
		if at.IsZero() {
			at = time.Now()
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO failures (run_id, mode, site, url, kind, class, status_code, message, resolved, occurred_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
			f.RunID, f.Mode, f.Site, f.URL, f.Kind, f.Class, f.StatusCode, f.Message, formatTime(at)); err != nil {
			return fmt.Errorf("insert failure: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Store) UnresolvedFailures(ctx context.Context, class string) ([]Failure, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, run_id, mode, site, url, kind, class, status_code, message, occurred_at
FROM failures WHERE resolved = 0 AND class = ? ORDER BY id`, class)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Failure
	for rows.Next() {
		var f Failure
		var id int64
		var at string
		if err := rows.Scan(&id, &f.RunID, &f.Mode, &f.Site, &f.URL, &f.Kind, &f.Class, &f.StatusCode, &f.Message, &at); err != nil {
			return nil, err
		}
		f.OccurredAt, _ = time.Parse(time.RFC3339, at)
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) ResolveFailures(ctx context.Context, urls []string) error {
	if len(urls) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, u := range urls {
		if _, err := tx.ExecContext(ctx, `UPDATE failures SET resolved = 1 WHERE url = ? AND resolved = 0`, u); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) CountAnnouncements(ctx context.Context) (int, error) {
	return s.count(ctx, "announcements")
}

func (s *Store) CountActiveAnnouncements(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM announcements WHERE status <> 'withdrawn'`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count active announcements: %w", err)
	}
	return n, nil
}

func (s *Store) CountVersions(ctx context.Context) (int, error) {
	return s.count(ctx, "versions")
}

func (s *Store) CountAliases(ctx context.Context) (int, error) {
	return s.count(ctx, "announcement_aliases")
}

func (s *Store) count(ctx context.Context, table string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		return 0, fmt.Errorf("count %s: %w", table, err)
	}
	return n, nil
}

type FailureCount struct {
	Class string `json:"class"`
	Count int    `json:"count"`
}

func (s *Store) FailureCounts(ctx context.Context) ([]FailureCount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT class, COUNT(*) FROM failures GROUP BY class ORDER BY class`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FailureCount
	for rows.Next() {
		var f FailureCount
		if err := rows.Scan(&f.Class, &f.Count); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func recordStatus(status string) string {
	if status == "" {
		return "active"
	}
	return status
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func nowUTC() time.Time { return time.Now().UTC().Truncate(time.Second) }
