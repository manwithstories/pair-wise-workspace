//! SQLite persistence: schema, fingerprint uniqueness, chunked transactions,
//! and resumable ingest cursors.
//!
//! All writes are idempotent. Re-importing a batch hits the unique fingerprint
//! index (`INSERT OR IGNORE`), conflicts are counted, and already-committed
//! chunks are skipped via the per-file ingest cursor.

use std::path::Path;

use rusqlite::{Connection, OptionalExtension};

use crate::error::{Error, Result};
use crate::model::{Quarantined, Record};

pub const SCHEMA_VERSION: u32 = 1;

pub struct Store {
    pub conn: Connection,
}

#[derive(Debug, Clone, Copy, Default)]
pub struct InsertCounts {
    pub inserted: u64,
    pub duplicate: u64,
    pub quarantined: u64,
}

impl Store {
    pub fn open(path: &Path) -> Result<Store> {
        let conn = Connection::open(path)?;
        conn.pragma_update(None, "journal_mode", "WAL")?;
        conn.pragma_update(None, "synchronous", "NORMAL")?;
        conn.pragma_update(None, "temp_store", "MEMORY")?;
        conn.pragma_update(None, "foreign_keys", "ON")?;
        let mut store = Store { conn };
        store.init_schema()?;
        Ok(store)
    }

    /// In-memory database used by tests.
    pub fn open_memory() -> Result<Store> {
        let conn = Connection::open_in_memory()?;
        conn.pragma_update(None, "temp_store", "MEMORY")?;
        let mut store = Store { conn };
        store.init_schema()?;
        Ok(store)
    }

    fn init_schema(&mut self) -> Result<()> {
        self.conn.execute_batch(
            r#"
            CREATE TABLE IF NOT EXISTS meta (
                key   TEXT PRIMARY KEY,
                value TEXT NOT NULL
            );

            CREATE TABLE IF NOT EXISTS records (
                fingerprint  TEXT PRIMARY KEY,
                source_file  TEXT NOT NULL,
                line_no      INTEGER NOT NULL,
                raw_tick     INTEGER NOT NULL,
                event_time   INTEGER,
                event        TEXT NOT NULL,
                src_ip       TEXT NOT NULL,
                user         TEXT,
                user_agent   TEXT,
                dest_ip      TEXT,
                dest_port    INTEGER,
                status       INTEGER,
                method       TEXT,
                path         TEXT,
                bytes        INTEGER,
                arrival_seq  INTEGER NOT NULL
            );
            CREATE INDEX IF NOT EXISTS idx_records_event_time ON records(event_time);
            CREATE INDEX IF NOT EXISTS idx_records_src ON records(src_ip, event_time);

            CREATE TABLE IF NOT EXISTS quarantined (
                id           INTEGER PRIMARY KEY AUTOINCREMENT,
                source_file  TEXT NOT NULL,
                line_no      INTEGER NOT NULL,
                raw          TEXT NOT NULL,
                reason       TEXT NOT NULL,
                UNIQUE(source_file, line_no)
            );

            CREATE TABLE IF NOT EXISTS clock_state (
                id           INTEGER PRIMARY KEY CHECK (id = 1),
                high         INTEGER NOT NULL,
                last_raw     INTEGER NOT NULL,
                seen         INTEGER NOT NULL,
                wraparounds  INTEGER NOT NULL
            );

            CREATE TABLE IF NOT EXISTS ingest_cursor (
                source_file  TEXT PRIMARY KEY,
                last_line    INTEGER NOT NULL,
                finished     INTEGER NOT NULL
            );

            CREATE TABLE IF NOT EXISTS window_stats (
                window_id    INTEGER PRIMARY KEY,
                start_sec    INTEGER NOT NULL,
                end_sec      INTEGER NOT NULL,
                event_count  INTEGER NOT NULL,
                restatements INTEGER NOT NULL DEFAULT 0,
                finalized    INTEGER NOT NULL DEFAULT 0
            );

            CREATE TABLE IF NOT EXISTS rule_hits (
                id           INTEGER PRIMARY KEY AUTOINCREMENT,
                rule_id      TEXT NOT NULL,
                group_key    TEXT NOT NULL,
                window_start INTEGER NOT NULL,
                event_time   INTEGER NOT NULL,
                fingerprint  TEXT NOT NULL,
                UNIQUE(rule_id, group_key, window_start, fingerprint)
            );
            CREATE INDEX IF NOT EXISTS idx_rule_hits ON rule_hits(rule_id, window_start);

            CREATE TABLE IF NOT EXISTS sessions (
                session_id   INTEGER PRIMARY KEY,
                src_ip       TEXT NOT NULL,
                user_agent   TEXT NOT NULL,
                start_sec    INTEGER NOT NULL,
                end_sec      INTEGER NOT NULL,
                event_count  INTEGER NOT NULL,
                anomalous    INTEGER NOT NULL DEFAULT 0,
                UNIQUE(src_ip, user_agent, start_sec)
            );

            CREATE TABLE IF NOT EXISTS anomaly_baseline (
                entity       TEXT PRIMARY KEY,
                median       REAL NOT NULL,
                mad          REAL NOT NULL,
                samples      INTEGER NOT NULL
            );

            CREATE TABLE IF NOT EXISTS anomalies (
                id           INTEGER PRIMARY KEY AUTOINCREMENT,
                entity       TEXT NOT NULL,
                window_id    INTEGER NOT NULL,
                score        REAL NOT NULL,
                UNIQUE(entity, window_id)
            );
            "#,
        )?;
        self.set_meta("schema_version", &SCHEMA_VERSION.to_string())?;
        Ok(())
    }

    // ---- meta -------------------------------------------------------------

    pub fn set_meta(&self, key: &str, value: &str) -> Result<()> {
        self.conn.execute(
            "INSERT INTO meta(key, value) VALUES(?1, ?2)
             ON CONFLICT(key) DO UPDATE SET value=excluded.value",
            (key, value),
        )?;
        Ok(())
    }

    pub fn get_meta(&self, key: &str) -> Result<Option<String>> {
        Ok(self
            .conn
            .query_row("SELECT value FROM meta WHERE key=?1", (key,), |r| r.get(0))
            .optional()?)
    }

    // ---- ingest cursor ----------------------------------------------------

    pub fn cursor_line(&self, file: &str) -> Result<u64> {
        Ok(self
            .conn
            .query_row(
                "SELECT last_line FROM ingest_cursor WHERE source_file=?1",
                (file,),
                |r| r.get::<_, i64>(0),
            )
            .optional()?
            .unwrap_or(0) as u64)
    }

    pub fn is_file_finished(&self, file: &str) -> Result<bool> {
        Ok(self
            .conn
            .query_row(
                "SELECT finished FROM ingest_cursor WHERE source_file=?1",
                (file,),
                |r| r.get::<_, i64>(0),
            )
            .optional()?
            .map(|f| f != 0)
            .unwrap_or(false))
    }

    pub fn upsert_cursor(&self, file: &str, last_line: u64, finished: bool) -> Result<()> {
        self.conn.execute(
            "INSERT INTO ingest_cursor(source_file, last_line, finished)
             VALUES(?1, ?2, ?3)
             ON CONFLICT(source_file) DO UPDATE SET
                last_line=excluded.last_line, finished=excluded.finished",
            (file, last_line as i64, finished as i64),
        )?;
        Ok(())
    }

    // ---- rows -------------------------------------------------------------

    /// Insert one record. Returns true when newly inserted, false on fingerprint
    /// conflict (duplicate import).
    pub fn insert_record(
        &self,
        record: &Record,
        event_time: u64,
        arrival_seq: u64,
    ) -> Result<bool> {
        let n = self.conn.execute(
            "INSERT OR IGNORE INTO records(
                fingerprint, source_file, line_no, raw_tick, event_time, event,
                src_ip, user, user_agent, dest_ip, dest_port, status, method,
                path, bytes, arrival_seq)
             VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12,?13,?14,?15,?16)",
            rusqlite::params![
                record.fingerprint,
                record.source_file,
                record.line_no as i64,
                record.raw_tick as i64,
                event_time as i64,
                record.event,
                record.src_ip,
                record.user,
                record.user_agent,
                record.dest_ip,
                record.dest_port.map(|v| v as i64),
                record.status.map(|v| v as i64),
                record.method,
                record.path,
                record.bytes.map(|v| v as i64),
                arrival_seq as i64,
            ],
        )?;
        Ok(n > 0)
    }

    pub fn insert_quarantine(&self, q: &Quarantined) -> Result<()> {
        self.conn.execute(
            "INSERT OR IGNORE INTO quarantined(source_file, line_no, raw, reason)
             VALUES(?1,?2,?3,?4)",
            (&q.source_file, q.line_no as i64, &q.raw, &q.reason),
        )?;
        Ok(())
    }

    pub fn count_records(&self) -> Result<u64> {
        Ok(self
            .conn
            .query_row("SELECT COUNT(*) FROM records", [], |r| r.get::<_, i64>(0))?
            as u64)
    }

    pub fn count_quarantine(&self) -> Result<u64> {
        Ok(self
            .conn
            .query_row("SELECT COUNT(*) FROM quarantined", [], |r| {
                r.get::<_, i64>(0)
            })? as u64)
    }

    // ---- clock state ------------------------------------------------------

    pub fn save_clock(&self, state: &crate::clock::ClockState) -> Result<()> {
        self.conn.execute(
            "INSERT INTO clock_state(id, high, last_raw, seen, wraparounds)
             VALUES(1,?1,?2,?3,?4)
             ON CONFLICT(id) DO UPDATE SET high=excluded.high,
                last_raw=excluded.last_raw, seen=excluded.seen,
                wraparounds=excluded.wraparounds",
            rusqlite::params![
                state.high as i64,
                state.last_raw as i64,
                state.seen as i64,
                state.wraparounds as i64,
            ],
        )?;
        Ok(())
    }

    pub fn load_clock(&self) -> Result<Option<crate::clock::ClockState>> {
        let row = self
            .conn
            .query_row(
                "SELECT high, last_raw, seen, wraparounds FROM clock_state WHERE id=1",
                [],
                |r| {
                    Ok(crate::clock::ClockState {
                        high: r.get::<_, i64>(0)? as u64,
                        last_raw: r.get::<_, i64>(1)? as u32,
                        seen: r.get::<_, i64>(2)? != 0,
                        wraparounds: r.get::<_, i64>(3)? as u64,
                    })
                },
            )
            .optional()?;
        Ok(row)
    }
}

/// Run `body` inside an explicit transaction.
pub fn with_tx<T>(conn: &mut Connection, body: impl FnOnce(&Connection) -> Result<T>) -> Result<T> {
    conn.execute_batch("BEGIN")?;
    let result = body(conn);
    match result {
        Ok(value) => {
            conn.execute_batch("COMMIT")?;
            Ok(value)
        }
        Err(err) => {
            let _ = conn.execute_batch("ROLLBACK");
            Err(Error::Other(format!("transaction rolled back: {err}")))
        }
    }
}
