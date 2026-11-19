"""分块检查点：临时产物 + 原子提交的本地 SQLite 索引。

提交顺序固定为：
    1. 片段先写入同目录临时文件（.tmp-<chunk>）并 fsync；
    2. os.replace 原子替换为正式片段文件；
    3. 在同一事务内把提交记录写入 SQLite（WAL），提交后索引才可见。
因此“写产物中退出”留下的是可覆盖的临时文件，“更新索引时退出”
只会缺失提交记录 —— 恢复后重算该块即可，已提交区间绝不重复计入。
"""

from dataclasses import dataclass
import json
import os
import sqlite3
from pathlib import Path

from .errors import CheckpointError

SCHEMA_VERSION = 1
DB_NAME = "relaymix.sqlite3"
RUN_META_NAME = "run.json"


@dataclass(frozen=True)
class ChunkRecord:
    chunk_index: int
    start_tick: int
    end_tick: int
    frame_index: int
    segment_file: str
    segment_hash: str          # 片段字节 SHA-256（64 hex）
    input_hash: str            # 覆盖区间内源块哈希聚合
    params_fingerprint: str
    algo_version: str


class CheckpointStore:
    def __init__(self, root: str | Path):
        self.root = Path(root)
        self.segments_dir = self.root / "segments"
        self.db_path = self.root / DB_NAME
        self.meta_path = self.root / RUN_META_NAME
        self.root.mkdir(parents=True, exist_ok=True)
        self.segments_dir.mkdir(parents=True, exist_ok=True)
        self._conn = sqlite3.connect(self.db_path, isolation_level=None)
        self._conn.row_factory = sqlite3.Row
        self._conn.execute("PRAGMA journal_mode=WAL")
        self._conn.execute("PRAGMA synchronous=FULL")
        self._create_schema()

    def close(self) -> None:
        self._conn.close()

    def __enter__(self) -> "CheckpointStore":
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    def _create_schema(self) -> None:
        self._conn.executescript(
            """
            CREATE TABLE IF NOT EXISTS schema_meta (
                key TEXT PRIMARY KEY,
                value TEXT NOT NULL
            );
            CREATE TABLE IF NOT EXISTS chunks (
                chunk_index INTEGER PRIMARY KEY,
                start_tick INTEGER NOT NULL,
                end_tick INTEGER NOT NULL,
                frame_index INTEGER NOT NULL,
                segment_file TEXT NOT NULL,
                segment_hash TEXT NOT NULL,
                input_hash TEXT NOT NULL,
                params_fingerprint TEXT NOT NULL,
                algo_version TEXT NOT NULL,
                committed_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
            );
            CREATE TABLE IF NOT EXISTS keyframes (
                tick INTEGER PRIMARY KEY,
                chunk_index INTEGER NOT NULL,
                frame_index INTEGER NOT NULL,
                hit INTEGER NOT NULL
            );
            """
        )
        self._conn.execute(
            "INSERT OR IGNORE INTO schema_meta(key, value) VALUES('schema_version', ?)",
            (str(SCHEMA_VERSION),),
        )

    # ---------- 运行级元数据：清单/输入/算法/参数指纹 ----------

    def write_run_meta(self, *, manifest_json: str, input_fingerprint: str,
                       params_fingerprint: str, algo_version: str,
                       total_chunks: int) -> None:
        payload = {
            "manifest": json.loads(manifest_json),
            "input_fingerprint": input_fingerprint,
            "params_fingerprint": params_fingerprint,
            "algo_version": algo_version,
            "total_chunks": total_chunks,
            "schema_version": SCHEMA_VERSION,
        }
        tmp = self.meta_path.with_suffix(".json.tmp")
        tmp.write_text(json.dumps(payload, sort_keys=True), encoding="utf-8")
        os.replace(tmp, self.meta_path)

    def read_run_meta(self) -> dict:
        if not self.meta_path.exists():
            raise CheckpointError(
                "run metadata missing", error_code="run_meta_missing"
            )
        try:
            return json.loads(self.meta_path.read_text(encoding="utf-8"))
        except json.JSONDecodeError as exc:
            raise CheckpointError(
                "run metadata corrupt",
                error_code="run_meta_corrupt",
                byte_offset=exc.pos,
            ) from exc

    def validate_run(self, *, manifest_json: str, input_fingerprint: str,
                     params_fingerprint: str, algo_version: str) -> None:
        """恢复校验：清单、输入块、算法版本、参数指纹任一变化即拒绝混用。"""
        meta = self.read_run_meta()
        if meta.get("algo_version") != algo_version:
            raise CheckpointError(
                "algorithm version changed; recompute all chunks",
                error_code="algo_version_mismatch",
                expected=algo_version,
                found=meta.get("algo_version"),
                recompute_from_tick=0,
            )
        if meta.get("params_fingerprint") != params_fingerprint:
            raise CheckpointError(
                "parameter fingerprint changed; old segments cannot be reused",
                error_code="params_fingerprint_mismatch",
                recompute_from_tick=self._first_committed_tick(),
            )
        if meta.get("input_fingerprint") != input_fingerprint:
            raise CheckpointError(
                "input block fingerprints changed; recompute affected range",
                error_code="input_fingerprint_mismatch",
                recompute_from_tick=self._first_committed_tick(),
            )
        if json.dumps(meta.get("manifest"), sort_keys=True) != json.dumps(
            json.loads(manifest_json), sort_keys=True
        ):
            raise CheckpointError(
                "manifest content changed",
                error_code="manifest_mismatch",
                recompute_from_tick=self._first_committed_tick(),
            )

    def _first_committed_tick(self) -> int:
        row = self._conn.execute(
            "SELECT MIN(start_tick) AS t FROM chunks"
        ).fetchone()
        return int(row["t"]) if row and row["t"] is not None else 0

    # ---------- 分块提交 ----------

    def committed_indices(self) -> set[int]:
        return {
            int(r["chunk_index"])
            for r in self._conn.execute("SELECT chunk_index FROM chunks")
        }

    def is_committed(self, chunk_index: int) -> bool:
        row = self._conn.execute(
            "SELECT 1 FROM chunks WHERE chunk_index=?", (chunk_index,)
        ).fetchone()
        return row is not None

    def commit_chunk(self, record: ChunkRecord, payload: bytes) -> Path:
        """临时文件 -> 原子替换 -> 事务提交索引，三步顺序不可调换。"""
        final_path = self.segments_dir / record.segment_file
        tmp_path = self.segments_dir / f".tmp-{record.chunk_index}-{record.segment_file}"
        if self.is_committed(record.chunk_index):
            # 已提交区间绝不重复计入。
            return final_path
        with open(tmp_path, "wb") as fh:
            fh.write(payload)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp_path, final_path)
        self._conn.execute("BEGIN IMMEDIATE")
        try:
            self._conn.execute(
                """INSERT INTO chunks(chunk_index, start_tick, end_tick, frame_index,
                                      segment_file, segment_hash, input_hash,
                                      params_fingerprint, algo_version)
                   VALUES(?,?,?,?,?,?,?,?,?)""",
                (record.chunk_index, record.start_tick, record.end_tick,
                 record.frame_index, record.segment_file, record.segment_hash,
                 record.input_hash, record.params_fingerprint, record.algo_version),
            )
            self._conn.execute(
                "INSERT OR REPLACE INTO keyframes(tick, chunk_index, frame_index, hit)"
                " VALUES(?,?,?,1)",
                (record.start_tick, record.chunk_index, record.frame_index),
            )
            self._conn.execute("COMMIT")
        except Exception:
            self._conn.execute("ROLLBACK")
            raise
        return final_path

    def fetch_chunks(self) -> list[ChunkRecord]:
        rows = self._conn.execute(
            "SELECT * FROM chunks ORDER BY chunk_index"
        ).fetchall()
        return [
            ChunkRecord(
                chunk_index=r["chunk_index"], start_tick=r["start_tick"],
                end_tick=r["end_tick"], frame_index=r["frame_index"],
                segment_file=r["segment_file"], segment_hash=r["segment_hash"],
                input_hash=r["input_hash"],
                params_fingerprint=r["params_fingerprint"],
                algo_version=r["algo_version"],
            )
            for r in rows
        ]

    def lookup_keyframe(self, tick: int) -> sqlite3.Row | None:
        return self._conn.execute(
            "SELECT * FROM keyframes WHERE tick=?", (tick,)
        ).fetchone()

    def all_keyframes(self) -> list[sqlite3.Row]:
        return list(self._conn.execute("SELECT * FROM keyframes ORDER BY tick"))

    def mark_finished(self) -> None:
        tmp = self.root / ".finished.tmp"
        tmp.write_text("ok\n", encoding="utf-8")
        os.replace(tmp, self.root / ".finished")

    def is_finished(self) -> bool:
        return (self.root / ".finished").exists()
