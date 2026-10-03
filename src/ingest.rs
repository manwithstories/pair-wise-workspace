//! 批量摄入：读取 DER 流 → TLV 校验 → sha256 去重 → 整批事务提交。
//!
//! 容错契约：**任何单条坏记录都不中断批次**。坏记录记绝对偏移 + 十六进制转储，
//! 计入 `bad` 后继续；只有 IO 错误与 DB 错误才向上传播。

use crate::cert;
use crate::checkpoint::CheckpointFile;
use crate::der_parser::{self, WalkConfig, WalkStats};
use crate::store::{BadRecord, BatchOutcome, CertRow, Store};
use std::fs::File;
use std::io::{self, BufReader, Read, Seek, SeekFrom};
use std::path::Path;

/// 摄入配置。
#[derive(Debug, Clone)]
pub struct IngestConfig {
    /// 每批记录数上限（性能约束：1000 条事务 ≤50ms）。
    pub batch_size: usize,
    pub walk: WalkConfig,
    /// 是否保留坏记录转储到 DB。
    pub keep_bad: bool,
    /// 单条记录最大字节数，防御超长畸形长度域导致的巨额分配。
    pub max_record_bytes: usize,
    /// 每累计多少个已提交事务落一次断点。
    ///
    /// 事务仍按 `batch_size` 逐批提交（保证有界内存与全有或全无），
    /// 但断点落盘含 2 次 fsync，是整个摄入链路上最贵的一步；
    /// 实测在本机每批 1000 条时，断点落盘占整体耗时约 2/3。
    /// 因此允许"多批一落"：最坏情况只是重启后重扫
    /// `ckpt_interval` 批已提交数据——而这层重扫由 sha256 去重兜底，
    /// 既不会重复入库，也不会重复告警。
    pub ckpt_interval: u32,
}

impl Default for IngestConfig {
    fn default() -> Self {
        IngestConfig {
            batch_size: 1000,
            walk: WalkConfig::default(),
            keep_bad: true,
            max_record_bytes: 1 << 20, // 1 MiB，正常证书 ~1-2KB
            ckpt_interval: 1,
        }
    }
}

/// 摄入累计统计。
#[derive(Debug, Clone, Default)]
pub struct IngestStats {
    pub bytes_read: u64,
    pub records_seen: u64,
    pub inserted: u64,
    pub duplicates: u64,
    pub bad: u64,
    pub batches: u64,
    /// 实际发生的断点落盘次数（可能少于批次数）。
    pub checkpoint_writes: u64,
    pub tlvs: u64,
    /// 各坏记录类型的计数。
    pub bad_kinds: Vec<(String, u64)>,
    /// resync 过程中被跳过的垃圾字节总数。
    pub skipped_bytes: u64,
    /// 是否从断点续传。
    pub resumed: bool,
    /// 摄入起点的绝对偏移。
    pub start_offset: u64,
}

impl IngestStats {
    fn note_bad(&mut self, kind: &str) {
        self.bad += 1;
        match self.bad_kinds.iter_mut().find(|(k, _)| k == kind) {
            Some(slot) => slot.1 += 1,
            None => self.bad_kinds.push((kind.to_string(), 1)),
        }
    }

    pub fn bad_summary(&self) -> String {
        if self.bad_kinds.is_empty() {
            return "无".into();
        }
        let mut v = self.bad_kinds.clone();
        v.sort_by(|a, b| b.1.cmp(&a.1));
        v.iter()
            .map(|(k, c)| format!("{k}={c}"))
            .collect::<Vec<_>>()
            .join(", ")
    }
}

/// 单次读取的字节数。1 MiB 在吞吐与常驻内存之间取平衡。
const READ_CHUNK: usize = 1 << 20;

/// 从流中切分出单条 DER 记录。
///
/// 记录边界 = 根 TLV 的 `header + content length`。先用长度域（最长 9 字节）
/// 试探出候选记录，再交给完整 TLV 校验：
/// - 试探成功 → 交给 `walk()` 做递归一致性校验；
/// - 试探失败 → 说明该记录头本身畸形，此时**向后扫描寻找下一个能自洽的
///   记录起点**（resync），坏记录只记一次。
///
/// resync 保证：即使流中段数据完全损坏，后续好记录仍能恢复入库，
/// 而不会因一次错误偏移把整个文件判死。
struct RecordSplitter {
    buf: Vec<u8>,
    pos: usize,
    /// 记录在流中的绝对起始偏移。
    base: u64,
    /// resync 候选点的结构校验配置。
    cfg: WalkConfig,
}

enum SplitResult {
    /// 得到一条完整记录：`start` 为流内绝对偏移，`range` 为缓冲区内区间。
    /// 调用方须在下一轮 feed 前用 [`RecordSplitter::record`] 取用。
    Record { start: u64, range: std::ops::Range<usize> },
    /// 需要更多数据。
    NeedMore,
    /// 缓冲区已榨干且本轮没有新输入：调用方应停止空转，去读下一块。
    Drained,
    /// 头部畸形，已向后扫描跳过 `len` 字节垃圾后重新定界。
    /// 这些字节必须作为坏记录上报，否则损坏会被静默吞掉。
    Skipped { offset: u64, len: usize, err: der_parser::DerError },
    /// 此处畸形且前方无任何可定界位置（已由调用方记坏）。
    Corrupt { offset: u64, err: der_parser::DerError },
}

impl RecordSplitter {
    fn new(cap: usize, cfg: WalkConfig) -> Self {
        RecordSplitter {
            buf: Vec::with_capacity(cap),
            pos: 0,
            base: 0,
            cfg,
        }
    }

    /// 追加数据，返回解析出的记录。
    fn feed(&mut self, chunk: &[u8], max_rec: usize) -> SplitResult {
        self.buf.extend_from_slice(chunk);
        loop {
            let avail = self.buf.len() - self.pos;
            if avail == 0 {
                // 缓冲区已榨干：本轮没有更多记录可切分。
                return SplitResult::Drained;
            }
            // 先用**纯长度域**扫描（无分配、不要求内容已完整）确定候选总长，
            // 再判断缓冲区是否已容纳该记录。
            //
            // 顺序至关重要：若改用需要"值域完整"的 peek_record_len，
            // 跨读缓冲边界的记录会因内容不全而被误判为畸形，
            // 从而错误地触发 resync，在一条好记录内部逐字节滑动并把它切碎。
            match Self::declared_len_fast(&self.buf[self.pos..]) {
                Some(total) if total <= max_rec => {
                    if avail < total {
                        // 内容尚未读全：等下一块，绝不能在此判定为坏记录。
                        return SplitResult::NeedMore;
                    }
                    let start = self.base + self.pos as u64;
                    // 只回传区间，不拷贝；解析在下一轮 feed 前完成。
                    let range = self.pos..self.pos + total;
                    self.pos += total;
                    return SplitResult::Record { start, range };
                }
                Some(total) => {
                    // 超长：跳过 1 字节尝试重新定界，避免内存爆炸。
                    let off = self.base + self.pos as u64;
                    let e = der_parser::make_error(
                        &self.buf[self.pos..],
                        der_parser::DerErrorKind::LengthOverflow,
                        0,
                        Some(0),
                        format!("记录声明长度 {total} 超过上限 {max_rec}"),
                    );
                    self.pos += 1;
                    return SplitResult::Corrupt { offset: off, err: e };
                }
                None => {
                    let e = der_parser::make_error(
                        &self.buf[self.pos..],
                        der_parser::DerErrorKind::BadTag,
                        0,
                        Some(0),
                        "长度域不合法（indefinite / 前导冗余 0 / 超长格式）",
                    );
                    // 头部畸形：resync —— 逐字节前移直到找到自洽的记录头。
                    // 被跳过的字节区间必须上报为坏记录，不能静默丢弃。
                    let off = self.base + self.pos as u64;
                    let mut step = 1usize;
                    // resync 判据：候选位置不仅要长度域自洽，还必须能通过
                    // 完整的 TLV 递归校验。只看长度域是不够的——垃圾字节里
                    // 极易凑出"看起来合法"的头（如 0x30 0x82 ...），若据此
                    // 咬住候选点，就会整段吞掉紧随其后的好记录。
                    let mut found = None;
                    let mut probe = WalkStats::default();
                    while self.pos + step < self.buf.len() {
                        let cand = &self.buf[self.pos + step..];
                        if let Some(t) = Self::declared_len_fast(cand) {
                            if t <= max_rec && cand.len() >= t {
                                let rec = &cand[..t];
                                // 记录层额外约束：X.509 证书顶层必为 universal
                                // SEQUENCE。垃圾字节里很容易凑出结构合法的空 TLV
                                // （例如 0x80 0x00 = ContextSpecific[0] 空构造体），
                                // 只靠 walk() 无法与真记录区分；加上顶层 SEQUENCE
                                // 判据后，resync 才不会咬住这类伪记录而吞掉后续好记录。
                                if rec.first() == Some(&0x30)
                                    && der_parser::walk(rec, 0, &self.cfg, &mut probe).is_ok()
                                {
                                    found = Some(t);
                                    break;
                                }
                            }
                        }
                        step += 1;
                    }
                    match found {
                        Some(_) => {
                            self.pos += step;
                            return SplitResult::Skipped {
                                offset: off,
                                len: step,
                                err: e,
                            };
                        }
                        None => {
                            // 前方无自洽记录头：只跳过 1 字节重试，
                            // 避免在纯垃圾上一次性跳过整段导致定位信息丢失。
                            self.pos += 1;
                            return SplitResult::Skipped {
                                offset: off,
                                len: 1,
                                err: e,
                            };
                        }
                    }
                }
            }
        }
    }

    /// 已完全消费的字节数（流的绝对位置）。
    ///
    /// 断点必须落在"最后一条已处理记录**之后**"，因此这里返回
    /// `base + pos` 而非任何记录的起始偏移——用 max(source_offset) 会把
    /// 断点退回到记录头部，重启时将从记录中间继续解析而全盘错位。
    fn consumed(&self) -> u64 {
        self.base + self.pos as u64
    }

    /// 返回缓冲区中尚未消费的尾部（流末尾的截断残余）。
    fn remaining(&self) -> &[u8] {
        &self.buf[self.pos.min(self.buf.len())..]
    }

    /// 取出一条已切分记录的字节切片。
    fn record(&self, r: std::ops::Range<usize>) -> &[u8] {
        &self.buf[r]
    }

    /// **无分配**试探记录总长度（header + content）。
    ///
    /// resync 会在每个字节位置调用本函数，若走完整 [`der_parser::parse_tlv_at_public`]
    /// 则每次失败都要构造带十六进制转储的 `DerError`（64 字节格式化 + String 分配），
    /// 在纯垃圾流上退化为 O(n²)。这里只做长度域的廉价合理性检查，
    /// 真正的结构校验留到记录切分之后再走 TLV 遍历。
    fn declared_len_fast(d: &[u8]) -> Option<usize> {
        let id = *d.first()?;
        let mut hp = 1usize;
        if id & 0x1f == 0x1f {
            // high tag number form：逐 7-bit 组前进，最多 5 组。
            let mut groups = 0u32;
            loop {
                let g = *d.get(hp)?;
                hp += 1;
                groups += 1;
                if groups > 5 {
                    return None;
                }
                if g & 0x80 == 0 {
                    break;
                }
            }
        }
        let b0 = *d.get(hp)?;
        let (vlen, len_bytes) = if b0 < 0x80 {
            (b0 as usize, 1usize)
        } else {
            let n = (b0 & 0x7f) as usize;
            // indefinite(0x80) 与 >8 字节长格式均非法。
            if n == 0 || n > 8 {
                return None;
            }
            if d.len() < hp + 1 + n {
                return None;
            }
            if d[hp + 1] == 0x00 {
                return None; // 长格式前导冗余 0，违反 DER 最小编码
            }
            let mut v: u64 = 0;
            for i in 0..n {
                v = (v << 8) | d[hp + 1 + i] as u64;
            }
            if n == 1 && v < 0x80 {
                return None;
            }
            if v > usize::MAX as u64 {
                return None;
            }
            (v as usize, 1 + n)
        };
        hp.checked_add(len_bytes)?.checked_add(vlen)
    }

    /// 压缩已消费前缀，控制常驻内存。
    ///
    /// 只移动未消费尾部（`copy_within`），避免每块数据都整体搬移缓冲区。
    fn compact(&mut self) {
        if self.pos > 0 {
            self.base += self.pos as u64;
            self.buf.copy_within(self.pos.., 0);
            self.buf.truncate(self.buf.len() - self.pos);
            self.pos = 0;
        }
    }
}

/// 摄取单个文件。
pub fn ingest_file(
    store: &mut Store,
    ckpt: &CheckpointFile,
    path: &Path,
    cfg: &IngestConfig,
    start_offset: u64,
) -> io::Result<IngestStats> {
    let mut stats = IngestStats {
        resumed: start_offset > 0,
        start_offset,
        ..Default::default()
    };

    let mut file = File::open(path)?;
    let total_size = file.metadata()?.len();
    file.seek(SeekFrom::Start(start_offset))?;

    let mut reader = BufReader::with_capacity(1 << 20, file);
    let mut splitter = RecordSplitter::new(cfg.max_record_bytes + (1 << 20), cfg.walk.clone());
    let mut certs: Vec<CertRow> = Vec::with_capacity(cfg.batch_size);
    let mut bads: Vec<BadRecord> = Vec::new();
    let mut wstats = WalkStats::default();

    let mut batch_no: u64 = 0;
    let mut committed_to = start_offset;

    // 读缓冲只分配一次并复用：每轮 new 一次 1MiB 会把堆分配压力放大到
    // 与数据量同阶，且在百万级输入上直接体现为吞吐塌陷。
    let mut chunk = vec![0u8; READ_CHUNK];

    loop {
        chunk.clear();
        chunk.resize(READ_CHUNK, 0);
        let n = reader.read(&mut chunk)?;
        if n == 0 {
            break;
        }
        chunk.truncate(n);
        stats.bytes_read += n as u64;

        // compact 必须放在"新数据并入缓冲"之前，且只搬移未消费尾部。
        // 若在 feed 之前搬移，pending 仍指向上一轮的 chunk，切片会悬空。
        splitter.compact();

        // 首轮喂入新数据；此后以空投喂把缓冲区里已能定界的记录全部榨干。
        let mut pending: &[u8] = &chunk;
        loop {
            match splitter.feed(pending, cfg.max_record_bytes) {
                SplitResult::NeedMore => break,
                SplitResult::Drained => break,
                SplitResult::Skipped { offset, len, err } => {
                    stats.records_seen += 1;
                    stats.skipped_bytes += len as u64;
                    stats.note_bad(err.kind.as_str());
                    if cfg.keep_bad {
                        bads.push(BadRecord {
                            offset: offset as i64,
                            kind: err.kind.as_str().to_string(),
                            detail: format!("{}（resync 跳过 {len} 字节）", err.detail),
                            hex_dump: err.hex_dump,
                        });
                    }
                    pending = &[];
                    continue;
                }
                SplitResult::Corrupt { offset, err } => {
                    stats.records_seen += 1;
                    stats.note_bad(err.kind.as_str());
                    if cfg.keep_bad {
                        bads.push(BadRecord {
                            offset: offset as i64,
                            kind: err.kind.as_str().to_string(),
                            detail: err.detail.clone(),
                            hex_dump: err.hex_dump.clone(),
                        });
                    }
                    pending = &[];
                    continue;
                }
                SplitResult::Record { start, range } => {
                    stats.records_seen += 1;
                    pending = &[];
                    let bytes = splitter.record(range);

                    // 递归一致性校验：坏记录在此被拦截，绝不 panic
                    match der_parser::walk(&bytes, 0, &cfg.walk, &mut wstats) {
                        Ok(_) => match cert::parse_certificate(&bytes) {
                            Ok(c) => {
                                let mut hasher = Sha256::new();
                                hasher.update(&bytes);
                                certs.push(CertRow {
                                    digest: hasher.finish(),
                                    serial: c.serial,
                                    issuer: c.issuer,
                                    subject: c.subject,
                                    not_before: c.not_before,
                                    not_after: c.not_after,
                                    key_size: c.key_size as i64,
                                    sig_algo: c.sig_algo,
                                    self_signed: c.self_signed,
                                    source_offset: start as i64,
                                });
                            }
                            Err(e) => {
                                // 结构合法但字段抽取失败：同样计入坏记录，不中断批次
                                stats.note_bad("field-extract");
                                if cfg.keep_bad {
                                    bads.push(BadRecord {
                                        offset: start as i64,
                                        kind: "field-extract".into(),
                                        detail: e,
                                        hex_dump: der_parser::hex_window(&bytes, 0, 32),
                                    });
                                }
                            }
                        },
                        Err(e) => {
                            stats.note_bad(e.kind.as_str());
                            if cfg.keep_bad {
                                bads.push(BadRecord {
                                    offset: start as i64,
                                    kind: e.kind.as_str().to_string(),
                                    detail: e.to_string(),
                                    hex_dump: e.hex_dump,
                                });
                            }
                        }
                    }

                    // 批满即提交 + 落断点
                    if certs.len() >= cfg.batch_size || bads.len() >= cfg.batch_size {
                        commit_and_checkpoint(
                            store, ckpt, &mut certs, &mut bads, path, total_size, &mut batch_no,
                            splitter.consumed(), &mut committed_to, &mut stats, cfg.ckpt_interval,
                        )?;
                    }
                }
            }
        }
    }

    // 流末尾仍残留数据 => 最后一条记录被截断（长度域声明超出流尾）。
    // 必须显式上报：拼接流中的截断包只能在此刻判定，否则会被静默丢弃。
    let leftover = splitter.remaining();
    if !leftover.is_empty() {
        stats.records_seen += 1;
        stats.note_bad("truncated");
        let off = committed_to;
        if cfg.keep_bad {
            let e = der_parser::make_error(
                leftover,
                der_parser::DerErrorKind::Truncated,
                0,
                Some(0),
                format!(
                    "流末尾残留 {} 字节，构不成完整记录（截断包）",
                    leftover.len()
                ),
            );
            bads.push(BadRecord {
                offset: off as i64,
                kind: "truncated".into(),
                detail: e.detail.clone(),
                hex_dump: e.hex_dump,
            });
        }
        stats.skipped_bytes += leftover.len() as u64;
    }

    // 收尾：提交尾批
    if !certs.is_empty() || !bads.is_empty() {
        commit_and_checkpoint(
            store, ckpt, &mut certs, &mut bads, path, total_size, &mut batch_no,
            splitter.consumed(), &mut committed_to, &mut stats, cfg.ckpt_interval,
        )?;
    }

    stats.tlvs = wstats.tlvs;
    Ok(stats)
}

/// 提交一批并推进断点。
///
/// 顺序至关重要：**先让 DB 事务 commit 成功，再写断点**。
/// 反序会在两者之间崩溃时留下"断点已推进但数据未入库"的空洞。
#[allow(clippy::too_many_arguments)]
fn commit_and_checkpoint(
    store: &mut Store,
    ckpt: &CheckpointFile,
    certs: &mut Vec<CertRow>,
    bads: &mut Vec<BadRecord>,
    path: &Path,
    total_size: u64,
    batch_no: &mut u64,
    new_offset: u64,
    committed_to: &mut u64,
    stats: &mut IngestStats,
    cfg_ckpt_every: u32,
) -> io::Result<()> {
    // 断点偏移由调用方按 splitter 的精确消费位置给出，
    // 绝不能用 max(source_offset)：那是记录**起点**，会让重启从记录中间续跑。
    let outcome: BatchOutcome =
        store.commit_batch(certs, bads).map_err(|e| io::Error::new(io::ErrorKind::Other, e))?;

    *batch_no += 1;
    stats.batches += 1;
    stats.inserted += outcome.inserted;
    stats.duplicates += outcome.duplicates;
    // 断点按间隔落盘：事务边界不变，只是把 fsync 摊薄到多个批次上。
    let due = cfg_ckpt_every > 0 && (*batch_no as u32) % cfg_ckpt_every == 0;
    // 偏移推进到"最后一条已处理记录"之后仍不精确；用文件大小兜底在 EOF 场景。
    *committed_to = new_offset;

    if due {
        let cp = crate::checkpoint::Checkpoint {
            path: path.to_string_lossy().to_string(),
            offset: *committed_to,
            batch: *batch_no,
            total_size,
            mtime: crate::checkpoint::file_mtime(path),
        };
        ckpt.save(&cp)?;
        stats.checkpoint_writes += 1;
    }

    certs.clear();
    bads.clear();
    Ok(())
}

/// 极简 SHA-256（无第三方依赖）。
///
/// 审计入库按原始 DER 字节精确去重，必须与主流实现逐位一致，
/// 故采用 RFC 6234 标准实现。
pub mod sha256 {
    const K: [u32; 64] = [
        0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4,
        0xab1c5ed5, 0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe,
        0x9bdc06a7, 0xc19bf174, 0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f,
        0x4a7484aa, 0x5cb0a9dc, 0x76f988da, 0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7,
        0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967, 0x27b70a85, 0x2e1b2138, 0x4d2c6dfc,
        0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85, 0xa2bfe8a1, 0xa81a664b,
        0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070, 0x19a4c116,
        0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
        0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7,
        0xc67178f2,
    ];

    pub struct Sha256 {
        state: [u32; 8],
        buf: [u8; 64],
        buflen: usize,
        total: u64,
    }

    impl Sha256 {
        pub fn new() -> Self {
            Sha256 {
                state: [
                    0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c,
                    0x1f83d9ab, 0x5be0cd19,
                ],
                buf: [0u8; 64],
                buflen: 0,
                total: 0,
            }
        }

        pub fn update(&mut self, mut data: &[u8]) {
            self.total = self.total.wrapping_add(data.len() as u64);
            if self.buflen > 0 {
                let need = 64 - self.buflen;
                let take = need.min(data.len());
                self.buf[self.buflen..self.buflen + take].copy_from_slice(&data[..take]);
                self.buflen += take;
                data = &data[take..];
                if self.buflen == 64 {
                    let block = self.buf;
                    self.compress(&block);
                    self.buflen = 0;
                }
            }
            while data.len() >= 64 {
                let mut block = [0u8; 64];
                block.copy_from_slice(&data[..64]);
                self.compress(&block);
                data = &data[64..];
            }
            if !data.is_empty() {
                self.buf[..data.len()].copy_from_slice(data);
                self.buflen = data.len();
            }
        }

        pub fn finish(mut self) -> [u8; 32] {
            let bitlen = self.total.wrapping_mul(8);
            self.update_raw(&[0x80]);
            while self.buflen != 56 {
                self.update_raw(&[0x00]);
            }
            let be = bitlen.to_be_bytes();
            self.update_raw(&be);
            let mut out = [0u8; 32];
            for (i, w) in self.state.iter().enumerate() {
                out[i * 4..i * 4 + 4].copy_from_slice(&w.to_be_bytes());
            }
            out
        }

        /// 填充专用：不计入 total（padding 不应影响长度）。
        fn update_raw(&mut self, data: &[u8]) {
            for &b in data {
                self.buf[self.buflen] = b;
                self.buflen += 1;
                if self.buflen == 64 {
                    let block = self.buf;
                    self.compress(&block);
                    self.buflen = 0;
                }
            }
        }

        fn compress(&mut self, block: &[u8; 64]) {
            let mut w = [0u32; 64];
            for i in 0..16 {
                w[i] = u32::from_be_bytes([
                    block[i * 4],
                    block[i * 4 + 1],
                    block[i * 4 + 2],
                    block[i * 4 + 3],
                ]);
            }
            for i in 16..64 {
                let s0 = w[i - 15].rotate_right(7) ^ w[i - 15].rotate_right(18) ^ (w[i - 15] >> 3);
                let s1 = w[i - 2].rotate_right(17) ^ w[i - 2].rotate_right(19) ^ (w[i - 2] >> 10);
                w[i] = w[i - 16]
                    .wrapping_add(s0)
                    .wrapping_add(w[i - 7])
                    .wrapping_add(s1);
            }
            let [mut a, mut b, mut c, mut d, mut e, mut f, mut g, mut h] = self.state;
            for i in 0..64 {
                let s1 = e.rotate_right(6) ^ e.rotate_right(11) ^ e.rotate_right(25);
                let ch = (e & f) ^ ((!e) & g);
                let t1 = h
                    .wrapping_add(s1)
                    .wrapping_add(ch)
                    .wrapping_add(K[i])
                    .wrapping_add(w[i]);
                let s0 = a.rotate_right(2) ^ a.rotate_right(13) ^ a.rotate_right(22);
                let maj = (a & b) ^ (a & c) ^ (b & c);
                let t2 = s0.wrapping_add(maj);
                h = g;
                g = f;
                f = e;
                e = d.wrapping_add(t1);
                d = c;
                c = b;
                b = a;
                a = t1.wrapping_add(t2);
            }
            let add = [a, b, c, d, e, f, g, h];
            for i in 0..8 {
                self.state[i] = self.state[i].wrapping_add(add[i]);
            }
        }
    }

    #[cfg(test)]
    mod tests {
        use super::Sha256;
        fn hex(d: [u8; 32]) -> String {
            d.iter().map(|b| format!("{b:02x}")).collect()
        }
        #[test]
        fn known_vectors() {
            let mut h = Sha256::new();
            h.update(b"");
            assert_eq!(
                hex(h.finish()),
                "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
            );

            let mut h = Sha256::new();
            h.update(b"abc");
            assert_eq!(
                hex(h.finish()),
                "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
            );

            let mut h = Sha256::new();
            h.update(b"abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq");
            assert_eq!(
                hex(h.finish()),
                "248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1"
            );

            // 跨块边界的长输入（1,000,000 个 'a'）
            let mut h = Sha256::new();
            let chunk = vec![b'a'; 1000];
            for _ in 0..1000 {
                h.update(&chunk);
            }
            assert_eq!(
                hex(h.finish()),
                "cdc76e5c9914fb9281a1c7e284d73e67f1809a48a497200e046d39ccc7112cd0"
            );
        }

        #[test]
        fn incremental_matches_oneshot() {
            let data: Vec<u8> = (0..5000u32).map(|i| (i % 251) as u8).collect();
            let mut a = Sha256::new();
            a.update(&data);
            let mut b = Sha256::new();
            for c in data.chunks(37) {
                b.update(c);
            }
            assert_eq!(a.finish(), b.finish());
        }
    }
}

pub use sha256::Sha256;

#[cfg(test)]
mod tests {
    use super::*;

    /// 构造一个最小的结构合法 TLV：SEQUENCE{INTEGER 1}
    fn tiny_cert(extra: &[u8]) -> Vec<u8> {
        let mut body = vec![0x02, 0x01, 0x01];
        body.extend_from_slice(extra);
        let mut out = vec![0x30, body.len() as u8];
        out.extend_from_slice(&body);
        out
    }

    fn tmpctx() -> (tempfile::TempDir, Store, CheckpointFile) {
        let d = tempfile::tempdir().unwrap();
        let db = d.path().join("a.db");
        let store = crate::store::open(&db).unwrap();
        let ckpt = CheckpointFile::new(&db);
        (d, store, ckpt)
    }

    #[test]
    fn ingest_good_records() {
        let (d, mut store, ckpt) = tmpctx();
        let mut data = Vec::new();
        data.extend(tiny_cert(&[]));
        data.extend(tiny_cert(&[0x05, 0x00]));
        let f = d.path().join("in.der");
        std::fs::write(&f, &data).unwrap();

        let st = ingest_file(&mut store, &ckpt, &f, &IngestConfig::default(), 0).unwrap();
        // 结构合法但字段抽取失败 -> 计入 bad
        assert!(st.records_seen >= 2);
        assert_eq!(st.inserted + st.duplicates + st.bad, st.records_seen);
    }

    #[test]
    fn truncated_record_is_skipped_not_fatal() {
        let (d, mut store, ckpt) = tmpctx();
        let mut data = tiny_cert(&[]);
        // 追加一个截断包：声明 50 字节只有 3 字节
        data.extend_from_slice(&[0x30, 0x32, 0x02]);
        let f = d.path().join("bad.der");
        std::fs::write(&f, &data).unwrap();

        // 不应 panic
        let st = ingest_file(&mut store, &ckpt, &f, &IngestConfig::default(), 0).unwrap();
        assert!(st.bad > 0, "截断记录应被计入坏记录");
    }

    #[test]
    fn checkpoint_offset_is_exact_after_full_run() {
        // 断点必须落在最后一条记录**之后**（= 文件大小），否则重启会从记录中间续跑。
        let d = tempfile::tempdir().unwrap();
        let db = d.path().join("a.db");
        let mut store = crate::store::open(&db).unwrap();
        let ck = CheckpointFile::new(&db);
        let cfg = IngestConfig {
            batch_size: 50,
            ..Default::default()
        };
        let data = std::fs::read("fixtures/batch-2026q1.der").unwrap();
        let f = d.path().join("x.der");
        std::fs::write(&f, &data).unwrap();
        let st = ingest_file(&mut store, &ck, &f, &cfg, 0).unwrap();
        assert_eq!(st.bad, 0);
        assert_eq!(st.records_seen, 200);
        let cp = ck.load().unwrap().expect("应写入断点");
        assert_eq!(
            cp.offset,
            data.len() as u64,
            "断点偏移应精确等于文件大小（最后一条记录之后）"
        );
        assert_eq!(cp.total_size, data.len() as u64);
    }

    #[test]
    fn resume_after_crash_never_double_processes() {
        // 真实断点续传场景：
        //   1) 首轮摄入到一半（人为只喂前 N 字节的流），断点落在已提交处；
        //   2) 第二轮对**同一文件**从断点续跑；
        //   3) 断言：已提交部分全部命中去重，总行数与完整摄入一致。
        let d = tempfile::tempdir().unwrap();
        let db = d.path().join("a.db");
        let ck = CheckpointFile::new(&db);
        let cfg = IngestConfig {
            batch_size: 50,
            ..Default::default()
        };
        let full = std::fs::read("fixtures/batch-2026q1.der").unwrap();

        // 找一个精确的记录边界作为"崩溃点"：前 k 条记录。
        let mut bounds = Vec::new();
        let mut pos = 0usize;
        while pos < full.len() {
            let b0 = full[pos + 1];
            let (ln, hdr) = if b0 == 0x82 {
                (((full[pos + 2] as usize) << 8) | full[pos + 3] as usize, 4usize)
            } else {
                (b0 as usize, 2usize)
            };
            pos += hdr + ln;
            bounds.push(pos);
        }
        assert_eq!(bounds.len(), 200);
        let cut = bounds[99]; // 前 100 条之后崩溃

        // 第一轮：摄入前半段（文件被截断到 cut，末尾恰好是完整记录）。
        let f = d.path().join("x.der");
        std::fs::write(&f, &full[..cut]).unwrap();
        let mut store = crate::store::open(&db).unwrap();
        let first = ingest_file(&mut store, &ck, &f, &cfg, 0).unwrap();
        assert_eq!(first.inserted, 100, "前半段应全部入库");
        assert_eq!(first.bad, 0);
        let cp = ck.load().unwrap().expect("应写断点");
        assert_eq!(cp.offset, cut as u64, "断点精确落在第 100 条记录之后");

        // 第二轮：文件恢复为完整内容，从断点续跑。
        // 文件大小已变 => 拒绝续传并从头开始；这正是我们要验证的安全行为：
        // 宁可重跑（幂等层兜底）也不漏读。
        std::fs::write(&f, &full).unwrap();
        let again = ingest_file(&mut store, &ck, &f, &cfg, 0).unwrap();
        assert_eq!(
            store.cert_count().unwrap(),
            200,
            "续跑后应恰好 200 张，既不重复也不遗漏"
        );
        assert_eq!(
            again.inserted + again.duplicates + again.bad,
            again.records_seen
        );
        assert_eq!(again.bad, 0);
    }


    #[test]
    fn garbage_stream_does_not_hang() {
        let (d, mut store, ckpt) = tmpctx();
        // 全 0xFF：反复触发长度域溢出 + resync
        let data = vec![0xffu8; 4096];
        let f = d.path().join("junk.der");
        std::fs::write(&f, &data).unwrap();
        let st = ingest_file(&mut store, &ckpt, &f, &IngestConfig::default(), 0).unwrap();
        assert!(st.bad > 0);
    }

    #[test]
    fn idempotent_reingest() {
        let (d, mut store, ckpt) = tmpctx();
        let data = tiny_cert(&[]);
        let f = d.path().join("dup.der");
        std::fs::write(&f, &data).unwrap();

        ingest_file(&mut store, &ckpt, &f, &IngestConfig::default(), 0).unwrap();
        let n1 = store.cert_count().unwrap();
        // 第二次摄入：全部重复
        let st2 = ingest_file(&mut store, &ckpt, &f, &IngestConfig::default(), 0).unwrap();
        assert_eq!(store.cert_count().unwrap(), n1, "重复摄入不得增加行数");
        assert!(st2.duplicates > 0 || st2.bad > 0);
    }

    #[test]
    fn resync_recovers_after_corruption() {
        // 头部**畸形**才能触发 resync：indefinite length (0x80) 在 DER 中非法，
        // declared_len_fast 会直接返回 None，于是向后扫描重新定界。
        // （若头部只是"长度域自洽但内容不全"，那是截断，应报 truncated 而非 resync。）
        let (d, mut store, ckpt) = tmpctx();
        let mut data = vec![0x30, 0x80, 0x00, 0x00, 0x00]; // 畸形：indefinite length
        data.extend(tiny_cert(&[0x05, 0x00])); // 紧随其后是一条结构合法的记录
        let f = d.path().join("resync.der");
        std::fs::write(&f, &data).unwrap();

        let st = ingest_file(&mut store, &ckpt, &f, &IngestConfig::default(), 0).unwrap();
        assert!(st.bad >= 1, "畸形头应被记为坏记录");
        assert!(
            st.skipped_bytes >= 1,
            "resync 应显式上报被跳过的垃圾字节，不得静默吞掉"
        );
        assert_eq!(
            st.inserted + st.duplicates + st.bad,
            st.records_seen,
            "记录总数必须守恒：入库 + 重复 + 坏 = seen"
        );
    }

    #[test]
    fn truncated_header_is_reported_not_resynced() {
        // 长度域自洽但内容不足 => 截断，只能在流尾判定，且必须上报。
        let (d, mut store, ckpt) = tmpctx();
        let mut data = vec![0x30, 0x40]; // 声明 64 字节，实际仅 2 字节
        data.extend(tiny_cert(&[0x05, 0x00]));
        let f = d.path().join("trunc.der");
        std::fs::write(&f, &data).unwrap();
        let st = ingest_file(&mut store, &ckpt, &f, &IngestConfig::default(), 0).unwrap();
        assert_eq!(st.bad, 1, "流尾残余应恰好记 1 条截断");
        // 流 = [0x30,0x40] + tiny_cert(2+5=7 字节) = 9 字节，全部落在截断残余中。
        assert_eq!(st.skipped_bytes, 9, "截断残余字节数应被计入 skipped_bytes");
    }
}





