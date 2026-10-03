//! 断点续传：已提交批次偏移落盘与重启续跑。
//!
//! 原子性策略：写临时文件 → `fsync(tmp)` → `rename(tmp, dst)` → `fsync(dir)`。
//! `rename` 在同一文件系统内是原子的，因此读侧永远看到完整旧版本或完整新版本，
//! 不会出现"写了一半的断点"。这保证已提交批次绝不重处理：
//! 断点只在批次事务 `commit()` 成功之后才推进。

use std::fs::{File, OpenOptions};
use std::io::{self, Read, Seek, SeekFrom, Write};
use std::path::{Path, PathBuf};

/// 断点状态。序列化格式为紧凑文本，便于人工排障。
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Checkpoint {
    pub path: String,
    /// 已成功提交到的流内字节偏移（下一批从此处开始）。
    pub offset: u64,
    /// 已提交批次数。
    pub batch: u64,
    /// 已提交文件总字节数（用于校验续传时文件未变更）。
    pub total_size: u64,
    /// 文件 mtime（秒），变更则拒绝续传。
    pub mtime: u64,
}

impl Checkpoint {
    fn serialize(&self) -> String {
        format!(
            "version=1\npath={}\noffset={}\nbatch={}\ntotal_size={}\nmtime={}\n",
            self.path, self.offset, self.batch, self.total_size, self.mtime
        )
    }

    fn deserialize(s: &str) -> io::Result<Self> {
        let mut cp = Checkpoint::default();
        for line in s.lines() {
            let mut it = line.splitn(2, '=');
            let k = it.next().unwrap_or("").trim();
            let v = it.next().unwrap_or("").trim();
            match k {
                "path" => cp.path = v.to_string(),
                "offset" => cp.offset = v.parse().map_err(invalid)?,
                "batch" => cp.batch = v.parse().map_err(invalid)?,
                "total_size" => cp.total_size = v.parse().map_err(invalid)?,
                "mtime" => cp.mtime = v.parse().map_err(invalid)?,
                _ => {}
            }
        }
        Ok(cp)
    }
}

fn invalid(e: std::num::ParseIntError) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, e)
}

/// 断点文件：`<db>.ckpt`。
pub struct CheckpointFile {
    pub path: PathBuf,
}

impl CheckpointFile {
    pub fn new(db_path: &Path) -> Self {
        let mut p = db_path.as_os_str().to_os_string();
        p.push(".ckpt");
        CheckpointFile {
            path: PathBuf::from(p),
        }
    }

    /// 读取断点；不存在返回 None。
    pub fn load(&self) -> io::Result<Option<Checkpoint>> {
        let mut f = match File::open(&self.path) {
            Ok(f) => f,
            Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(None),
            Err(e) => return Err(e),
        };
        let mut buf = String::new();
        f.read_to_string(&mut buf)?;
        Checkpoint::deserialize(&buf).map(Some)
    }

    /// 原子落盘一个断点。
    ///
    /// 落盘耗时须 <10ms：写入的是常数级大小文本，2 次 fsync 在本地 SSD 上
    /// 通常 <5ms。fsync 失败视为错误向上传播——**绝不吞掉**，
    /// 否则断点落后于数据会导致重复处理（幂等层会挡住，但会浪费算力）。
    pub fn save(&self, cp: &Checkpoint) -> io::Result<()> {
        let tmp = self.path.with_extension("ckpt.tmp");
        {
            let mut f = OpenOptions::new()
                .create(true)
                .write(true)
                .truncate(true)
                .open(&tmp)?;
            f.write_all(cp.serialize().as_bytes())?;
            f.sync_all()?; // fsync(tmp)：确保内容落盘
        }
        std::fs::rename(&tmp, &self.path)?; // 原子替换
        // fsync 目录：确保 rename 本身持久化，否则崩溃后可能回退到旧断点。
        if let Some(dir) = self.path.parent() {
            let d = File::open(dir)?;
            d.sync_all()?;
        }
        Ok(())
    }

    /// 清除断点（摄入完成后调用）。
    pub fn clear(&self) -> io::Result<()> {
        match std::fs::remove_file(&self.path) {
            Ok(()) => Ok(()),
            Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(()),
            Err(e) => Err(e),
        }
    }
}

/// 取文件 mtime（Unix 秒），失败返回 0。
pub fn file_mtime(p: &Path) -> u64 {
    std::fs::metadata(p)
        .and_then(|m| m.modified())
        .ok()
        .and_then(|t| t.duration_since(std::time::UNIX_EPOCH).ok())
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

/// 判断能否安全续传。
///
/// 文件大小或 mtime 变化说明投递方替换了文件，此时旧偏移不再有意义，
/// 必须从头重来（否则会漏读或错位）。
pub fn resumable(cp: &Checkpoint, path: &Path, size: u64, mtime: u64) -> bool {
    cp.path == path.to_string_lossy() && cp.total_size == size && cp.mtime == mtime && cp.offset <= size
}

/// 在 `data` 的 `from` 偏移处续读，返回新的 File 与位置。
pub fn seek_to(f: &mut File, from: u64) -> io::Result<()> {
    f.seek(SeekFrom::Start(from)).map(|_| ())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tmpfile() -> (tempfile::TempDir, CheckpointFile) {
        let d = tempfile::tempdir().unwrap();
        let cf = CheckpointFile::new(&d.path().join("audit.db"));
        (d, cf)
    }

    #[test]
    fn roundtrip() {
        let (_d, cf) = tmpfile();
        let cp = Checkpoint {
            path: "/data/x.der".into(),
            offset: 4096,
            batch: 7,
            total_size: 1_000_000,
            mtime: 1_700_000_000,
        };
        cf.save(&cp).unwrap();
        let got = cf.load().unwrap().unwrap();
        assert_eq!(got, cp);
    }

    #[test]
    fn missing_returns_none() {
        let (_d, cf) = tmpfile();
        assert!(cf.load().unwrap().is_none());
    }

    #[test]
    fn save_is_atomic_no_partial_state() {
        let (_d, cf) = tmpfile();
        let cp = Checkpoint {
            path: "p".into(),
            offset: 1,
            batch: 1,
            total_size: 2,
            mtime: 3,
        };
        cf.save(&cp).unwrap();
        // 连续覆盖多次，每次都必须读回完整状态
        for i in 0..50u64 {
            let mut c2 = cp.clone();
            c2.offset = 1000 + i;
            c2.batch = i;
            cf.save(&c2).unwrap();
            let g = cf.load().unwrap().unwrap();
            assert_eq!(g.offset, 1000 + i);
            assert_eq!(g.batch, i);
        }
    }

    #[test]
    fn no_temp_file_left_behind() {
        let (_d, cf) = tmpfile();
        cf.save(&Checkpoint::default()).unwrap();
        let leftovers: Vec<_> = std::fs::read_dir(cf.path.parent().unwrap())
            .unwrap()
            .filter_map(|e| e.ok())
            .filter(|e| e.file_name().to_string_lossy().contains("tmp"))
            .collect();
        assert!(leftovers.is_empty(), "rename 后不应残留临时文件");
    }

    #[test]
    fn resumable_rejects_changed_file() {
        let cp = Checkpoint {
            path: "/d/x".into(),
            offset: 100,
            batch: 1,
            total_size: 200,
            mtime: 5,
        };
        assert!(resumable(&cp, Path::new("/d/x"), 200, 5));
        // 大小变化
        assert!(!resumable(&cp, Path::new("/d/x"), 201, 5));
        // mtime 变化
        assert!(!resumable(&cp, Path::new("/d/x"), 200, 6));
        // 路径不同
        assert!(!resumable(&cp, Path::new("/d/y"), 200, 5));
        // 文件被截短到断点之前：offset 100 > size 50，必须拒绝续传
        let mut truncated = cp.clone();
        truncated.total_size = 50;
        assert!(!resumable(&truncated, Path::new("/d/x"), 50, 5));
    }

    #[test]
    fn clear_is_idempotent() {
        let (_d, cf) = tmpfile();
        cf.save(&Checkpoint::default()).unwrap();
        cf.clear().unwrap();
        cf.clear().unwrap(); // 再清一次不应报错
        assert!(cf.load().unwrap().is_none());
    }

    #[test]
    fn corrupt_checkpoint_is_error_not_panic() {
        let (_d, cf) = tmpfile();
        std::fs::write(&cf.path, b"offset=notanumber\n").unwrap();
        assert!(cf.load().is_err());
    }
}
