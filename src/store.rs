//! rusqlite schema and transaction wrappers.
//!
//! SQLite is embedded via the `bundled` feature, so there is no database
//! process to supervise: the quarterly audit runs as a single self-contained
//! binary that can be copied onto an air-gapped review host.

use std::path::Path;

use rusqlite::{params, Connection, OptionalExtension};

/// Rows per zone-map block, and the audit policy default batch size.
pub const BLOCK_ROWS: i64 = 4096;
pub const DEFAULT_BATCH: usize = 1000;

pub type Result<T> = std::result::Result<T, StoreError>;

#[derive(Debug)]
pub enum StoreError {
    Sqlite(rusqlite::Error),
    Io(std::io::Error),
}

impl std::fmt::Display for StoreError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            StoreError::Sqlite(e) => write!(f, "sqlite: {e}"),
            StoreError::Io(e) => write!(f, "io: {e}"),
        }
    }
}

impl std::error::Error for StoreError {}

impl From<rusqlite::Error> for StoreError {
    fn from(e: rusqlite::Error) -> Self {
        StoreError::Sqlite(e)
    }
}

impl From<std::io::Error> for StoreError {
    fn from(e: std::io::Error) -> Self {
        StoreError::Io(e)
    }
}

/// A certificate projected into the audit table.
#[derive(Debug, Clone)]
pub struct CertRow {
    /// Lowercase hex SHA-256 of the original DER bytes; the dedup key.
    pub sha256: String,
    pub serial_hex: String,
    pub issuer: String,
    pub subject: String,
    pub not_before: i64,
    pub not_after: i64,
    pub key_size: i32,
    pub is_ca: bool,
    pub sig_alg: String,
    /// Absolute byte offset of the record in the source file, for audit trails.
    pub source_offset: i64,
    pub source_file: String,
}

pub struct Store {
    conn: Connection,
}

/// Open (or create) the audit database and apply the schema.
impl Store {
    pub fn open(path: &Path) -> Result<Self> {
        if let Some(dir) = path.parent() {
            if !dir.as_os_str().is_empty() {
                std::fs::create_dir_all(dir)?;
            }
        }
        let conn = Connection::open(path)?;
        // WAL keeps readers from blocking the ingest writer; NORMAL sync is the
        // right trade for a rebuildable cache (see the checkpoint module, which
        // provides the durable progress guarantee that matters here).
        conn.pragma_update(None, "journal_mode", "WAL")?;
        conn.pragma_update(None, "synchronous", "NORMAL")?;
        // Bounded page cache keeps resident memory flat regardless of DB size,
        // which is what holds the million-row case inside the 64 MB budget.
        conn.pragma_update(None, "cache_size", -8000)?; // 8000 KiB
        conn.pragma_update(None, "temp_store", "MEMORY")?;
        let s = Store { conn };
        s.init_schema()?;
        Ok(s)
    }

    /// In-memory store, used by the unit tests.
    pub fn open_in_memory() -> Result<Self> {
        let conn = Connection::open_in_memory()?;
        conn.pragma_update(None, "journal_mode", "MEMORY")?;
        let s = Store { conn };
        s.init_schema()?;
        Ok(s)
    }

    fn init_schema(&self) -> Result<()> {
        self.conn.execute_batch(
            r#"
            CREATE TABLE IF NOT EXISTS certs (
                id            INTEGER PRIMARY KEY,
                sha256        TEXT    NOT NULL UNIQUE,
                serial_hex    TEXT    NOT NULL,
                issuer        TEXT    NOT NULL,
                subject       TEXT    NOT NULL,
                not_before    INTEGER NOT NULL,
                not_after     INTEGER NOT NULL,
                key_size      INTEGER NOT NULL,
                is_ca         INTEGER NOT NULL,
                sig_alg       TEXT    NOT NULL,
                source_file   TEXT    NOT NULL,
                source_offset INTEGER NOT NULL,
                batch_no      INTEGER NOT NULL
            );

            -- Zone map: one row per BLOCK_ROWS certificates, holding the min/max
            -- of every range-queryable column. Queries scan this table first and
            -- only touch `certs` for blocks that cannot be excluded.
            CREATE TABLE IF NOT EXISTS zone_map (
                block_id   INTEGER PRIMARY KEY,
                row_min    INTEGER NOT NULL,
                row_max    INTEGER NOT NULL,
                nb_min     INTEGER NOT NULL,
                nb_max     INTEGER NOT NULL,
                na_min     INTEGER NOT NULL,
                na_max     INTEGER NOT NULL,
                ks_min     INTEGER NOT NULL,
                ks_max     INTEGER NOT NULL,
                cnt        INTEGER NOT NULL
            );

            CREATE TABLE IF NOT EXISTS corrupt_records (
                id           INTEGER PRIMARY KEY,
                source_file  TEXT NOT NULL,
                byte_offset  INTEGER NOT NULL,
                kind         TEXT NOT NULL,
                detail       TEXT NOT NULL,
                hex_before   TEXT NOT NULL,
                hex_after    TEXT NOT NULL,
                seen_batch   INTEGER NOT NULL
            );

            CREATE TABLE IF NOT EXISTS ingest_state (
                source_file  TEXT PRIMARY KEY,
                last_offset  INTEGER NOT NULL,
                batch_no     INTEGER NOT NULL,
                updated_at   INTEGER NOT NULL
            );

            CREATE INDEX IF NOT EXISTS idx_certs_not_after ON certs(not_after);
            CREATE INDEX IF NOT EXISTS idx_certs_key_size  ON certs(key_size);
            CREATE INDEX IF NOT EXISTS idx_certs_issuer    ON certs(issuer);
            CREATE INDEX IF NOT EXISTS idx_certs_not_before ON certs(not_before);
            "#,
        )?;
        Ok(())
    }

    pub fn conn(&self) -> &Connection {
        &self.conn
    }

    /// Does this SHA-256 already exist? Used for the dedup fast path.
    pub fn contains_sha(&self, sha: &str) -> Result<bool> {
        let n: i64 = self.conn.query_row(
            "SELECT EXISTS(SELECT 1 FROM certs WHERE sha256 = ?1)",
            params![sha],
            |r| r.get(0),
        )?;
        Ok(n != 0)
    }

    /// Begin a transaction; returned guard rolls back unless committed.
    pub fn begin(&self) -> Result<()> {
        self.conn.execute_batch("BEGIN IMMEDIATE")?;
        Ok(())
    }

    pub fn commit(&self) -> Result<()> {
        self.conn.execute_batch("COMMIT")?;
        Ok(())
    }

    pub fn rollback(&self) {
        let _ = self.conn.execute_batch("ROLLBACK");
    }

    /// Insert one certificate. Returns false when the SHA-256 was already
    /// present, which is how a repeated delivery is skipped without any
    /// alerting side effect.
    pub fn insert_cert(&self, row: &CertRow, batch_no: i64) -> Result<bool> {
        let changed = self.conn.execute(
            "INSERT OR IGNORE INTO certs
                (sha256, serial_hex, issuer, subject, not_before, not_after,
                 key_size, is_ca, sig_alg, source_file, source_offset, batch_no)
             VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12)",
            params![
                row.sha256,
                row.serial_hex,
                row.issuer,
                row.subject,
                row.not_before,
                row.not_after,
                row.key_size,
                row.is_ca as i32,
                row.sig_alg,
                row.source_file,
                row.source_offset,
                batch_no,
            ],
        )?;
        Ok(changed == 1)
    }

    /// Record a corrupt DER record. Deduped by (file, offset) so a re-delivered
    /// batch does not accumulate duplicate corruption findings either.
    pub fn insert_corrupt(
        &self,
        file: &str,
        offset: i64,
        kind: &str,
        detail: &str,
        hex_before: &str,
        hex_after: &str,
        batch_no: i64,
    ) -> Result<()> {
        self.conn.execute(
            "INSERT INTO corrupt_records
                (source_file, byte_offset, kind, detail, hex_before, hex_after, seen_batch)
             VALUES (?1,?2,?3,?4,?5,?6,?7)
             ON CONFLICT DO NOTHING",
            params![file, offset, kind, detail, hex_before, hex_after, batch_no],
        )?;
        Ok(())
    }

    /// Rebuild zone-map statistics for the whole table.
    ///
    /// Rebuilding wholesale is cheaper and simpler than incremental maintenance:
    /// the table is append-only in `id` order, so a single ordered pass groups
    /// rows into fixed-size blocks deterministically.
    pub fn rebuild_zone_map(&self, block_rows: i64) -> Result<()> {
        self.conn.execute_batch("DELETE FROM zone_map")?;
        let mut stmt = self.conn.prepare(
            "SELECT id, not_before, not_after, key_size
             FROM certs ORDER BY id",
        )?;
        let mut rows = stmt.query([])?;

        let mut block_id: i64 = 0;
        let mut acc: Option<BlockAccumulator> = None;
        let mut buf: Vec<Zone> = Vec::new();

        while let Some(r) = rows.next()? {
            let row = ZoneRow {
                id: r.get(0)?,
                nb: r.get(1)?,
                na: r.get(2)?,
                ks: r.get(3)?,
            };
            let a = acc.get_or_insert_with(|| BlockAccumulator::new(block_id, &row));
            a.push(&row);
            if a.len() >= block_rows {
                buf.push(a.to_zone_row());
                block_id += 1;
                acc = None;
            }
        }
        if let Some(a) = acc {
            buf.push(a.to_zone_row());
        }
        drop(rows);
        drop(stmt);

        let mut ins = self.conn.prepare(
            "INSERT INTO zone_map
                (block_id,row_min,row_max,nb_min,nb_max,na_min,na_max,ks_min,ks_max,cnt)
             VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10)",
        )?;
        for z in &buf {
            ins.execute(params![
                z.block_id, z.row_min, z.row_max, z.nb_min, z.nb_max,
                z.na_min, z.na_max, z.ks_min, z.ks_max, z.cnt,
            ])?;
        }
        Ok(())
    }

    /// Blocks whose zone map overlaps the requested ranges.
    ///
    /// A block is skipped when the block's min sits above `high`, or its max
    /// below `low`, on the constrained dimension. This is the entire point of
    /// the index: at a million rows the scan is over ~245 block rows rather
    /// than a million certificates.
    pub fn candidate_blocks(
        &self,
        nb: Option<(i64, i64)>,
        na: Option<(i64, i64)>,
        ks: Option<(i64, i64)>,
    ) -> Result<Vec<(i64, i64, i64)>> {
        // Constrain the dimensions the caller actually specified; an
        // unconstrained dimension must not narrow anything, so it simply
        // contributes no predicate rather than a vacuous one.
        let dims: [(&str, Option<(i64, i64)>); 3] = [("nb", nb), ("na", na), ("ks", ks)];
        let mut sql = String::from("SELECT block_id, row_min, row_max FROM zone_map WHERE 1=1");
        let mut args: Vec<i64> = Vec::with_capacity(6);
        for (col, r) in dims {
            if let Some((lo, hi)) = r {
                let a = args.len() + 1;
                sql.push_str(&format!(" AND {col}_max >= ?{a} AND {col}_min <= ?{}", a + 1));
                args.push(lo);
                args.push(hi);
            }
        }
        let mut stmt = self.conn.prepare(&sql)?;
        let mut rows = stmt.query(rusqlite::params_from_iter(args.iter()))?;
        let mut out = Vec::new();
        while let Some(r) = rows.next()? {
            out.push((r.get::<_, i64>(0)?, r.get::<_, i64>(1)?, r.get::<_, i64>(2)?));
        }
        Ok(out)
    }

    /// Total block count, for reporting the skip rate.
    pub fn block_count(&self) -> Result<i64> {
        Ok(self
            .conn
            .query_row("SELECT COUNT(*) FROM zone_map", [], |r| r.get(0))?)
    }

    /// Count certificates within the given ranges, visiting only candidate blocks.
    ///
    /// Returns [`QueryStats`] so callers can report the block-skip rate that the
    /// performance budget is actually stated in.
    pub fn count_in_ranges(
        &self,
        nb: Option<(i64, i64)>,
        na: Option<(i64, i64)>,
        ks: Option<(i64, i64)>,
    ) -> Result<QueryStats> {
        let blocks = self.candidate_blocks(nb, na, ks)?;
        let total_blocks = self.block_count()?;

        // Re-check the full predicate on the rows of each surviving block: a
        // block's min/max envelope is a superset of the rows it holds, so the
        // zone map can only prove that a block *might* contain a match.
        let dims: [(&str, Option<(i64, i64)>); 3] =
            [("not_before", nb), ("not_after", na), ("key_size", ks)];
        let mut clause = String::new();
        let mut args: Vec<i64> = Vec::with_capacity(6);
        for (col, r) in dims {
            if let Some((lo, hi)) = r {
                // ?1 and ?2 are already taken by the row-id range.
                let a = args.len() + 3;
                clause.push_str(&format!(" AND {col} BETWEEN ?{a} AND ?{}", a + 1));
                args.push(lo);
                args.push(hi);
            }
        }

        let ranges = collapse_ranges(&blocks);
        let mut total = 0i64;
        for &(lo, hi) in &ranges {
            let sql = format!("SELECT COUNT(*) FROM certs WHERE id BETWEEN ?1 AND ?2{clause}");
            let mut st = self.conn.prepare(&sql)?;
            let mut all: Vec<&dyn rusqlite::ToSql> = vec![&lo, &hi];
            for a in &args {
                all.push(a);
            }
            let n: i64 = st.query_row(all.as_slice(), |r| r.get(0))?;
            total += n;
        }

        let rows_scanned = ranges
            .iter()
            .map(|(lo, hi)| (hi - lo + 1).max(0))
            .sum::<i64>()
            .min(self.total_certs()?);
        Ok(QueryStats {
            matched: total,
            blocks_visited: blocks.len() as i64,
            blocks_total: total_blocks,
            rows_scanned,
            total_rows: self.total_certs()?,
        })
    }

    /// Certificates expiring within `window_secs` of `now`.
    pub fn expiring_soon(&self, now: i64, window_secs: i64) -> Result<Vec<ExpiringCert>> {
        let mut stmt = self.conn.prepare(
            "SELECT issuer, subject, not_after, key_size, serial_hex
             FROM certs WHERE not_after >= ?1 AND not_after <= ?2
             ORDER BY not_after LIMIT 500",
        )?;
        let rows = stmt.query_map(params![now, now + window_secs], |r| {
            Ok(ExpiringCert {
                issuer: r.get(0)?,
                subject: r.get(1)?,
                not_after: r.get(2)?,
                key_size: r.get(3)?,
                serial_hex: r.get(4)?,
            })
        })?;
        rows.collect::<std::result::Result<Vec<_>, _>>().map_err(Into::into)
    }

    /// Certificates below the minimum acceptable key strength.
    pub fn weak_keys(&self, min_bits: i64) -> Result<Vec<ExpiringCert>> {
        let mut stmt = self.conn.prepare(
            "SELECT issuer, subject, not_after, key_size, serial_hex
             FROM certs WHERE key_size < ?1 ORDER BY key_size ASC LIMIT 500",
        )?;
        let rows = stmt.query_map(params![min_bits], |r| {
            Ok(ExpiringCert {
                issuer: r.get(0)?,
                subject: r.get(1)?,
                not_after: r.get(2)?,
                key_size: r.get(3)?,
                serial_hex: r.get(4)?,
            })
        })?;
        rows.collect::<std::result::Result<Vec<_>, _>>().map_err(Into::into)
    }

    /// Save ingest progress so a restart resumes instead of redoing the file.
    pub fn save_progress(&self, file: &str, offset: i64, batch_no: i64, now: i64) -> Result<()> {
        self.conn.execute(
            "INSERT INTO ingest_state (source_file, last_offset, batch_no, updated_at)
             VALUES (?1,?2,?3,?4)
             ON CONFLICT(source_file) DO UPDATE SET
                last_offset = excluded.last_offset,
                batch_no    = excluded.batch_no,
                updated_at  = excluded.updated_at",
            params![file, offset, batch_no, now],
        )?;
        Ok(())
    }

    pub fn load_progress(&self, file: &str) -> Result<Option<(i64, i64)>> {
        self.conn
            .query_row(
                "SELECT last_offset, batch_no FROM ingest_state WHERE source_file = ?1",
                params![file],
                |r| Ok((r.get(0)?, r.get(1)?)),
            )
            .optional()
            .map_err(Into::into)
    }

    pub fn total_certs(&self) -> Result<i64> {
        Ok(self.conn.query_row("SELECT COUNT(*) FROM certs", [], |r| r.get(0))?)
    }

    pub fn corrupt_count(&self) -> Result<i64> {
        Ok(self
            .conn
            .query_row("SELECT COUNT(*) FROM corrupt_records", [], |r| r.get(0))?)
    }

    /// Per-file progress, for the resume report.
    pub fn progress_rows(&self) -> Result<Vec<(String, i64, i64)>> {
        let mut stmt = self
            .conn
            .prepare("SELECT source_file, last_offset, batch_no FROM ingest_state ORDER BY source_file")?;
        let rows = stmt.query_map([], |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)))?;
        rows.collect::<std::result::Result<Vec<_>, _>>().map_err(Into::into)
    }

    pub fn corrupt_samples(&self, limit: i64) -> Result<Vec<CorruptRow>> {
        let mut stmt = self.conn.prepare(
            "SELECT source_file, byte_offset, kind, detail, hex_before, hex_after
             FROM corrupt_records ORDER BY source_file, byte_offset LIMIT ?1",
        )?;
        let rows = stmt.query_map(params![limit], |r| {
            Ok(CorruptRow {
                source_file: r.get(0)?,
                byte_offset: r.get(1)?,
                kind: r.get(2)?,
                detail: r.get(3)?,
                hex_before: r.get(4)?,
                hex_after: r.get(5)?,
            })
        })?;
        rows.collect::<std::result::Result<Vec<_>, _>>().map_err(Into::into)
    }
}

/// Collapse candidate blocks, already ordered by `block_id`, into the minimal
/// set of contiguous `id` ranges. Adjacent surviving blocks merge into a single
/// `id BETWEEN` scan, so a contiguous hit costs one index range read.
fn collapse_ranges(blocks: &[(i64, i64, i64)]) -> Vec<(i64, i64)> {
    let mut out: Vec<(i64, i64)> = Vec::new();
    for &(_, row_min, row_max) in blocks {
        match out.last_mut() {
            Some(last) if row_min <= last.1 + 1 => last.1 = last.1.max(row_max),
            _ => out.push((row_min, row_max)),
        }
    }
    out
}

/// What a range query actually cost, for the performance report.
#[derive(Debug, Clone, Copy)]
pub struct QueryStats {
    pub matched: i64,
    pub blocks_visited: i64,
    pub blocks_total: i64,
    /// Rows within surviving blocks: an upper bound on rows actually read.
    pub rows_scanned: i64,
    pub total_rows: i64,
}

impl QueryStats {
    /// Fraction of blocks the zone map eliminated outright.
    pub fn skip_rate(&self) -> f64 {
        if self.blocks_total == 0 {
            return 0.0;
        }
        1.0 - (self.blocks_visited as f64 / self.blocks_total as f64)
    }
}

#[derive(Debug, Clone)]
struct ZoneRow {
    #[allow(dead_code)]
    id: i64,
    nb: i64,
    na: i64,
    ks: i64,
}

struct BlockAccumulator {
    block_id: i64,
    row_min: i64,
    row_max: i64,
    nb_min: i64,
    nb_max: i64,
    na_min: i64,
    na_max: i64,
    ks_min: i64,
    ks_max: i64,
    cnt: i64,
}

impl BlockAccumulator {
    fn new(block_id: i64, r: &ZoneRow) -> Self {
        BlockAccumulator {
            block_id,
            row_min: r.id,
            row_max: r.id,
            nb_min: r.nb,
            nb_max: r.nb,
            na_min: r.na,
            na_max: r.na,
            ks_min: r.ks,
            ks_max: r.ks,
            cnt: 1,
        }
    }
    fn push(&mut self, r: &ZoneRow) {
        self.row_min = self.row_min.min(r.id);
        self.row_max = self.row_max.max(r.id);
        self.nb_min = self.nb_min.min(r.nb);
        self.nb_max = self.nb_max.max(r.nb);
        self.na_min = self.na_min.min(r.na);
        self.na_max = self.na_max.max(r.na);
        self.ks_min = self.ks_min.min(r.ks);
        self.ks_max = self.ks_max.max(r.ks);
        self.cnt += 1;
    }
    fn len(&self) -> i64 {
        self.cnt
    }
    fn to_zone_row(&self) -> Zone {
        Zone {
            block_id: self.block_id,
            row_min: self.row_min,
            row_max: self.row_max,
            nb_min: self.nb_min,
            nb_max: self.nb_max,
            na_min: self.na_min,
            na_max: self.na_max,
            ks_min: self.ks_min,
            ks_max: self.ks_max,
            cnt: self.cnt,
        }
    }
}

#[derive(Debug, Clone)]
pub struct Zone {
    pub block_id: i64,
    pub row_min: i64,
    pub row_max: i64,
    pub nb_min: i64,
    pub nb_max: i64,
    pub na_min: i64,
    pub na_max: i64,
    pub ks_min: i64,
    pub ks_max: i64,
    pub cnt: i64,
}

#[derive(Debug, Clone)]
pub struct ExpiringCert {
    pub issuer: String,
    pub subject: String,
    pub not_after: i64,
    pub key_size: i64,
    pub serial_hex: String,
}

#[derive(Debug, Clone)]
pub struct CorruptRow {
    pub source_file: String,
    pub byte_offset: i64,
    pub kind: String,
    pub detail: String,
    pub hex_before: String,
    pub hex_after: String,
}

#[cfg(test)]
mod tests {
    use super::*;

    fn row(sha: &str, nb: i64, na: i64, ks: i64) -> CertRow {
        CertRow {
            sha256: sha.into(),
            serial_hex: "01".into(),
            issuer: "CN=Test".into(),
            subject: "CN=Leaf".into(),
            not_before: nb,
            not_after: na,
            key_size: ks as i32,
            is_ca: false,
            sig_alg: "ecdsa-with-SHA256".into(),
            source_offset: 0,
            source_file: "f.der".into(),
        }
    }

    fn store_with(n: i64) -> Store {
        let s = Store::open_in_memory().unwrap();
        s.begin().unwrap();
        for i in 0..n {
            // not_after ascends with the id, giving the zone map real structure.
            s.insert_cert(&row(&format!("sha{i:08}"), 1_000 + i, 2_000 + i, 256), 1).unwrap();
        }
        s.commit().unwrap();
        s
    }

    #[test]
    fn dedup_keeps_first_delivery() {
        let s = Store::open_in_memory().unwrap();
        s.begin().unwrap();
        assert!(s.insert_cert(&row("dup", 1, 2, 256), 1).unwrap());
        assert!(!s.insert_cert(&row("dup", 9, 9, 512), 2).unwrap());
        s.commit().unwrap();
        assert_eq!(s.total_certs().unwrap(), 1);
    }

    #[test]
    fn rollback_discards_whole_batch() {
        let s = Store::open_in_memory().unwrap();
        s.begin().unwrap();
        s.insert_cert(&row("a", 1, 2, 256), 1).unwrap();
        s.rollback();
        assert_eq!(s.total_certs().unwrap(), 0);
    }

    #[test]
    fn zone_map_blocks_partition_the_table() {
        let s = store_with(10_000);
        s.rebuild_zone_map(BLOCK_ROWS).unwrap();
        // 10000 rows over 4096-row blocks: two full blocks plus a remainder.
        assert_eq!(s.block_count().unwrap(), 3);
    }

    #[test]
    fn narrow_range_skips_most_blocks() {
        let s = store_with(100_000);
        s.rebuild_zone_map(BLOCK_ROWS).unwrap();
        // A 10-second window out of a 100000-second span: only 1 block can hit.
        let st = s.count_in_ranges(None, Some((50_000, 50_010)), None).unwrap();
        assert_eq!(st.matched, 11);
        assert!(st.blocks_visited <= 2, "visited {} of {}", st.blocks_visited, st.blocks_total);
        // Skip rate is what the performance budget is really about.
        assert!(st.skip_rate() > 0.90, "skip rate only {:.3}", st.skip_rate());
    }

    #[test]
    fn range_count_matches_brute_force() {
        let s = store_with(20_000);
        s.rebuild_zone_map(BLOCK_ROWS).unwrap();
        for (lo, hi) in [(2_000, 2_500), (5_000, 9_999), (20_000, 30_000)] {
            let count = s.count_in_ranges(None, Some((lo, hi)), None).unwrap().matched;
            // Row i carries not_after = 2000 + i for i in 0..20000.
            let expect = (hi.min(21_999) - lo + 1).max(0);
            assert_eq!(count, expect, "not_after in {lo}..={hi}");
        }
    }

    #[test]
    fn combined_dimensions_are_conjunctive() {
        let s = store_with(20_000);
        s.rebuild_zone_map(BLOCK_ROWS).unwrap();
        // not_after 5000..5010 AND key_size == 256 (every row).
        let all = s.count_in_ranges(None, Some((5_000, 5_010)), None).unwrap().matched;
        assert_eq!(all, 11);
        let none = s
            .count_in_ranges(None, Some((5_000, 5_010)), Some((512, 1024)))
            .unwrap()
            .matched;
        assert_eq!(none, 0);
    }

    #[test]
    fn progress_roundtrips_and_updates() {
        let s = Store::open_in_memory().unwrap();
        assert_eq!(s.load_progress("a.der").unwrap(), None);
        s.save_progress("a.der", 100, 1, 10).unwrap();
        s.save_progress("a.der", 200, 2, 20).unwrap();
        assert_eq!(s.load_progress("a.der").unwrap(), Some((200, 2)));
    }

    #[test]
    fn expiring_and_weak_queries() {
        let s = Store::open_in_memory().unwrap();
        s.begin().unwrap();
        s.insert_cert(&row("strong", 0, 5_000, 2048), 1).unwrap();
        s.insert_cert(&row("weak", 0, 9_000, 512), 1).unwrap();
        s.commit().unwrap();
        assert_eq!(s.expiring_soon(4_000, 2_000).unwrap().len(), 1);
        assert_eq!(s.weak_keys(1024).unwrap().len(), 1);
    }
}
