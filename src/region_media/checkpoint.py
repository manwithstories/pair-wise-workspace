"""SQLite 事务检查点与中断续跑。

每个分块（chunk）在一个 ``BEGIN IMMEDIATE`` 事务内写入分片状态、
计数器与错误事件，提交采用 SQLite 原子事务（WAL + 完整 fsync）：
写入中途失败（含磁盘错误、中断信号）只回滚，已提交的最后一个
有效检查点保持不变，绝不伪报成功。

续跑时三重校验：输入清单摘要、处理配置、分块进度三者必须匹配；
不匹配返回 CHECKPOINT_MISMATCH 并拒绝沿用旧索引，避免重复计数或
覆盖不匹配结果。
"""

from __future__ import annotations

import json
import os
import sqlite3
from dataclasses import dataclass

from .model import ErrorEvent, ErrorKind, SegmentStatus

SCHEMA_VERSION = 1


class CheckpointError(RuntimeError):
    def __init__(self, kind: ErrorKind, detail: str) -> None:
        super().__init__(detail)
        self.kind = kind
        self.event = ErrorEvent(kind, "checkpoint", detail)


@dataclass(frozen=True)
class RunConfig:
    manifest_path: str
    chunk_size: int
    profile_seed: int
    input_digest: int

    def as_dict(self) -> dict:
        return {"manifest_path": os.path.abspath(self.manifest_path),
                "chunk_size": self.chunk_size,
                "profile_seed": self.profile_seed,
                "input_digest": self.input_digest}


class CheckpointStore:
    def __init__(self, path: str) -> None:
        self.path = path
        fresh = not os.path.exists(path)
        self._conn = sqlite3.connect(path)
        self._conn.execute("PRAGMA journal_mode=WAL")
        self._conn.execute("PRAGMA synchronous=FULL")
        self._init_schema()
        self._fresh = fresh

    def _init_schema(self) -> None:
        self._conn.executescript(
            """
            CREATE TABLE IF NOT EXISTS meta (
                key TEXT PRIMARY KEY,
                value TEXT NOT NULL
            );
            CREATE TABLE IF NOT EXISTS chunks (
                chunk_id INTEGER PRIMARY KEY,
                last_segment_seq INTEGER NOT NULL,
                counters_json TEXT NOT NULL,
                committed_at_us INTEGER NOT NULL
            );
            CREATE TABLE IF NOT EXISTS segments (
                segment_seq INTEGER PRIMARY KEY,
                status TEXT NOT NULL,
                packets INTEGER NOT NULL,
                recovered_packets INTEGER NOT NULL,
                checksum INTEGER NOT NULL
            );
            CREATE TABLE IF NOT EXISTS errors (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                kind TEXT NOT NULL,
                location TEXT NOT NULL,
                detail TEXT NOT NULL,
                segment_seq INTEGER
            );
            """)
        self._conn.commit()

    def initialize_run(self, config: RunConfig) -> bool:
        """写入运行配置；返回 True 表示新建运行，False 表示续跑。"""
        row = self._conn.execute(
            "SELECT value FROM meta WHERE key='config'").fetchone()
        payload = json.dumps(config.as_dict(), sort_keys=True)
        if row is None:
            self._conn.execute(
                "INSERT INTO meta(key, value) VALUES('config', ?)", (payload,))
            self._conn.execute(
                "INSERT INTO meta(key, value) VALUES('schema_version', ?)",
                (str(SCHEMA_VERSION),))
            self._conn.commit()
            return True
        if row[0] != payload:
            raise CheckpointError(
                ErrorKind.CHECKPOINT_MISMATCH,
                "输入摘要或配置与既有检查点不一致，拒绝续跑")
        return False

    def resume_from(self) -> int:
        """返回已提交的最大分片序号；无检查点返回 -1。"""
        row = self._conn.execute(
            "SELECT MAX(last_segment_seq) FROM chunks").fetchone()
        return int(row[0]) if row and row[0] is not None else -1

    def commit_chunk(self, chunk_id: int, last_segment_seq: int,
                     counters: dict, segment_rows: list[tuple],
                     errors: list[ErrorEvent]) -> None:
        """单事务原子提交一个分块；任何异常都回滚并向上抛出。"""
        try:
            self._conn.execute("BEGIN IMMEDIATE")
            self._conn.execute(
                "INSERT INTO chunks(chunk_id, last_segment_seq,"
                " counters_json, committed_at_us) VALUES(?, ?, ?, ?)",
                (chunk_id, last_segment_seq,
                 json.dumps(counters, sort_keys=True),
                 _now()))
            self._conn.executemany(
                "INSERT OR REPLACE INTO segments(segment_seq, status,"
                " packets, recovered_packets, checksum)"
                " VALUES(?, ?, ?, ?, ?)", segment_rows)
            self._conn.executemany(
                "INSERT INTO errors(kind, location, detail, segment_seq)"
                " VALUES(?, ?, ?, ?)",
                [(e.kind.value, e.location, e.detail, e.segment_seq)
                 for e in errors])
            self._conn.commit()
        except sqlite3.Error as exc:
            self._conn.rollback()
            raise CheckpointError(ErrorKind.DISK_WRITE_FAILED,
                                  f"检查点写入失败已回滚: {exc}") from exc

    def load_counters(self) -> dict:
        rows = self._conn.execute(
            "SELECT counters_json FROM chunks ORDER BY chunk_id").fetchall()
        merged: dict = {}
        for (raw,) in rows:
            for k, v in json.loads(raw).items():
                merged[k] = merged.get(k, 0) + v
        return merged

    def load_errors(self) -> list[ErrorEvent]:
        cur = self._conn.execute(
            "SELECT kind, location, detail, segment_seq FROM errors ORDER BY id")
        return [ErrorEvent(ErrorKind(k), loc, detail, seq)
                for k, loc, detail, seq in cur.fetchall()]

    def load_segments(self) -> dict[int, dict]:
        cur = self._conn.execute(
            "SELECT segment_seq, status, packets, recovered_packets, checksum"
            " FROM segments")
        return {seq: {"status": SegmentStatus(st), "packets": pk,
                      "recovered_packets": rc, "checksum": cs}
                for seq, st, pk, rc, cs in cur.fetchall()}

    def close(self) -> None:
        self._conn.close()


def _now() -> int:
    from .model import now_us
    return now_us()
