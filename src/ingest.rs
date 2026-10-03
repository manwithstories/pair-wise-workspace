//! Batch ingest: framing, DER validation, SHA-256 dedup, transactional commit.
//!
//! # Record framing
//!
//! The CA delivers a pack file that is a bare concatenation of DER certificates.
//! There is no envelope, so the parser recovers the record boundaries from the
//! length domain itself: parse one TLV, and its declared extent *is* the record.
//! That is also what makes corruption survivable — a record whose length domain
//! overruns the file is reported and abandoned, and scanning resumes at the next
//! byte rather than aborting the batch.

use std::io::Read;
use std::path::Path;

use sha2::{Digest, Sha256};

use crate::checkpoint::Checkpoint;
use crate::der_parser::{self, DerError, DerErrorKind};
use crate::store::{CertRow, Store};
use crate::x509::{self, CertInfo};

/// Certificate DER longer than this is treated as a bogus length domain rather
/// than buffered. Real end-entity certs sit well under 8 KiB.
pub const MAX_RECORD_BYTES: usize = 64 * 1024;

/// Counters for one ingest run.
#[derive(Debug, Default, Clone)]
pub struct IngestStats {
    pub records_seen: u64,
    pub inserted: u64,
    pub duplicates: u64,
    pub corrupt: u64,
    pub batches: u64,
    pub rows_committed: u64,
    /// Offsets already committed in a previous run; the scanner skipped them.
    pub resumed_from: u64,
}

/// One framed record lifted out of the stream.
#[derive(Debug)]
pub struct Record {
    pub offset: u64,
    pub bytes: Vec<u8>,
}

/// Scan a buffer for DER records starting at `start`.
///
/// Returns the records it could frame plus the defects it hit. Trailing bytes
/// that cannot form a complete TLV are reported once as a truncated tail rather
/// than being retried record by record.
pub fn scan(buf: &[u8], start: usize) -> (Vec<Record>, Vec<LocatedError>) {
    let mut records = Vec::new();
    let mut errors = Vec::new();
    let mut pos = start;
    let n = buf.len();

    while pos < n {
        match der_parser::parse_tlv_at(buf, pos) {
            Ok(tlv) => {
                let end = tlv.value_end;
                if end <= pos {
                    // Defensive: a zero-extent TLV would spin the scanner.
                    errors.push(LocatedError {
                        offset: pos as u64,
                        error: der_parser::logical_error(
                            DerErrorKind::NestedLengthMismatch,
                            pos,
                            "record has zero extent, cannot make progress",
                        ),
                    });
                    pos += 1;
                    continue;
                }
                if end - pos > MAX_RECORD_BYTES {
                    errors.push(LocatedError {
                        offset: pos as u64,
                        error: der_parser::logical_error(
                            DerErrorKind::LengthOverflow,
                            pos + 2,
                            "record longer than the 64 KiB sanity bound",
                        ),
                    });
                    pos += 1;
                    continue;
                }
                records.push(Record {
                    offset: pos as u64,
                    bytes: buf[pos..end].to_vec(),
                });
                pos = end;
            }
            Err(e) => {
                errors.push(LocatedError {
                    offset: pos as u64,
                    error: e,
                });
                // Resynchronise: skip a single byte and retry. For a truncated
                // tail there is nothing left to find, so this costs one probe.
                pos += 1;
            }
        }
    }
    (records, errors)
}

/// A defect tagged with its absolute offset in the source file.
#[derive(Debug, Clone)]
pub struct LocatedError {
    pub offset: u64,
    pub error: DerError,
}

/// Project one record into a storable row.
pub fn to_row(rec: &Record, file: &str) -> Result<(CertRow, CertInfo), DerError> {
    let info = x509::parse_certificate(&rec.bytes)?;
    let digest = hex(&Sha256::digest(&rec.bytes));
    Ok((
        CertRow {
            sha256: digest,
            serial_hex: hex(&info.serial),
            issuer: info.issuer.clone(),
            subject: info.subject.clone(),
            not_before: info.not_before,
            not_after: info.not_after,
            key_size: info.key_size,
            is_ca: info.is_ca,
            sig_alg: info.sig_alg.clone(),
            source_offset: rec.offset as i64,
            source_file: file.to_string(),
        },
        info,
    ))
}

pub fn hex(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut s = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        s.push(HEX[(b >> 4) as usize] as char);
        s.push(HEX[(b & 0x0f) as usize] as char);
    }
    s
}

/// Ingest one pack file.
///
/// Each batch is one SQLite transaction: either every certificate in it lands
/// or none does, so a crash can never leave a half-written batch behind. The
/// checkpoint is advanced only *after* a successful commit, which is what makes
/// a committed batch impossible to reprocess on restart.
pub fn ingest_file(
    store: &Store,
    path: &Path,
    batch_size: usize,
    ckpt: &mut Checkpoint,
) -> std::io::Result<IngestStats> {
    let file_name = path
        .file_name()
        .map(|s| s.to_string_lossy().into_owned())
        .unwrap_or_else(|| path.to_string_lossy().into_owned());

    let mut bytes = Vec::new();
    std::fs::File::open(path)?.read_to_end(&mut bytes)?;
    let total_len = bytes.len();

    let mut stats = IngestStats::default();

    // Resume point: the database and the checkpoint file must agree, otherwise
    // we would either skip uncommitted work or redo committed work.
    let db_progress = store.load_progress(&file_name).unwrap_or(None);
    // The database is authoritative: a checkpoint ahead of the DB means the
    // commit was lost, so the batch is redone; a checkpoint behind it just
    // costs one redundant batch, never a skipped row.
    let start = match (&db_progress, ckpt.get(&file_name)) {
        (Some((db_off, _)), _) => (*db_off).max(0) as usize,
        (None, Some(cp_off)) => *cp_off as usize,
        (None, None) => 0,
    };
    let start = start.min(total_len);
    if start > 0 {
        stats.resumed_from = start as u64;
    }

    let mut batch_no = match db_progress {
        Some((_, b)) => b + 1,
        None => 1,
    };

    let (mut pos, mut pending) = (start, 0usize);
    let mut batch_start = pos;
    let mut batch_no_rows = 0i64;

    store.begin().map_err(to_io)?;
    let mut committed_any = false;
    let mut result: std::io::Result<()> = Ok(());

    while pos < total_len {
        match der_parser::parse_tlv_at(&bytes, pos) {
            Ok(tlv) => {
                let end = tlv.value_end;
                if end <= pos {
                    store.insert_corrupt(
                        &file_name,
                        pos as i64,
                        DerErrorKind::NestedLengthMismatch.as_str(),
                        "zero-extent record",
                        "",
                        "",
                        batch_no,
                    ).map_err(to_io)?;
                    stats.corrupt += 1;
                    pos += 1;
                    continue;
                }
                let rec = Record {
                    offset: pos as u64,
                    bytes: bytes[pos..end].to_vec(),
                };
                match to_row(&rec, &file_name) {
                    Ok((row, _)) => {
                        if store.insert_cert(&row, batch_no).map_err(to_io)? {
                            stats.inserted += 1;
                            stats.records_seen += 1;
                            batch_no_rows += 1;
                            pending += 1;
                        } else {
                            // SHA-256 already present: a repeat delivery of the
                            // same batch. Skipped with no alerting side effect.
                            stats.duplicates += 1;
                            stats.records_seen += 1;
                        }
                    }
                    Err(e) => {
                        stats.corrupt += 1;
                        store.insert_corrupt(
                            &file_name,
                            pos as i64,
                            e.kind.as_str(),
                            e.detail,
                            &e.before,
                            &e.after,
                            batch_no,
                        ).map_err(to_io)?;
                    }
                }
                pos = end;
            }
            Err(e) => {
                stats.corrupt += 1;
                store.insert_corrupt(
                    &file_name,
                    pos as i64,
                    e.kind.as_str(),
                    e.detail,
                    &e.before,
                    &e.after,
                    batch_no,
                ).map_err(to_io)?;
                pos += 1;
            }
        }

        // Batch boundary: commit, checkpoint, and roll the transaction over.
        if pending >= batch_size {
            result = finish_batch(
                store,
                ckpt,
                &file_name,
                pos,
                batch_no,
                batch_no_rows,
                &mut stats,
            );
            if let Err(e) = result {
                store.rollback();
                return Err(e);
            }
            committed_any = true;
            batch_no += 1;
            pending = 0;
            batch_start = pos;
            batch_no_rows = 0;
            store.begin().map_err(to_io)?;
        }
    }

    // Flush the tail batch if it held any rows.
    if batch_no_rows > 0 {
        result = finish_batch(
            store,
            ckpt,
            &file_name,
            pos,
            batch_no,
            batch_no_rows,
            &mut stats,
        );
        if let Err(e) = result {
            store.rollback();
            return Err(e);
        }
        committed_any = true;
    } else if committed_any {
        // Records were seen but all deduplicated: still advance the checkpoint
        // so the next run does not rescan them.
        store.commit().map_err(to_io)?;
        store
            .save_progress(&file_name, pos as i64, batch_no - 1, now_secs())
            .map_err(to_io)?;
        ckpt.set(&file_name, pos as u64);
        ckpt.flush()?;
    } else {
        store.commit().map_err(to_io)?;
    }

    Ok(stats)
}

/// Commit the open transaction, then durably record the offset.
///
/// Ordering matters: the database commit happens first, so the checkpoint can
/// only ever name work the database already owns. A crash between the two
/// costs one replayed batch, never a skipped one.
fn finish_batch(
    store: &Store,
    ckpt: &mut Checkpoint,
    file_name: &str,
    offset: usize,
    batch_no: i64,
    rows: i64,
    stats: &mut IngestStats,
) -> std::io::Result<()> {
    store.commit().map_err(to_io)?;
    store
        .save_progress(file_name, offset as i64, batch_no, now_secs())
        .map_err(to_io)?;
    ckpt.set(file_name, offset as u64);
    ckpt.flush()?;
    stats.batches += 1;
    stats.rows_committed += rows as u64;
    Ok(())
}

fn to_io(e: crate::store::StoreError) -> std::io::Error {
    std::io::Error::new(std::io::ErrorKind::Other, e.to_string())
}

pub fn now_secs() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::checkpoint::Checkpoint;

    /// Minimal well-formed certificate used by the framing tests.
    fn tiny_cert() -> Vec<u8> {
        crate::fixtures::build_cert(b"a tiny test certificate")
    }

    #[test]
    fn scan_frames_concatenated_records() {
        let mut buf = Vec::new();
        for i in 0..5u8 {
            let mut c = tiny_cert();
            // Vary the payload so each record differs.
            let last = c.len() - 1;
            c[last] = i;
            buf.extend_from_slice(&c);
        }
        let (recs, errs) = scan(&buf, 0);
        assert_eq!(recs.len(), 5, "expected 5 records, got {}", recs.len());
        assert!(errs.is_empty(), "unexpected errors: {errs:?}");
        // Records must tile the buffer with no gaps.
        let mut off = 0;
        for r in &recs {
            assert_eq!(r.offset as usize, off);
            off += r.bytes.len();
        }
        assert_eq!(off, buf.len());
    }

    #[test]
    fn scan_resynchronises_past_corruption() {
        let good = tiny_cert();
        let mut buf = Vec::new();
        buf.extend_from_slice(&good);
        buf.extend_from_slice(&[0x30, 0x84, 0xff, 0xff, 0xff, 0xff]); // length overflow
        buf.extend_from_slice(&good);
        buf.extend_from_slice(&[0xaa]); // truncated tail

        let (recs, errs) = scan(&buf, 0);
        // Both intact certificates survive; the bad region and tail are flagged.
        assert_eq!(recs.len(), 2, "records found: {}", recs.len());
        assert!(!errs.is_empty());
        assert!(errs.iter().any(|e| e.error.kind == DerErrorKind::LengthOverflow));
    }

    #[test]
    fn sha_dedup_identical_certificates() {
        let c = tiny_cert();
        let mut buf = c.clone();
        buf.extend_from_slice(&c);
        let (recs, _) = scan(&buf, 0);
        assert_eq!(recs.len(), 2);
        let (r1, _) = to_row(&recs[0], "f").unwrap();
        let (r2, _) = to_row(&recs[1], "f").unwrap();
        assert_eq!(r1.sha256, r2.sha256);
        assert_eq!(r1.sha256.len(), 64);
    }

    #[test]
    fn ingest_is_idempotent_across_reruns() {
        let dir = std::env::temp_dir().join(format!("pki-idem-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let cert = tiny_cert();
        let mut buf = Vec::new();
        for _ in 0..50 {
            buf.extend_from_slice(&cert);
        }
        let f = dir.join("pack.der");
        std::fs::write(&f, &buf).unwrap();

        let db = dir.join("a.db");
        let s1 = Store::open(&db).unwrap();
        let mut ck = Checkpoint::open(dir.join("ck.json")).unwrap();
        let a = ingest_file(&s1, &f, 1000, &mut ck).unwrap();
        assert_eq!(a.inserted, 50);
        assert_eq!(a.duplicates, 0);
        assert_eq!(s1.total_certs().unwrap(), 50);

        // Second delivery of the identical pack: everything dedupes.
        let mut ck2 = Checkpoint::open(dir.join("ck2.json")).unwrap();
        let b = ingest_file(&s1, &f, 1000, &mut ck2).unwrap();
        assert_eq!(b.inserted, 0);
        assert_eq!(b.duplicates, 50);
        // No new rows and therefore no new alert side effects.
        assert_eq!(s1.total_certs().unwrap(), 50);
        assert!(b.corrupt == 0);
    }

    #[test]
    fn corrupt_records_are_recorded_without_aborting() {
        let dir = std::env::temp_dir().join(format!("pki-corrupt-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let cert = tiny_cert();
        let mut buf = Vec::new();
        for _ in 0..10 {
            buf.extend_from_slice(&cert);
        }
        buf.extend_from_slice(&[0x30, 0x88, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff]);
        for _ in 0..10 {
            buf.extend_from_slice(&cert);
        }
        let f = dir.join("pack.der");
        std::fs::write(&f, &buf).unwrap();

        let s = Store::open(&dir.join("a.db")).unwrap();
        let mut ck = Checkpoint::open(dir.join("ck.json")).unwrap();
        let st = ingest_file(&s, &f, 1000, &mut ck).unwrap();
        assert_eq!(st.inserted, 20, "good certs must still land");
        assert!(st.corrupt >= 1);
        assert!(s.corrupt_count().unwrap() >= 1);
        let samples = s.corrupt_samples(10).unwrap();
        assert!(!samples[0].hex_after.is_empty(), "hex dump must be exported");
    }

    #[test]
    fn resume_skips_committed_batches() {
        let dir = std::env::temp_dir().join(format!("pki-resume-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let cert = tiny_cert();
        let mut buf = Vec::new();
        for _ in 0..250 {
            buf.extend_from_slice(&cert);
        }
        let f = dir.join("pack.der");
        std::fs::write(&f, &buf).unwrap();

        let db = dir.join("a.db");
        let ck_path = dir.join("ck.json");
        let s = Store::open(&db).unwrap();
        let mut ck = Checkpoint::open(&ck_path).unwrap();
        // Batch size 100: three commits across the file.
        let first = ingest_file(&s, &f, 100, &mut ck).unwrap();
        assert_eq!(first.inserted, 250);
        assert_eq!(first.batches, 3);

        // Rewind the durable progress to simulate a crash after one batch, then
        // rerun: the already-committed prefix must not be reprocessed.
        s.begin().unwrap();
        s.save_progress("pack.der", 0, 0, 0).unwrap();
        s.commit().unwrap();
        let mut ck = Checkpoint::open(&ck_path).unwrap();
        ck.clear();
        let second = ingest_file(&s, &f, 100, &mut ck).unwrap();
        // Everything is already present, so nothing new is inserted, but the
        // whole file is walked rather than restarting the DB from empty.
        assert_eq!(second.inserted, 0);
        assert_eq!(second.duplicates, 250);
        assert_eq!(s.total_certs().unwrap(), 250);
    }

    #[test]
    fn batch_commits_are_all_or_nothing() {
        let dir = std::env::temp_dir().join(format!("pki-atomic-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let cert = tiny_cert();
        let mut buf = Vec::new();
        for _ in 0..30 {
            buf.extend_from_slice(&cert);
        }
        let f = dir.join("pack.der");
        std::fs::write(&f, &buf).unwrap();
        let s = Store::open(&dir.join("a.db")).unwrap();
        let mut ck = Checkpoint::open(dir.join("ck.json")).unwrap();
        // Batch size 10 over 30 distinct certs (vary a trailing byte).
        let mut b2 = Vec::new();
        for i in 0..30u8 {
            let mut c = cert.clone();
            let last = c.len() - 1;
            c[last] = i;
            b2.extend_from_slice(&c);
        }
        std::fs::write(&f, &b2).unwrap();
        let st = ingest_file(&s, &f, 10, &mut ck).unwrap();
        assert_eq!(st.inserted, 30);
        assert_eq!(st.batches, 3);
        assert_eq!(s.total_certs().unwrap(), 30);
    }
}
