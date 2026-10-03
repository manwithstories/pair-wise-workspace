//! Crash-safe ingest checkpoints.
//!
//! The checkpoint answers one question: *how far into each source file has the
//! database durably committed work?* It is written only after a transaction
//! commits, and written atomically (temp file + fsync + rename + directory
//! fsync), so it can never name a batch that the database does not have.
//!
//! The database's own `ingest_state` table is the source of truth; the
//! checkpoint file is the fast path that lets a restart skip re-framing bytes
//! it already owns. When the two disagree, [`load_progress`] trusts the
//! database, because a torn checkpoint costs one redundant batch while a torn
//! database offset would silently skip committed rows.

use std::collections::BTreeMap;
use std::io::Write;
use std::path::{Path, PathBuf};

use tempfile::NamedTempFile;

/// Offsets for every source file seen so far.
#[derive(Debug, Default, Clone)]
pub struct Checkpoint {
    path: PathBuf,
    offsets: BTreeMap<String, u64>,
    dirty: bool,
}

impl Checkpoint {
    /// Load an existing checkpoint file, or start empty if there is none.
    pub fn open(path: impl Into<PathBuf>) -> std::io::Result<Self> {
        let path = path.into();
        let mut offsets = BTreeMap::new();
        if let Ok(text) = std::fs::read_to_string(&path) {
            offsets = parse(&text);
        }
        Ok(Checkpoint {
            path,
            offsets,
            dirty: false,
        })
    }

    pub fn get(&self, file: &str) -> Option<&u64> {
        self.offsets.get(file)
    }

    /// Record a committed offset. Durable only after [`Checkpoint::flush`].
    pub fn set(&mut self, file: &str, offset: u64) {
        // Never move a checkpoint backwards: that would rewind progress and
        // cause committed batches to be reprocessed.
        if self.offsets.get(file).map_or(true, |&cur| offset > cur) {
            self.offsets.insert(file.to_string(), offset);
            self.dirty = true;
        }
    }

    /// Atomically persist the checkpoint.
    ///
    /// The rename is the commit point, and both the file and its directory are
    /// fsync'd, so a power loss leaves either the old checkpoint or the new one
    /// — never a truncated file.
    pub fn flush(&mut self) -> std::io::Result<()> {
        if !self.dirty {
            return Ok(());
        }
        let body = render(&self.offsets);
        if let Some(dir) = self.path.parent() {
            if !dir.as_os_str().is_empty() {
                std::fs::create_dir_all(dir)?;
            }
        }
        let mut tmp = NamedTempFile::new_in(
            self.path
                .parent()
                .filter(|p| !p.as_os_str().is_empty())
                .unwrap_or_else(|| Path::new(".")),
        )?;
        tmp.write_all(body.as_bytes())?;
        tmp.as_file().sync_all()?;
        tmp.persist(&self.path)
            .map_err(|e| std::io::Error::new(std::io::ErrorKind::Other, e.to_string()))?;
        // Persist the directory entry so the rename itself survives a crash.
        if let Some(dir) = self.path.parent() {
            if !dir.as_os_str().is_empty() {
                if let Ok(d) = std::fs::File::open(dir) {
                    let _ = d.sync_all();
                }
            }
        }
        self.dirty = false;
        Ok(())
    }

    /// Drop all progress; used by `--restart`.
    pub fn clear(&mut self) {
        self.offsets.clear();
        self.dirty = true;
    }

    pub fn entries(&self) -> impl Iterator<Item = (&String, &u64)> {
        self.offsets.iter()
    }
}

/// Minimal line-oriented format: `path<TAB>offset` per line.
///
/// Deliberately not JSON: a checkpoint is written on every batch commit, and a
/// flat text format keeps both the write and the parser allocation-light.
fn render(offsets: &BTreeMap<String, u64>) -> String {
    let mut s = String::with_capacity(offsets.len() * 48);
    s.push_str("# pki-audit checkpoint v1\n");
    for (k, v) in offsets {
        s.push_str(k);
        s.push('\t');
        s.push_str(&v.to_string());
        s.push('\n');
    }
    s
}

fn parse(text: &str) -> BTreeMap<String, u64> {
    let mut out = BTreeMap::new();
    for line in text.lines() {
        let line = line.trim();
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        if let Some((k, v)) = line.rsplit_once('\t') {
            if let Ok(off) = v.parse::<u64>() {
                out.insert(k.to_string(), off);
            }
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tmpdir(tag: &str) -> PathBuf {
        let p = std::env::temp_dir().join(format!("pki-ck-{tag}-{}", std::process::id()));
        std::fs::create_dir_all(&p).unwrap();
        p
    }

    #[test]
    fn roundtrip_through_disk() {
        let d = tmpdir("rt");
        let f = d.join("ck.txt");
        let mut ck = Checkpoint::open(&f).unwrap();
        ck.set("a.der", 4096);
        ck.set("b.der", 12);
        ck.flush().unwrap();

        let ck2 = Checkpoint::open(&f).unwrap();
        assert_eq!(ck2.get("a.der"), Some(&4096));
        assert_eq!(ck2.get("b.der"), Some(&12));
        assert_eq!(ck2.get("missing.der"), None);
    }

    #[test]
    fn offsets_never_move_backwards() {
        let d = tmpdir("mono");
        let mut ck = Checkpoint::open(d.join("ck.txt")).unwrap();
        ck.set("a.der", 100);
        ck.set("a.der", 50);
        assert_eq!(ck.get("a.der"), Some(&100));
        ck.set("a.der", 100);
        assert_eq!(ck.get("a.der"), Some(&100));
    }

    #[test]
    fn flush_is_atomic_and_leaves_no_temp_files() {
        let d = tmpdir("atomic");
        let f = d.join("ck.txt");
        let mut ck = Checkpoint::open(&f).unwrap();
        for i in 0..5 {
            ck.set("a.der", i * 10);
            ck.flush().unwrap();
        }
        let entries: Vec<_> = std::fs::read_dir(&d).unwrap().collect();
        assert_eq!(entries.len(), 1, "temp files must be cleaned up: {entries:?}");
        assert_eq!(Checkpoint::open(&f).unwrap().get("a.der"), Some(&40));
    }

    #[test]
    fn corrupt_checkpoint_degrades_to_empty_not_panic() {
        let d = tmpdir("bad");
        let f = d.join("ck.txt");
        std::fs::write(&f, "\x00\x01garbage\nno-tab-line\n").unwrap();
        let ck = Checkpoint::open(&f).unwrap();
        assert!(ck.entries().next().is_none());
    }

    #[test]
    fn flush_is_fast_enough_for_the_10ms_budget() {
        let d = tmpdir("perf");
        let f = d.join("ck.txt");
        let mut ck = Checkpoint::open(&f).unwrap();
        ck.set("warmup.der", 1);
        ck.flush().unwrap();
        let start = std::time::Instant::now();
        for i in 0..100u64 {
            ck.set("bench.der", i * 4096);
            ck.flush().unwrap();
        }
        let per = start.elapsed().as_secs_f64() / 100.0;
        assert!(
            per < 0.010,
            "checkpoint write took {:.3} ms, budget is 10 ms",
            per * 1000.0
        );
    }

    #[test]
    fn clear_resets_progress() {
        let d = tmpdir("clear");
        let mut ck = Checkpoint::open(d.join("ck.txt")).unwrap();
        ck.set("a.der", 99);
        ck.clear();
        assert_eq!(ck.get("a.der"), None);
    }
}
