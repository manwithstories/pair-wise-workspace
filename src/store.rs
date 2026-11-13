use std::path::Path;
use std::time::Duration;

use rusqlite::{params, Connection, OptionalExtension, Transaction};

use crate::identity::IdentityConflict;
use crate::parse::ParseReject;
use crate::timewin::{window_end, window_start, WrapNotice};

#[derive(Debug, Clone)]
pub struct EventRow {
    pub fingerprint: String,
    pub kind: String,
    pub device: String,
    pub batch_seq: u32,
    pub event_seq: u64,
    pub raw_ts: i64,
    pub event_ts: i64,
    pub window_start: i64,
    pub host: String,
    pub src_ip: Option<String>,
    pub src_port: Option<u16>,
    pub dst_ip: Option<String>,
    pub dst_port: Option<u16>,
    pub public_ip: Option<String>,
    pub public_port: Option<u16>,
    pub action: Option<String>,
    pub signature: Option<String>,
    pub bytes: u64,
    pub direction: Option<String>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum EventDisposition {
    Inserted,
    Duplicate,
    Closed,
}

#[derive(Debug, Clone)]
pub struct RuleEvent {
    pub host: String,
    pub event_ts: i64,
    pub action: String,
    pub src_ip: Option<String>,
    pub dst_ip: Option<String>,
    pub dst_port: Option<u16>,
    pub bytes: u64,
    pub signature: Option<String>,
}

#[derive(Debug, Clone)]
pub struct HourAggregate {
    pub host: String,
    pub hour_start: i64,
    pub bytes: u64,
    pub sessions: u64,
}

#[derive(Debug, Clone, Default)]
pub struct StoreStats {
    pub windows: u64,
    pub closed_windows: u64,
    pub stored_events: u64,
    pub parse_success: u64,
    pub parse_rejected: u64,
    pub late_dropped: u64,
    pub quarantined: u64,
    pub wraps: u64,
    pub identity_conflicts: u64,
}

impl StoreStats {
    pub fn raw_events(&self) -> u64 {
        self.parse_success + self.parse_rejected
    }
}

pub struct Store {
    connection: Connection,
}

impl Store {
    pub fn open(path: &Path) -> rusqlite::Result<Self> {
        let connection = Connection::open(path)?;
        Self::configure(&connection)?;
        let store = Self { connection };
        store.initialize()?;
        Ok(store)
    }

    pub fn open_in_memory() -> rusqlite::Result<Self> {
        let connection = Connection::open_in_memory()?;
        Self::configure(&connection)?;
        let store = Self { connection };
        store.initialize()?;
        Ok(store)
    }

    fn configure(connection: &Connection) -> rusqlite::Result<()> {
        connection.busy_timeout(Duration::from_secs(10))?;
        connection.execute_batch(
            "PRAGMA journal_mode=WAL;
             PRAGMA synchronous=NORMAL;
             PRAGMA temp_store=MEMORY;
             PRAGMA cache_size=-65536;
             PRAGMA foreign_keys=ON;",
        )?;
        Ok(())
    }

    fn initialize(&self) -> rusqlite::Result<()> {
        self.connection.execute_batch(
            "CREATE TABLE IF NOT EXISTS windows (
                window_id INTEGER PRIMARY KEY,
                start_ts INTEGER NOT NULL,
                end_ts INTEGER NOT NULL,
                closed INTEGER NOT NULL DEFAULT 0,
                closed_at INTEGER
             );
             CREATE TABLE IF NOT EXISTS seen_events (
                fingerprint TEXT PRIMARY KEY,
                kind TEXT NOT NULL,
                event_ts INTEGER NOT NULL,
                window_start INTEGER NOT NULL
             );
             CREATE TABLE IF NOT EXISTS events (
                fingerprint TEXT PRIMARY KEY,
                kind TEXT NOT NULL,
                device TEXT NOT NULL,
                batch_seq INTEGER NOT NULL,
                event_seq INTEGER NOT NULL,
                raw_ts INTEGER NOT NULL,
                event_ts INTEGER NOT NULL,
                window_start INTEGER NOT NULL,
                host TEXT NOT NULL,
                src_ip TEXT,
                src_port INTEGER,
                dst_ip TEXT,
                dst_port INTEGER,
                public_ip TEXT,
                public_port INTEGER,
                action TEXT,
                signature TEXT,
                bytes INTEGER NOT NULL DEFAULT 0,
                direction TEXT
             );
             CREATE INDEX IF NOT EXISTS idx_events_kind_ts ON events(kind, event_ts);
             CREATE INDEX IF NOT EXISTS idx_events_kind_host_ts ON events(kind, host, event_ts);
             CREATE INDEX IF NOT EXISTS idx_events_kind_host_port_ts ON events(kind, host, dst_port, event_ts);
             CREATE TABLE IF NOT EXISTS parse_rejections (
                fingerprint TEXT PRIMARY KEY,
                source TEXT NOT NULL,
                line_no INTEGER NOT NULL,
                summary TEXT NOT NULL,
                reason TEXT NOT NULL
             );
             CREATE TABLE IF NOT EXISTS late_drops (
                fingerprint TEXT PRIMARY KEY,
                kind TEXT NOT NULL,
                event_ts INTEGER NOT NULL,
                window_start INTEGER NOT NULL,
                reason TEXT NOT NULL
             );
             CREATE TABLE IF NOT EXISTS time_quarantine (
                fingerprint TEXT PRIMARY KEY,
                device TEXT NOT NULL,
                raw_ts INTEGER NOT NULL,
                reason TEXT NOT NULL
             );
             CREATE TABLE IF NOT EXISTS wrap_events (
                fingerprint TEXT PRIMARY KEY,
                device TEXT NOT NULL,
                wrap_no INTEGER NOT NULL,
                raw_before INTEGER NOT NULL,
                raw_after INTEGER NOT NULL,
                normalized_after INTEGER NOT NULL
             );
             CREATE TABLE IF NOT EXISTS identity_conflicts (
                fingerprint TEXT PRIMARY KEY,
                ip TEXT NOT NULL,
                previous_host TEXT NOT NULL,
                winning_host TEXT NOT NULL,
                effective_from INTEGER NOT NULL,
                reason TEXT NOT NULL
             );",
        )?;
        Ok(())
    }

    pub fn in_transaction<T>(
        &mut self,
        action: impl FnOnce(&Transaction<'_>) -> rusqlite::Result<T>,
    ) -> rusqlite::Result<T> {
        let transaction = self.connection.transaction()?;
        let value = action(&transaction)?;
        transaction.commit()?;
        Ok(value)
    }

    pub fn record_rejection(tx: &Transaction<'_>, reject: &ParseReject) -> rusqlite::Result<bool> {
        let changed = tx.execute(
            "INSERT OR IGNORE INTO parse_rejections
             (fingerprint, source, line_no, summary, reason)
             VALUES (?1, ?2, ?3, ?4, ?5)",
            params![
                reject.fingerprint,
                reject.source,
                reject.line_no as i64,
                reject.summary,
                reject.reason
            ],
        )?;
        Ok(changed > 0)
    }

    pub fn record_conflict(
        tx: &Transaction<'_>,
        conflict: &IdentityConflict,
    ) -> rusqlite::Result<bool> {
        let changed = tx.execute(
            "INSERT OR IGNORE INTO identity_conflicts
             (fingerprint, ip, previous_host, winning_host, effective_from, reason)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6)",
            params![
                conflict.fingerprint,
                conflict.ip,
                conflict.previous_host,
                conflict.winning_host,
                conflict.effective_from,
                conflict.reason
            ],
        )?;
        Ok(changed > 0)
    }

    pub fn record_wrap(tx: &Transaction<'_>, notice: &WrapNotice) -> rusqlite::Result<bool> {
        let changed = tx.execute(
            "INSERT OR IGNORE INTO wrap_events
             (fingerprint, device, wrap_no, raw_before, raw_after, normalized_after)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6)",
            params![
                notice.fingerprint(),
                notice.device,
                i64::from(notice.wrap_no),
                notice.raw_before,
                notice.raw_after,
                notice.normalized_after
            ],
        )?;
        Ok(changed > 0)
    }

    pub fn record_event(
        tx: &Transaction<'_>,
        event: &EventRow,
    ) -> rusqlite::Result<EventDisposition> {
        if Self::event_is_seen(tx, &event.fingerprint)? {
            return Ok(EventDisposition::Duplicate);
        }
        if Self::window_is_closed(tx, event.window_start)? {
            return Ok(EventDisposition::Closed);
        }
        Self::mark_seen(
            tx,
            &event.fingerprint,
            &event.kind,
            event.event_ts,
            event.window_start,
        )?;
        Self::ensure_window(tx, event.window_start)?;
        tx.execute(
            "INSERT INTO events
             (fingerprint, kind, device, batch_seq, event_seq, raw_ts, event_ts,
              window_start, host, src_ip, src_port, dst_ip, dst_port, public_ip,
              public_port, action, signature, bytes, direction)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13,
                     ?14, ?15, ?16, ?17, ?18, ?19)",
            params![
                event.fingerprint,
                event.kind,
                event.device,
                i64::from(event.batch_seq),
                event.event_seq as i64,
                event.raw_ts,
                event.event_ts,
                event.window_start,
                event.host,
                event.src_ip,
                event.src_port.map(i64::from),
                event.dst_ip,
                event.dst_port.map(i64::from),
                event.public_ip,
                event.public_port.map(i64::from),
                event.action,
                event.signature,
                event.bytes.min(i64::MAX as u64) as i64,
                event.direction,
            ],
        )?;
        Ok(EventDisposition::Inserted)
    }

    pub fn record_late(
        tx: &Transaction<'_>,
        event: &EventRow,
        reason: &str,
    ) -> rusqlite::Result<bool> {
        if Self::event_is_seen(tx, &event.fingerprint)? {
            return Ok(false);
        }
        Self::mark_seen(
            tx,
            &event.fingerprint,
            &event.kind,
            event.event_ts,
            event.window_start,
        )?;
        tx.execute(
            "INSERT OR IGNORE INTO late_drops
             (fingerprint, kind, event_ts, window_start, reason)
             VALUES (?1, ?2, ?3, ?4, ?5)",
            params![
                event.fingerprint,
                event.kind,
                event.event_ts,
                event.window_start,
                reason
            ],
        )?;
        Ok(true)
    }

    #[allow(clippy::too_many_arguments)]
    pub fn record_quarantine(
        tx: &Transaction<'_>,
        fingerprint: &str,
        kind: &str,
        device: &str,
        raw_ts: i64,
        event_ts: i64,
        window_start: i64,
        reason: &str,
    ) -> rusqlite::Result<bool> {
        if Self::event_is_seen(tx, fingerprint)? {
            return Ok(false);
        }
        Self::mark_seen(tx, fingerprint, kind, event_ts, window_start)?;
        tx.execute(
            "INSERT OR IGNORE INTO time_quarantine
             (fingerprint, device, raw_ts, reason)
             VALUES (?1, ?2, ?3, ?4)",
            params![fingerprint, device, raw_ts, reason],
        )?;
        Ok(true)
    }

    fn event_is_seen(tx: &Transaction<'_>, fingerprint: &str) -> rusqlite::Result<bool> {
        let found: Option<i64> = tx
            .query_row(
                "SELECT 1 FROM seen_events WHERE fingerprint = ?1",
                params![fingerprint],
                |row| row.get(0),
            )
            .optional()?;
        Ok(found.is_some())
    }

    fn mark_seen(
        tx: &Transaction<'_>,
        fingerprint: &str,
        kind: &str,
        event_ts: i64,
        window_start: i64,
    ) -> rusqlite::Result<()> {
        tx.execute(
            "INSERT INTO seen_events (fingerprint, kind, event_ts, window_start)
             VALUES (?1, ?2, ?3, ?4)",
            params![fingerprint, kind, event_ts, window_start],
        )?;
        Ok(())
    }

    fn window_is_closed(tx: &Transaction<'_>, window_start_ts: i64) -> rusqlite::Result<bool> {
        let closed: Option<i64> = tx
            .query_row(
                "SELECT closed FROM windows WHERE window_id = ?1",
                params![window_start_ts],
                |row| row.get(0),
            )
            .optional()?;
        Ok(matches!(closed, Some(1)))
    }

    fn ensure_window(tx: &Transaction<'_>, start_ts: i64) -> rusqlite::Result<()> {
        tx.execute(
            "INSERT OR IGNORE INTO windows (window_id, start_ts, end_ts, closed)
             VALUES (?1, ?2, ?3, 0)",
            params![start_ts, start_ts, window_end(start_ts)],
        )?;
        Ok(())
    }

    pub fn close_windows_through(tx: &Transaction<'_>, watermark: i64) -> rusqlite::Result<u64> {
        let mut statement = tx.prepare(
            "SELECT window_id FROM windows
             WHERE closed = 0 AND end_ts + ?1 <= ?2",
        )?;
        let ids: Vec<i64> = statement
            .query_map(
                params![crate::timewin::ALLOWED_LATENESS_SECONDS, watermark],
                |row| row.get::<_, i64>(0),
            )?
            .collect::<Result<Vec<_>, _>>()?;
        drop(statement);
        let mut closed = 0u64;
        for id in ids {
            closed += tx.execute(
                "UPDATE windows SET closed = 1, closed_at = ?1 WHERE window_id = ?2",
                params![watermark, id],
            )? as u64;
        }
        Ok(closed)
    }

    pub fn close_all_windows(tx: &Transaction<'_>, watermark: i64) -> rusqlite::Result<u64> {
        let changed = tx.execute(
            "UPDATE windows SET closed = 1, closed_at = ?1 WHERE closed = 0",
            params![watermark],
        )?;
        Ok(changed as u64)
    }

    pub fn reopen_closed_windows(&mut self) -> rusqlite::Result<Vec<i64>> {
        let transaction = self.connection.transaction()?;
        let mut statement =
            transaction.prepare("SELECT window_id FROM windows WHERE closed = 1")?;
        let ids: Vec<i64> = statement
            .query_map([], |row| row.get::<_, i64>(0))?
            .collect::<Result<Vec<_>, _>>()?;
        drop(statement);
        for id in &ids {
            transaction.execute(
                "DELETE FROM seen_events
                 WHERE fingerprint IN (SELECT fingerprint FROM late_drops WHERE window_start = ?1)",
                params![id],
            )?;
            transaction.execute(
                "DELETE FROM late_drops WHERE window_start = ?1",
                params![id],
            )?;
        }
        transaction.execute(
            "UPDATE windows SET closed = 0, closed_at = NULL WHERE closed = 1",
            [],
        )?;
        transaction.commit()?;
        Ok(ids)
    }

    pub fn stats(&self) -> rusqlite::Result<StoreStats> {
        Ok(StoreStats {
            windows: self.count("windows")?,
            closed_windows: self.count_where("windows", "closed = 1")?,
            stored_events: self.count("events")?,
            parse_success: self.count("seen_events")?,
            parse_rejected: self.count("parse_rejections")?,
            late_dropped: self.count("late_drops")?,
            quarantined: self.count("time_quarantine")?,
            wraps: self.count("wrap_events")?,
            identity_conflicts: self.count("identity_conflicts")?,
        })
    }

    fn count(&self, table: &str) -> rusqlite::Result<u64> {
        self.connection
            .query_row(&format!("SELECT COUNT(*) FROM {table}"), [], |row| {
                row.get::<_, i64>(0).map(|value| value as u64)
            })
    }

    fn count_where(&self, table: &str, predicate: &str) -> rusqlite::Result<u64> {
        self.connection.query_row(
            &format!("SELECT COUNT(*) FROM {table} WHERE {predicate}"),
            [],
            |row| row.get::<_, i64>(0).map(|value| value as u64),
        )
    }

    pub fn edr_rule_events(&self) -> rusqlite::Result<Vec<RuleEvent>> {
        self.rule_events(
            "SELECT host, event_ts, action, src_ip, dst_ip, dst_port, bytes, signature
             FROM events WHERE kind = 'edr' ORDER BY host, event_ts, fingerprint",
        )
    }

    pub fn nat_rule_events(&self) -> rusqlite::Result<Vec<RuleEvent>> {
        self.rule_events(
            "SELECT host, event_ts, action, src_ip, dst_ip, dst_port, bytes, signature
             FROM events WHERE kind = 'nat' AND direction = 'outbound'
             ORDER BY host, dst_port, event_ts, fingerprint",
        )
    }

    fn rule_events(&self, sql: &str) -> rusqlite::Result<Vec<RuleEvent>> {
        let mut statement = self.connection.prepare(sql)?;
        let rows = statement.query_map([], |row| {
            Ok(RuleEvent {
                host: row.get(0)?,
                event_ts: row.get(1)?,
                action: row.get::<_, Option<String>>(2)?.unwrap_or_default(),
                src_ip: row.get(3)?,
                dst_ip: row.get(4)?,
                dst_port: row.get::<_, Option<i64>>(5)?.map(|value| value as u16),
                bytes: row.get::<_, i64>(6)?.max(0) as u64,
                signature: row.get(7)?,
            })
        })?;
        rows.collect()
    }

    pub fn nat_hourly(&self) -> rusqlite::Result<Vec<HourAggregate>> {
        let mut statement = self.connection.prepare(
            "SELECT host, (event_ts / 3600) * 3600 AS hour_start,
                    SUM(bytes), COUNT(*)
             FROM events
             WHERE kind = 'nat' AND direction = 'outbound' AND host <> 'unknown'
             GROUP BY host, hour_start
             ORDER BY host, hour_start",
        )?;
        let rows = statement.query_map([], |row| {
            Ok(HourAggregate {
                host: row.get(0)?,
                hour_start: row.get(1)?,
                bytes: row.get::<_, i64>(2)?.max(0) as u64,
                sessions: row.get::<_, i64>(3)?.max(0) as u64,
            })
        })?;
        rows.collect()
    }

    pub fn event_window(&self, start: i64) -> i64 {
        window_start(start)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn event(fingerprint: &str, ts: i64) -> EventRow {
        EventRow {
            fingerprint: fingerprint.to_owned(),
            kind: "edr".to_owned(),
            device: "device".to_owned(),
            batch_seq: 1,
            event_seq: 1,
            raw_ts: ts,
            event_ts: ts,
            window_start: window_start(ts),
            host: "host-a".to_owned(),
            src_ip: None,
            src_port: None,
            dst_ip: None,
            dst_port: None,
            public_ip: None,
            public_port: None,
            action: Some("file_open".to_owned()),
            signature: None,
            bytes: 0,
            direction: None,
        }
    }

    #[test]
    fn fingerprints_are_idempotent() {
        let mut store = Store::open_in_memory().unwrap();
        store
            .in_transaction(|tx| {
                assert_eq!(
                    Store::record_event(tx, &event("same", 100))?,
                    EventDisposition::Inserted
                );
                assert_eq!(
                    Store::record_event(tx, &event("same", 100))?,
                    EventDisposition::Duplicate
                );
                Store::close_all_windows(tx, 1000)
            })
            .unwrap();
        assert_eq!(store.stats().unwrap().stored_events, 1);
    }

    #[test]
    fn bundled_sqlite_is_the_expected_release_line() {
        assert!(
            rusqlite::version().starts_with("3.45."),
            "{}",
            rusqlite::version()
        );
    }
}
