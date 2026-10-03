//! Zone Map：块级 min/max 统计与范围查询剪枝。
//!
//! 每 [`BLOCK_ROWS`] 行为一块，为 `not_before / not_after / key_size`
//! 各维护一对 min/max。范围查询先只扫 zone_map 表：
//! 块的 min > high 或 max < low 即整块跳过，命中块才回 `certificates` 取数。
//!
//! 跳过率目标是 ≥90%；代价是每 4096 行约 60 字节的额外空间，可忽略。

use crate::store::{CertRow, Store, ZoneRow};
use rusqlite::{params, Connection};

/// 每块行数。
pub const BLOCK_ROWS: i64 = 4096;

/// 按块累积器。摄入过程中增量喂入，满块即产出一条统计。
pub struct ZoneBuilder {
    row_min: i64,
    row_max: i64,
    nb_min: i64,
    nb_max: i64,
    na_min: i64,
    na_max: i64,
    ks_min: i64,
    ks_max: i64,
    count: i64,
}

impl Default for ZoneBuilder {
    fn default() -> Self {
        ZoneBuilder {
            row_min: i64::MAX,
            row_max: i64::MIN,
            nb_min: i64::MAX,
            nb_max: i64::MIN,
            na_min: i64::MAX,
            na_max: i64::MIN,
            ks_min: i64::MAX,
            ks_max: i64::MIN,
            count: 0,
        }
    }
}

impl ZoneBuilder {
    pub fn new() -> Self {
        Self::default()
    }

    /// 喂入一行，返回 true 表示当前块已满。
    pub fn push(&mut self, r: &CertRow) -> bool {
        self.row_min = self.row_min.min(r.source_offset);
        self.row_max = self.row_max.max(r.source_offset);
        self.nb_min = self.nb_min.min(r.not_before);
        self.nb_max = self.nb_max.max(r.not_before);
        self.na_min = self.na_min.min(r.not_after);
        self.na_max = self.na_max.max(r.not_after);
        self.ks_min = self.ks_min.min(r.key_size);
        self.ks_max = self.ks_max.max(r.key_size);
        self.count += 1;
        self.count >= BLOCK_ROWS
    }

    pub fn is_empty(&self) -> bool {
        self.count == 0
    }

    pub fn count(&self) -> i64 {
        self.count
    }

    /// 导出当前块统计。
    pub fn finish(&self) -> ZoneRow {
        ZoneRow {
            row_min: if self.count == 0 { 0 } else { self.row_min },
            row_max: if self.count == 0 { 0 } else { self.row_max },
            nb_min: if self.count == 0 { 0 } else { self.nb_min },
            nb_max: if self.count == 0 { 0 } else { self.nb_max },
            na_min: if self.count == 0 { 0 } else { self.na_min },
            na_max: if self.count == 0 { 0 } else { self.na_max },
            ks_min: if self.count == 0 { 0 } else { self.ks_min },
            ks_max: if self.count == 0 { 0 } else { self.ks_max },
            row_count: self.count,
        }
    }
}

/// 查询维度。
#[derive(Debug, Clone, Copy)]
pub enum Column {
    NotBefore,
    NotAfter,
    KeySize,
}

/// 查询区间。
#[derive(Debug, Clone, Copy)]
pub struct Range {
    pub col: Column,
    pub low: i64,
    pub high: i64,
}

impl Range {
    pub fn not_after(low: i64, high: i64) -> Self {
        Range {
            col: Column::NotAfter,
            low,
            high,
        }
    }
    pub fn not_before(low: i64, high: i64) -> Self {
        Range {
            col: Column::NotBefore,
            low,
            high,
        }
    }
    pub fn key_size(low: i64, high: i64) -> Self {
        Range {
            col: Column::KeySize,
            low,
            high,
        }
    }
}

/// 块级剪枝结果。
#[derive(Debug, Clone)]
pub struct ScanResult {
    /// 命中的块 id（可直接用于回表 `row_min..=row_max`）。
    pub hit_zones: Vec<i64>,
    pub total_zones: usize,
    pub skipped_zones: usize,
}

impl ScanResult {
    pub fn skip_ratio(&self) -> f64 {
        if self.total_zones == 0 {
            return 1.0;
        }
        self.skipped_zones as f64 / self.total_zones as f64
    }
}

fn col_sql(c: Column) -> (&'static str, &'static str, &'static str) {
    // (用于 certificates 回表的列名, zone_map 的 min 列, zone_map 的 max 列)
    match c {
        Column::NotBefore => ("not_before", "nb_min", "nb_max"),
        Column::NotAfter => ("not_after", "na_min", "na_max"),
        Column::KeySize => ("key_size", "ks_min", "ks_max"),
    }
}

/// 块级扫描：只读 zone_map，返回可安全跳过的块已剔除的结果。
pub fn scan_zones(conn: &Connection, r: Range) -> rusqlite::Result<ScanResult> {
    let (_base, min_col, max_col) = col_sql(r.col);
    // 关键剪枝谓词下推到 SQL：min > high OR max < low 的块直接不返回。
    let sql = format!(
        "SELECT zone_id FROM zone_map WHERE NOT ({min_col} > ?1 OR {max_col} < ?2) ORDER BY zone_id"
    );
    let mut stmt = conn.prepare(&sql)?;
    let hit: Vec<i64> = stmt
        .query_map(params![r.high, r.low], |row| row.get(0))?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    let total: usize = conn.query_row("SELECT COUNT(*) FROM zone_map", [], |x| x.get(0))?;
    let skipped = total.saturating_sub(hit.len());
    Ok(ScanResult {
        hit_zones: hit,
        total_zones: total,
        skipped_zones: skipped,
    })
}

/// 计数：走 zone map 剪枝后回表，仅统计命中行。
pub fn count_range(conn: &Connection, r: Range) -> rusqlite::Result<CountResult> {
    let t0 = std::time::Instant::now();
    let scan = scan_zones(conn, r)?;
    let (base, _, _) = col_sql(r.col);
    let total: i64 = if scan.hit_zones.is_empty() {
        0
    } else {
        // 每个命中块按其行区间回表，再套精确范围谓词。
        // 每个命中块用其 source_offset 行区间回表计数。
        // 行区间而非 id 区间：zone_map 记录的是源偏移，而 source_offset 有索引。
        let sql = format!(
            "SELECT COUNT(*) FROM certificates \
             WHERE source_offset >= ?1 AND source_offset <= ?2 AND {base} BETWEEN ?3 AND ?4"
        );
        let mut stmt = conn.prepare(&sql)?;
        let mut sum = 0i64;
        let mut range_stmt =
            conn.prepare("SELECT row_min, row_max FROM zone_map WHERE zone_id = ?1")?;
        for zid in &scan.hit_zones {
            let (rmin, rmax): (i64, i64) =
                range_stmt.query_row(params![zid], |x| Ok((x.get(0)?, x.get(1)?)))?;
            sum += stmt.query_row(params![rmin, rmax, r.low, r.high], |x| x.get::<_, i64>(0))?;
        }
        sum
    };
    Ok(CountResult {
        count: total,
        skipped_zones: scan.skipped_zones,
        total_zones: scan.total_zones,
        hit_zones: scan.hit_zones.len(),
        elapsed_us: t0.elapsed().as_micros() as u64,
    })
}

/// 计数结果，附带剪枝效果与耗时，便于验证性能约束。
#[derive(Debug, Clone, Copy)]
pub struct CountResult {
    pub count: i64,
    pub skipped_zones: usize,
    pub total_zones: usize,
    pub hit_zones: usize,
    pub elapsed_us: u64,
}

impl CountResult {
    pub fn skip_ratio(&self) -> f64 {
        if self.total_zones == 0 {
            return 1.0;
        }
        self.skipped_zones as f64 / self.total_zones as f64
    }
}

/// 从 `certificates` 全量重建 zone_map（摄入完成后或 schema 变更后调用）。
pub fn rebuild(store: &mut Store) -> rusqlite::Result<usize> {
    store.conn().execute("DELETE FROM zone_map", [])?;
    let mut builder = ZoneBuilder::new();
    let mut zones = 0usize;
    let sql = "SELECT source_offset, not_before, not_after, key_size \
               FROM certificates ORDER BY id ASC";
    // 先把 (offset, nb, na, ks) 全部读入内存再写 zone_map：
    // append_zone 需要 &mut Store，而 prepare 会长期持有 &Connection。
    // 每行 4 个 i64 = 32 字节；100 万行约 32MB，一次性建图可接受，
    // 且避免逐行回查造成的 N+1 查询。
    let tuples: Vec<(i64, i64, i64, i64)> = {
        let mut stmt = store.conn().prepare(sql)?;
        let mut rows = stmt.query([])?;
        let mut v = Vec::new();
        while let Some(row) = rows.next()? {
            v.push((row.get(0)?, row.get(1)?, row.get(2)?, row.get(3)?));
        }
        v
    };
    for (off, nb, na, ks) in &tuples {
        let r = CertRow {
            digest: [0; 32],
            serial: Vec::new(),
            issuer: String::new(),
            subject: String::new(),
            not_before: *nb,
            not_after: *na,
            key_size: *ks,
            sig_algo: String::new(),
            self_signed: false,
            source_offset: *off,
        };
        if builder.push(&r) {
            let z = builder.finish();
            store.append_zone(&z)?;
            zones += 1;
            builder = ZoneBuilder::new();
        }
    }
    drop(tuples);
    if !builder.is_empty() {
        let z = builder.finish();
        store.append_zone(&z)?;
        zones += 1;
    }
    Ok(zones)
}

/// 块级统计（供 CLI `zones` 子命令展示）。
pub fn zone_stats(conn: &Connection) -> rusqlite::Result<(usize, i64)> {
    let n: usize = conn.query_row("SELECT COUNT(*) FROM zone_map", [], |x| x.get(0))?;
    let rows: i64 = conn.query_row("SELECT IFNULL(SUM(row_count),0) FROM zone_map", [], |x| x.get(0))?;
    Ok((n, rows))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::store;

    fn mkrow(i: i64, nb: i64, na: i64, ks: i64) -> CertRow {
        // digest 必须逐行唯一：直接用 [i as u8;32] 会让 i=0/256/512 撞唯一索引，
        // 使测试数据被去重成 256 行，掩盖真实的块统计行为。
        let mut digest = [0u8; 32];
        digest[..8].copy_from_slice(&i.to_le_bytes());
        CertRow {
            digest,
            serial: vec![i as u8],
            issuer: "CN=CA".into(),
            subject: format!("CN=s{i}"),
            not_before: nb,
            not_after: na,
            key_size: ks,
            sig_algo: "sha256".into(),
            self_signed: false,
            source_offset: i * 100,
        }
    }

    fn setup(n: i64) -> (tempfile::TempDir, Store) {
        let d = tempfile::tempdir().unwrap();
        let mut store = store::open(&d.path().join("z.db")).unwrap();
        let mut rows = Vec::new();
        for i in 0..n {
            // not_after 随行号单调递增，便于验证剪枝正确性。
            rows.push(mkrow(i, 1_000_000 + i, 1_700_000_000 + i, 1024 + (i % 4) * 512));
        }
        // 分批提交以贴近真实摄入路径。
        for chunk in rows.chunks(1000) {
            store.commit_batch(chunk, &[]).unwrap();
        }
        rebuild(&mut store).unwrap();
        (d, store)
    }

    #[test]
    fn builder_emits_blocks_of_4096() {
        let mut b = ZoneBuilder::new();
        // 前 BLOCK_ROWS-1 行不满块，第 BLOCK_ROWS 行（即第 4096 行）恰好满块。
        for i in 0..BLOCK_ROWS - 1 {
            assert!(!b.push(&mkrow(i, 0, 0, 0)), "第 {} 行不应满块", i + 1);
        }
        assert!(b.push(&mkrow(BLOCK_ROWS - 1, 0, 0, 0)), "第 4096 行应满块");
        let z = b.finish();
        assert_eq!(z.row_count, BLOCK_ROWS);
        // 满块后应重置，否则下一块会串统计
        let b2 = ZoneBuilder::new();
        assert_eq!(b2.count(), 0);
    }

    #[test]
    fn count_matches_ground_truth() {
        // 10000 行 -> 3 块（4096+4096+1808）
        let (_d, store) = setup(10_000);
        // 全区间：应等于总行数
        let all = count_range(store.conn(), Range::not_after(i64::MIN, i64::MAX)).unwrap();
        assert_eq!(all.count, 10_000);
        // 窄区间：not_after 落在 [1_700_005_000, 1_700_005_100]
        let lo = 1_700_005_000i64;
        let hi = 1_700_005_100i64;
        let narrow = count_range(store.conn(), Range::not_after(lo, hi)).unwrap();
        let expected = store
            .conn()
            .query_row(
                "SELECT COUNT(*) FROM certificates WHERE not_after BETWEEN ?1 AND ?2",
                params![lo, hi],
                |x| x.get::<_, i64>(0),
            )
            .unwrap();
        assert_eq!(narrow.count, expected, "zone map 结果必须与 SQL 真值一致");
        assert!(expected > 0, "测试区间应命中数据");
    }

    #[test]
    fn empty_range_returns_zero() {
        let (_d, store) = setup(5_000);
        let r = count_range(store.conn(), Range::not_after(1, 2)).unwrap();
        assert_eq!(r.count, 0);
    }

    #[test]
    fn skip_ratio_meets_target() {
        // 构造 10 万行，覆盖约 25 块，查询一个窄的 notAfter 窗口
        let (_d, store) = setup(100_000);
        let r = count_range(store.conn(), Range::not_after(1_700_050_000, 1_700_050_010)).unwrap();
        assert!(
            r.skip_ratio() >= 0.90,
            "块跳过率应 >=90%，实际 {:.2}% (跳 {}/{})",
            r.skip_ratio() * 100.0,
            r.skipped_zones,
            r.total_zones
        );
    }

    #[test]
    fn keysize_range_uses_zone_map() {
        let (_d, store) = setup(20_000);
        // key_size 只有 1024/1536/2048/2560 四个取值；查 [0,1200] 只命中 1024 的块
        let r = count_range(store.conn(), Range::key_size(0, 1200)).unwrap();
        let expected = store
            .conn()
            .query_row(
                "SELECT COUNT(*) FROM certificates WHERE key_size BETWEEN 0 AND 1200",
                [],
                |x| x.get::<_, i64>(0),
            )
            .unwrap();
        assert_eq!(r.count, expected);
    }

    #[test]
    fn zone_stats_reports_blocks() {
        let (_d, store) = setup(10_000);
        let (zones, rows) = zone_stats(store.conn()).unwrap();
        assert_eq!(zones, 3);
        assert_eq!(rows, 10_000);
    }
}
