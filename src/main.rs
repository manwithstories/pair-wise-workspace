//! CLI 入口：串接 解析 → 摄入 → 建图 → 查询。
//!
//! 无界面，全部在终端内运行。

use cert_audit::checkpoint::{self, CheckpointFile};
use cert_audit::store;
use cert_audit::zone_map;
use cert_audit::ingest::{self, IngestConfig};
use clap::{Parser, Subcommand};
use std::path::PathBuf;
use std::time::Instant;

/// 解析 `YYYY-MM-DD` 或 Unix 秒为 Unix 秒。
fn parse_date(s: &str) -> Result<i64, String> {
    let t = s.trim();
    if let Ok(n) = t.parse::<i64>() {
        return Ok(n);
    }
    let parts: Vec<&str> = t.split('-').collect();
    if parts.len() != 3 {
        return Err(format!("无法解析日期 `{s}`，应为 YYYY-MM-DD 或 Unix 秒"));
    }
    let y: i64 = parts[0].parse().map_err(|_| format!("年份非法: {}", parts[0]))?;
    let m: i64 = parts[1].parse().map_err(|_| format!("月份非法: {}", parts[1]))?;
    let d: i64 = parts[2].parse().map_err(|_| format!("日期非法: {}", parts[2]))?;
    if !(1..=12).contains(&m) || !(1..=31).contains(&d) {
        return Err(format!("日期越界: {s}"));
    }
    Ok(days_from_civil(y, m, d) * 86400)
}

/// Howard Hinnant days_from_civil。
fn days_from_civil(y: i64, m: i64, d: i64) -> i64 {
    let y = if m <= 2 { y - 1 } else { y };
    let era = if y >= 0 { y } else { y - 399 } / 400;
    let yoe = y - era * 400;
    let mp = (m + 9) % 12;
    let doy = (153 * mp + 2) / 5 + d - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    era * 146097 + doe - 719468
}

/// Unix 秒 → `YYYY-MM-DD`。
fn fmt_date(ts: i64) -> String {
    let days = ts.div_euclid(86400);
    let z = days + 719468;
    let era = if z >= 0 { z } else { z - 146096 } / 146097;
    let doe = z - era * 146097;
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    let y = if m <= 2 { y + 1 } else { y };
    format!("{y:04}-{m:02}-{d:02}")
}

/// 解析 `LOW,HIGH`，缺省 HIGH 为 i64::MAX。
fn parse_range(s: &str) -> Result<(i64, i64), String> {
    match s.split_once(',') {
        Some((a, b)) => Ok((parse_date(a)?, parse_date(b)?)),
        None => {
            let lo = parse_date(s)?;
            Ok((lo, i64::MAX))
        }
    }
}

#[derive(Parser, Debug)]
#[command(
    name = "cert-audit",
    version,
    about = "金融 PKI 合规审计：DER X.509 证书批量入库与范围审计"
)]
struct Cli {
    #[command(subcommand)]
    cmd: Cmd,
}

#[derive(Subcommand, Debug)]
enum Cmd {
    /// 从目录摄入 DER 证书流（幂等 + 断点续传）
    Ingest {
        /// 证书流目录（扫描 .der/.cer/.crt）
        #[arg(long, default_value = "./fixtures")]
        dir: PathBuf,
        /// 数据库路径
        #[arg(long, default_value = "./audit.db")]
        db: PathBuf,
        /// 每批记录数
        #[arg(long, default_value_t = 1000)]
        batch: usize,
        /// 忽略断点，从头重跑
        #[arg(long, default_value_t = false)]
        restart: bool,
        /// 每累计多少个事务落一次断点（越大越快；崩溃后最多重扫该数量的批，
        /// 由 sha256 去重兜底，不会重复入库）
        #[arg(long, default_value_t = 1)]
        ckpt_every: u32,
        /// 只摄入此文件（跳过目录扫描）
        #[arg(long)]
        file: Option<PathBuf>,
    },
    /// 范围查询（走 zone map 块级跳过）
    Query {
        /// 数据库路径
        #[arg(long, default_value = "./audit.db")]
        db: PathBuf,
        /// notAfter 范围，格式 LOW,HIGH（YYYY-MM-DD）
        #[arg(long)]
        not_after_range: Option<String>,
        /// notBefore 范围，格式 LOW,HIGH
        #[arg(long)]
        not_before_range: Option<String>,
        /// 密钥强度范围（位），格式 LOW,HIGH
        #[arg(long)]
        key_size_range: Option<String>,
        /// 只输出计数
        #[arg(long, default_value_t = false)]
        count: bool,
        /// 最多列出多少行明细
        #[arg(long, default_value_t = 20)]
        limit: usize,
        /// 只列出弱密钥（低于此位数）
        #[arg(long)]
        weak_key_below: Option<i64>,
        /// 只列出此日期之前过期的
        #[arg(long)]
        expiring_before: Option<String>,
    },
    /// 重建 zone map
    Rebuild {
        #[arg(long, default_value = "./audit.db")]
        db: PathBuf,
    },
    /// 查看坏记录台账
    Bad {
        #[arg(long, default_value = "./audit.db")]
        db: PathBuf,
        #[arg(long, default_value_t = 20)]
        limit: usize,
    },
}

fn main() {
    if let Err(e) = run() {
        eprintln!("error: {e}");
        std::process::exit(1);
    }
}

fn run() -> Result<(), Box<dyn std::error::Error>> {
    let cli = Cli::parse();
    match cli.cmd {
        Cmd::Ingest {
            dir,
            db,
            batch,
            restart,
            file,
            ckpt_every,
        } => cmd_ingest(dir, db, batch, restart, file, ckpt_every),
        Cmd::Query {
            db,
            not_after_range,
            not_before_range,
            key_size_range,
            count,
            limit,
            weak_key_below,
            expiring_before,
        } => cmd_query(
            db,
            not_after_range,
            not_before_range,
            key_size_range,
            count,
            limit,
            weak_key_below,
            expiring_before,
        ),
        Cmd::Rebuild { db } => {
            let mut s = store::open(&db)?;
            let t = Instant::now();
            let n = zone_map::rebuild(&mut s)?;
            println!(
                "重建 zone map：{n} 块，覆盖 {} 行，耗时 {:.2}s",
                s.cert_count()?,
                t.elapsed().as_secs_f64()
            );
            Ok(())
        }
        Cmd::Bad { db, limit } => {
            let s = store::open(&db)?;
            let mut stmt = s
                .conn()
                .prepare("SELECT source_offset, kind, detail, hex_dump FROM bad_records ORDER BY id LIMIT ?1")?;
            let rows = stmt.query_map([limit as i64], |r| {
                Ok((
                    r.get::<_, i64>(0)?,
                    r.get::<_, String>(1)?,
                    r.get::<_, String>(2)?,
                    r.get::<_, String>(3)?,
                ))
            })?;
            let mut n = 0;
            for r in rows {
                let (off, kind, detail, dump) = r?;
                n += 1;
                println!("偏移 0x{off:08x}  [{kind}]");
                println!("  {detail}");
                for line in dump.lines() {
                    println!("  {line}");
                }
                println!();
            }
            if n == 0 {
                println!("无坏记录。");
            }
            Ok(())
        }
    }
}

fn cmd_ingest(
    dir: PathBuf,
    db: PathBuf,
    batch: usize,
    restart: bool,
    file: Option<PathBuf>,
    ckpt_every: u32,
) -> Result<(), Box<dyn std::error::Error>> {
    let files: Vec<PathBuf> = match file {
        Some(f) => vec![f],
        None => {
            let mut v = Vec::new();
            for e in std::fs::read_dir(&dir)? {
                let p = e?.path();
                let ext = p.extension().and_then(|x| x.to_str()).unwrap_or("");
                if matches!(ext, "der" | "cer" | "crt") {
                    v.push(p);
                }
            }
            v.sort();
            v
        }
    };
    if files.is_empty() {
        return Err(format!("目录 {} 下没有 .der/.cer/.crt 文件", dir.display()).into());
    }

    let mut s = store::open(&db)?;
    let ckpt_file = CheckpointFile::new(&db);
    let cfg = IngestConfig {
        batch_size: batch.max(1),
        ckpt_interval: ckpt_every.max(1),
        ..Default::default()
    };

    println!(
        "摄入开始：{} 个文件，批量 {}，断点间隔 {} 批，数据库 {}",
        files.len(),
        cfg.batch_size,
        cfg.ckpt_interval,
        db.display()
    );

    let t_all = Instant::now();
    let mut total_ins = 0u64;
    let mut total_dup = 0u64;
    let mut total_bad = 0u64;

    for path in &files {
        let size = std::fs::metadata(path)?.len();
        let mtime = checkpoint::file_mtime(path);

        // 断点决策
        let mut start = 0u64;
        let mut resumed = false;
        if !restart {
            if let Some(cp) = ckpt_file.load()? {
                if checkpoint::resumable(&cp, path, size, mtime) && cp.offset > 0 {
                    start = cp.offset;
                    resumed = true;
                }
            }
        }

        let t = Instant::now();
        let st = ingest::ingest_file(&mut s, &ckpt_file, path, &cfg, start)?;
        let el = t.elapsed();

        total_ins += st.inserted;
        total_dup += st.duplicates;
        total_bad += st.bad;

        println!(
            "  {} 记录 {}  新增 {}  重复 {}  坏 {} 跳过 {}B 批次 {} 断点{}  {}{}  {:.3}s",
            path.file_name().unwrap_or_default().to_string_lossy(),
            st.records_seen,
            st.inserted,
            st.duplicates,
            st.bad,
            st.skipped_bytes,
            st.batches,
            st.checkpoint_writes,
            if resumed {
                format!("[续传@{}] ", start)
            } else {
                String::new()
            },
            if st.bad > 0 {
                format!("({})", st.bad_summary())
            } else {
                String::new()
            },
            el.as_secs_f64()
        );

        // 文件读完即清除断点，避免下次误续。
        // 必须在最后一笔事务提交之后才清除。
        ckpt_file.clear()?;
    }

    // 建 zone map
    let t = Instant::now();
    let zones = zone_map::rebuild(&mut s)?;
    println!(
        "zone map：{} 块（每块 {} 行），耗时 {:.3}s",
        zones,
        zone_map::BLOCK_ROWS,
        t.elapsed().as_secs_f64()
    );

    let total = t_all.elapsed().as_secs_f64();
    let rate = if total > 0.0 {
        (total_ins + total_dup + total_bad) as f64 / total
    } else {
        0.0
    };
    println!(
        "\n完成：入库 {} 张，去重跳过 {} 张，坏记录 {} 条，合计 {:.2}s，吞吐 {:.0} 记录/秒",
        total_ins, total_dup, total_bad, total, rate
    );
    println!(
        "库内证书 {} 张 / 坏记录 {} 条",
        s.cert_count()?,
        s.bad_count()?
    );
    if let Some(mb) = peak_rss_mb() {
        println!("摄入峰值常驻内存：{mb:.1} MB（预算 64 MB）");
    }
    Ok(())
}

#[allow(clippy::too_many_arguments)]
fn cmd_query(
    db: PathBuf,
    not_after_range: Option<String>,
    not_before_range: Option<String>,
    key_size_range: Option<String>,
    count: bool,
    limit: usize,
    weak_key_below: Option<i64>,
    expiring_before: Option<String>,
) -> Result<(), Box<dyn std::error::Error>> {
    let s = store::open(&db)?;
    let total = s.cert_count()?;

    // 构造 WHERE 片段与 zone map 扫描范围
    let mut where_sql = String::new();
    let mut args: Vec<Box<dyn rusqlite::ToSql>> = Vec::new();
    let mut zones: Vec<zone_map::Range> = Vec::new();

    // 追加一个区间谓词 col BETWEEN lo AND hi，并绑定两个参数。
    // 占位符编号由参数实际长度推导，杜绝编号与参数个数脱节。
    fn push_between(
        where_sql: &mut String,
        args: &mut Vec<Box<dyn rusqlite::ToSql>>,
        col: &str,
        lo: i64,
        hi: i64,
    ) {
        where_sql.push_str(if where_sql.is_empty() { " WHERE " } else { " AND " });
        let (p1, p2) = (args.len() + 1, args.len() + 2);
        where_sql.push_str(&format!("{col} BETWEEN ?{p1} AND ?{p2}"));
        args.push(Box::new(lo));
        args.push(Box::new(hi));
    }

    // 追加单边谓词 col <op> v，绑定一个参数。
    fn push_cmp(
        where_sql: &mut String,
        args: &mut Vec<Box<dyn rusqlite::ToSql>>,
        col: &str,
        op: &str,
        v: i64,
    ) {
        where_sql.push_str(if where_sql.is_empty() { " WHERE " } else { " AND " });
        where_sql.push_str(&format!("{col} {op} ?{}", args.len() + 1));
        args.push(Box::new(v));
    }

    if let Some(r) = &not_after_range {
        let (lo, hi) = parse_range(r)?;
        push_between(&mut where_sql, &mut args, "not_after", lo, hi);
        zones.push(zone_map::Range::not_after(lo, hi));
    }
    if let Some(r) = &not_before_range {
        let (lo, hi) = parse_range(r)?;
        push_between(&mut where_sql, &mut args, "not_before", lo, hi);
        zones.push(zone_map::Range::not_before(lo, hi));
    }
    if let Some(r) = &key_size_range {
        let (lo, hi) = parse_range(r)?;
        push_between(&mut where_sql, &mut args, "key_size", lo, hi);
        zones.push(zone_map::Range::key_size(lo, hi));
    }
    if let Some(w) = weak_key_below {
        push_cmp(&mut where_sql, &mut args, "key_size", "<", w);
        zones.push(zone_map::Range::key_size(0, w));
    }
    if let Some(e) = &expiring_before {
        let ts = parse_date(e)?;
        push_cmp(&mut where_sql, &mut args, "not_after", "<", ts);
        zones.push(zone_map::Range::not_after(i64::MIN, ts));
    }

    if where_sql.is_empty() {
        where_sql.push_str(" WHERE 1=1");
    }

    // 报告 zone map 剪枝效果
    if !zones.is_empty() {
        let t = Instant::now();
        let mut skipped = 0usize;
        let mut total_z = 0usize;
        for z in &zones {
            let sc = zone_map::scan_zones(s.conn(), *z)?;
            skipped += sc.skipped_zones;
            total_z += sc.total_zones;
        }
        let ratio = if total_z == 0 {
            1.0
        } else {
            skipped as f64 / total_z as f64
        };
        println!(
            "zone map：扫描 {} 块，跳过 {} 块（{:.1}%），耗时 {:.3}ms",
            total_z,
            skipped,
            ratio * 100.0,
            t.elapsed().as_secs_f64() * 1000.0
        );
    }

    let sql_count = format!("SELECT COUNT(*) FROM certificates{where_sql}");
    let t = Instant::now();
    let refs: Vec<&dyn rusqlite::ToSql> = args.iter().map(|b| b.as_ref()).collect();
    let n: i64 = s.conn().query_row(&sql_count, refs.as_slice(), |r| r.get(0))?;
    let el = t.elapsed();

    if count {
        println!("{n}");
        if !zones.is_empty() {
            println!(
                "# 共 {} 张，命中 {} 张，zone 剪枝后回表，耗时 {:.3}ms",
                total,
                n,
                el.as_secs_f64() * 1000.0
            );
        }
        return Ok(());
    }

    println!("命中 {n} / 共 {total} 张，耗时 {:.3}ms", el.as_secs_f64() * 1000.0);
    if limit == 0 || n == 0 {
        return Ok(());
    }

    let sql = format!(
        "SELECT subject, issuer, not_before, not_after, key_size, sig_algo \
         FROM certificates{where_sql} ORDER BY not_after ASC LIMIT {}",
        limit
    );
    let mut stmt = s.conn().prepare(&sql)?;
    let rows = stmt.query_map(refs.as_slice(), |r| {
        Ok((
            r.get::<_, String>(0)?,
            r.get::<_, String>(1)?,
            r.get::<_, i64>(2)?,
            r.get::<_, i64>(3)?,
            r.get::<_, i64>(4)?,
            r.get::<_, String>(5)?,
        ))
    })?;
    println!(
        "{:<38} {:<26} {:<12} {:<12} {:>6}  {}",
        "SUBJECT", "ISSUER", "NOT_BEFORE", "NOT_AFTER", "KEY", "SIG"
    );
    for r in rows {
        let (sub, iss, nb, na, ks, sig) = r?;
        println!(
            "{:<38} {:<26} {:<12} {:<12} {:>6}  {}",
            truncate(&sub, 38),
            truncate(&iss, 26),
            fmt_date(nb),
            fmt_date(na),
            ks,
            sig
        );
    }
    if n as usize > limit {
        println!("... 其余 {} 条未显示（--limit 调整）", n - limit as i64);
    }
    Ok(())
}

fn truncate(s: &str, n: usize) -> String {
    if s.chars().count() <= n {
        s.to_string()
    } else {
        s.chars().take(n - 1).collect::<String>() + "…"
    }
}

/// 本进程峰值常驻内存（MB），用于验证"百万级不超过 64MB"的约束。
///
/// Linux 可从 /proc/self/status 的 VmHWM 直接读取；
/// macOS 没有等价文本接口，返回 None（此时应由外部 `ps`/getrusage 观测）。
fn peak_rss_mb() -> Option<f64> {
    let status = std::fs::read_to_string("/proc/self/status").ok()?;
    for line in status.lines() {
        if let Some(v) = line.strip_prefix("VmHWM:") {
            let kb: u64 = v.trim().trim_end_matches("kB").trim().parse().ok()?;
            return Some(kb as f64 / 1024.0);
        }
    }
    None
}
