//! rusqlite schema 与事务封装。
//!
//! 单文件 SQLite（`bundled` 静态编译，零外部进程）。
//! 关键性能设定：`journal_mode=WAL` + `synchronous=NORMAL`。
//! 注意 checkpoint 落盘由 [`crate::checkpoint`] 以 fsync 独立保证，
//! 因此这里可以放心用 NORMAL 而非 FULL——避免每事务两次 fsync。

#[cfg(test)]
use rusqlite::Transaction;
use rusqlite::{params, Connection, OpenFlags};

/// 一条待入库的证书记录（已通过 DER 与字段解析）。
#[derive(Debug, Clone)]
pub struct CertRow {
    pub digest: [u8; 32],
    pub serial: Vec<u8>,
    pub issuer: String,
    pub subject: String,
    pub not_before: i64,
    pub not_after: i64,
    pub key_size: i64,
    pub sig_algo: String,
    pub self_signed: bool,
    /// 在源文件中的绝对偏移，便于回溯定位。
    pub source_offset: i64,
}

/// 一个坏记录（DER 损坏），仅计数并可选留存转储。
#[derive(Debug, Clone)]
pub struct BadRecord {
    pub offset: i64,
    pub kind: String,
    pub detail: String,
    pub hex_dump: String,
}

/// 数据库句柄。
pub struct Store {
    conn: Connection,
}

/// 打开（必要时创建）数据库并应用 schema 与 PRAGMA。
pub fn open(path: &std::path::Path) -> rusqlite::Result<Store> {
    let conn = Connection::open_with_flags(
        path,
        OpenFlags::SQLITE_OPEN_READ_WRITE
            | OpenFlags::SQLITE_OPEN_CREATE
            | OpenFlags::SQLITE_OPEN_NO_MUTEX,
    )?;
    // WAL 让读不阻塞写；NORMAL 在 WAL 下已能保证崩溃不丢已提交事务。
    conn.pragma_update(None, "journal_mode", "WAL")?;
    conn.pragma_update(None, "synchronous", "NORMAL")?;
    // 负数 = KB；设为较大缓存以满足"扫百万行"性能目标。
    // 常驻内存预算 64MB：page cache 与 mmap 都计入 RSS，两者必须分摊而不是各自吃满。
    // cache_size 为负数表示 KB，故 -8_000 = 8MB；mmap 取 16MB。
    // （曾设 cache=64MB + mmap=256MB，实测百万级摄入峰值 RSS 达 344MB，超限。）
    conn.pragma_update(None, "cache_size", -8_000i64)?; // 8MB page cache
    conn.pragma_update(None, "temp_store", "MEMORY")?;
    conn.pragma_update(None, "mmap_size", 16 * 1024 * 1024i64)?; // 16MB
    let store = Store { conn };
    store.init_schema()?;
    Ok(store)
}

impl Store {
    pub fn conn(&self) -> &Connection {
        &self.conn
    }

    fn init_schema(&self) -> rusqlite::Result<()> {
        self.conn.execute_batch(
            r#"
            CREATE TABLE IF NOT EXISTS certificates (
                id            INTEGER PRIMARY KEY,
                digest        BLOB NOT NULL UNIQUE,   -- sha256(raw DER)，唯一索引即去重闸门
                serial        BLOB NOT NULL,
                issuer        TEXT NOT NULL,
                subject       TEXT NOT NULL,
                not_before    INTEGER NOT NULL,        -- Unix 秒
                not_after     INTEGER NOT NULL,
                key_size      INTEGER NOT NULL,
                sig_algo      TEXT NOT NULL,
                self_signed   INTEGER NOT NULL,
                source_offset INTEGER NOT NULL
            );

            -- 范围审计主路径：not_after 范围扫描
            CREATE INDEX IF NOT EXISTS idx_cert_not_after ON certificates(not_after);
            CREATE INDEX IF NOT EXISTS idx_cert_issuer     ON certificates(issuer);
            CREATE INDEX IF NOT EXISTS idx_cert_key_size   ON certificates(key_size);

            -- Zone Map：每块一行的 min/max 统计
            CREATE TABLE IF NOT EXISTS zone_map (
                zone_id      INTEGER PRIMARY KEY,
                row_min      INTEGER NOT NULL,
                row_max      INTEGER NOT NULL,
                nb_min       INTEGER NOT NULL,
                nb_max       INTEGER NOT NULL,
                na_min       INTEGER NOT NULL,
                na_max       INTEGER NOT NULL,
                ks_min       INTEGER NOT NULL,
                ks_max       INTEGER NOT NULL,
                row_count    INTEGER NOT NULL
            );

            -- 坏记录台账：定位 + 转储，仅计数不中断批次
            CREATE TABLE IF NOT EXISTS bad_records (
                id         INTEGER PRIMARY KEY,
                source_offset INTEGER NOT NULL,
                kind       TEXT NOT NULL,
                detail     TEXT NOT NULL,
                hex_dump   TEXT NOT NULL
            );

            -- 摄入元信息：记录已处理文件与总批次，供幂等/审计追溯
            CREATE TABLE IF NOT EXISTS ingest_files (
                path        TEXT PRIMARY KEY,
                total_size  INTEGER NOT NULL,
                mtime       INTEGER NOT NULL,
                last_offset INTEGER NOT NULL DEFAULT 0
            );
            "#,
        )
    }

    /// 在单个事务内写入整批。**全有或全无**：
    /// 任一语句失败则整个批次回滚，不留半批。
    pub fn commit_batch(
        &mut self,
        certs: &[CertRow],
        bads: &[BadRecord],
    ) -> rusqlite::Result<BatchOutcome> {
        let tx = self.conn.transaction()?;
        let mut inserted = 0u64;
        let mut duplicates = 0u64;

        {
            let mut ins_cert = tx.prepare(
                "INSERT OR IGNORE INTO certificates
                 (digest, serial, issuer, subject, not_before, not_after, key_size,
                  sig_algo, self_signed, source_offset)
                 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10)",
            )?;
            for c in certs {
                let n = ins_cert.execute(params![
                    c.digest.as_ref(),
                    c.serial,
                    c.issuer,
                    c.subject,
                    c.not_before,
                    c.not_after,
                    c.key_size,
                    c.sig_algo,
                    c.self_signed as i64,
                    c.source_offset,
                ])?;
                // sqlite3_changes() 返回 1 插入 / 0 命中唯一约束(即重复)。
                if n == 1 {
                    inserted += 1;
                } else {
                    duplicates += 1;
                }
            }

            if !bads.is_empty() {
                let mut ins_bad = tx.prepare(
                    "INSERT INTO bad_records (source_offset, kind, detail, hex_dump)
                     VALUES (?1, ?2, ?3, ?4)",
                )?;
                for b in bads {
                    ins_bad.execute(params![b.offset, b.kind, b.detail, b.hex_dump])?;
                }
            }
        }

        tx.commit()?;
        Ok(BatchOutcome {
            inserted,
            duplicates,
            bad: bads.len() as u64,
        })
    }

    /// 追加一个已提交区间到 zone_map（与数据同事务）。
    pub fn append_zone(&mut self, z: &ZoneRow) -> rusqlite::Result<()> {
        self.conn.execute(
            "INSERT INTO zone_map
             (row_min,row_max,nb_min,nb_max,na_min,na_max,ks_min,ks_max,row_count)
             VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9)",
            params![
                z.row_min, z.row_max, z.nb_min, z.nb_max, z.na_min, z.na_max, z.ks_min,
                z.ks_max, z.row_count
            ],
        )?;
        Ok(())
    }

    pub fn next_zone_id(&self) -> rusqlite::Result<i64> {
        let v: i64 = self.conn.query_row("SELECT IFNULL(MAX(zone_id),-1)+1 FROM zone_map", [], |r| r.get(0))?;
        Ok(v)
    }

    pub fn cert_count(&self) -> rusqlite::Result<i64> {
        self.conn.query_row("SELECT COUNT(*) FROM certificates", [], |r| r.get(0))
    }

    pub fn bad_count(&self) -> rusqlite::Result<i64> {
        self.conn.query_row("SELECT COUNT(*) FROM bad_records", [], |r| r.get(0))
    }

    /// 供测试/校验：事务包装器确保回滚语义。
    #[cfg(test)]
    pub fn with_tx<T>(&mut self, f: impl FnOnce(&Transaction) -> rusqlite::Result<T>) -> rusqlite::Result<T> {
        let tx = self.conn.transaction()?;
        let out = f(&tx)?;
        tx.commit()?;
        Ok(out)
    }
}

/// 单批次摄入结果。
#[derive(Debug, Clone, Copy, Default)]
pub struct BatchOutcome {
    pub inserted: u64,
    pub duplicates: u64,
    pub bad: u64,
}

/// Zone Map 单行统计。
#[derive(Debug, Clone, Copy)]
pub struct ZoneRow {
    pub row_min: i64,
    pub row_max: i64,
    pub nb_min: i64,
    pub nb_max: i64,
    pub na_min: i64,
    pub na_max: i64,
    pub ks_min: i64,
    pub ks_max: i64,
    pub row_count: i64,
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tmpdb() -> Store {
        let dir = tempfile::tempdir().unwrap();
        let p = dir.path().join("t.db");
        // 目录随 tmpdb 句柄丢弃；用 leak 保证测试期间存活。
        Box::leak(Box::new(dir));
        open(&p).unwrap()
    }

    fn row(digest_byte: u8, na: i64, ks: i64) -> CertRow {
        CertRow {
            digest: [digest_byte; 32],
            serial: vec![1, 2, 3],
            issuer: "CN=Test CA".into(),
            subject: "CN=leaf".into(),
            not_before: 1_700_000_000,
            not_after: na,
            key_size: ks,
            sig_algo: "1.2.840.113549.1.1.11".into(),
            self_signed: false,
            source_offset: 0,
        }
    }

    #[test]
    fn inserts_and_dedups() {
        let mut s = tmpdb();
        let batch = vec![row(1, 100, 2048), row(2, 200, 1024)];
        let o = s.commit_batch(&batch, &[]).unwrap();
        assert_eq!(o.inserted, 2);
        assert_eq!(o.duplicates, 0);

        // 同批重放：全部命中唯一索引，零新增
        let o2 = s.commit_batch(&batch, &[]).unwrap();
        assert_eq!(o2.inserted, 0);
        assert_eq!(o2.duplicates, 2);
        assert_eq!(s.cert_count().unwrap(), 2);
    }

    #[test]
    fn duplicate_digest_never_overwrites() {
        let mut s = tmpdb();
        s.commit_batch(&[row(1, 100, 2048)], &[]).unwrap();
        // 同 digest 但字段不同：INSERT OR IGNORE 应完整丢弃，不做部分更新。
        let o = s.commit_batch(&[row(1, 999, 128)], &[]).unwrap();
        assert_eq!(o.duplicates, 1, "重复 digest 不应覆盖原记录");
        assert_eq!(o.inserted, 0);
        assert_eq!(s.cert_count().unwrap(), 1);
        // 原值应保持不变
        let na: i64 = s
            .conn()
            .query_row("SELECT not_after FROM certificates WHERE digest=?1", params![[1u8;32]], |r| r.get(0))
            .unwrap();
        assert_eq!(na, 100, "重复投递不得改写已有记录");
    }

    #[test]
    fn transaction_rolls_back_entirely_on_error() {
        let mut s = tmpdb();
        s.commit_batch(&[row(1, 100, 2048)], &[]).unwrap();
        let before = s.cert_count().unwrap();

        // 手工构造一个中途失败的事务：先插入合法行，再插入违反 NOT NULL 的行。
        let res: rusqlite::Result<()> = s.with_tx(|tx| {
            tx.execute(
                "INSERT INTO certificates
                 (digest,serial,issuer,subject,not_before,not_after,key_size,sig_algo,self_signed,source_offset)
                 VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10)",
                params![[2u8;32], vec![9u8], "CN=X", "CN=Y", 1i64, 2i64, 2048i64, "sha256", 0i64, 0i64],
            )?;
            // issuer 为 NULL -> 违反 NOT NULL，整批应回滚
            tx.execute(
                "INSERT INTO certificates
                 (digest,serial,issuer,subject,not_before,not_after,key_size,sig_algo,self_signed,source_offset)
                 VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10)",
                params![[3u8;32], vec![9u8], rusqlite::types::Null, "CN=Y", 1i64, 2i64, 2048i64, "sha256", 0i64, 0i64],
            )?;
            Ok(())
        });
        assert!(res.is_err(), "预期因 NOT NULL 违约而失败");
        assert_eq!(
            s.cert_count().unwrap(),
            before,
            "事务失败后必须整批回滚，不留半批数据"
        );
    }

    #[test]
    fn bad_records_persisted() {
        let mut s = tmpdb();
        let bads = vec![BadRecord {
            offset: 42,
            kind: "truncated".into(),
            detail: "TLV 截断".into(),
            hex_dump: "30 0a".into(),
        }];
        let o = s.commit_batch(&[row(7, 10, 2048)], &bads).unwrap();
        assert_eq!(o.bad, 1);
        assert_eq!(s.bad_count().unwrap(), 1);
    }

    #[test]
    fn empty_batch_ok() {
        let mut s = tmpdb();
        let o = s.commit_batch(&[], &[]).unwrap();
        assert_eq!(o.inserted, 0);
    }
}
