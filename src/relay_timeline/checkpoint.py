"""检查点：分块落盘、原子提交与断点恢复。

每个块先写临时产物，再原子提交到索引。恢复时校验输入摘要，
仅重算未提交块；索引损坏、输出截断或任务标识冲突时拒绝静默
续跑，需显式 force_rebuild 才会推倒重来。
"""

from __future__ import annotations

import hashlib
import json
import os
import shutil
from pathlib import Path

from .errors import CheckpointError, RelayError

INDEX_NAME = "checkpoint.index.json"
MIN_FREE_BYTES = 64 * 1024 * 1024  # 写入前至少保留 64MB 剩余空间


def ensure_disk_space(path: Path, needed_bytes: int) -> None:
    free = shutil.disk_usage(path).free
    if free < max(needed_bytes, MIN_FREE_BYTES):
        raise RelayError(f"磁盘剩余空间不足: {free} 字节，需要 {needed_bytes}")


def file_digest(path: Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for block in iter(lambda: fh.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()


class CheckpointStore:
    def __init__(
        self,
        root: Path,
        task_id: str,
        input_digest: str,
        force_rebuild: bool = False,
    ):
        if not task_id or any(c in task_id for c in "/\\.."):
            raise CheckpointError(f"非法任务标识: {task_id!r}")
        self.root = Path(root)
        self.task_id = task_id
        self.input_digest = input_digest
        self.tmp_dir = self.root / f".tmp-{task_id}"
        self.index_path = self.root / INDEX_NAME
        self.chunks: dict[int, dict] = {}
        self._load(force_rebuild)

    def _load(self, force_rebuild: bool) -> None:
        if not self.index_path.exists():
            return
        try:
            data = json.loads(self.index_path.read_text())
            assert isinstance(data["chunks"], dict)
        except (ValueError, KeyError, AssertionError, TypeError) as exc:
            raise CheckpointError(f"检查点索引损坏: {self.index_path} ({exc})")
        if data.get("task_id") != self.task_id:
            raise CheckpointError(
                f"任务标识冲突: 索引属于 {data.get('task_id')!r}，"
                f"当前任务 {self.task_id!r}"
            )
        if data.get("input_digest") != self.input_digest:
            if not force_rebuild:
                raise CheckpointError(
                    "输入摘要与检查点不一致，拒绝静默续跑；"
                    "确认后使用 force_rebuild 重建"
                )
            self._wipe()
            return
        self.chunks = {int(k): v for k, v in data["chunks"].items()}
        self._verify_committed_outputs()

    def _wipe(self) -> None:
        for idx, meta in self.chunks.items():
            path = self.root / meta["file"]
            path.unlink(missing_ok=True)
        self.chunks = {}
        self.index_path.unlink(missing_ok=True)
        shutil.rmtree(self.tmp_dir, ignore_errors=True)

    def _verify_committed_outputs(self) -> None:
        corrupt: list[int] = []
        for idx, meta in sorted(self.chunks.items()):
            path = self.root / meta["file"]
            if (
                not path.exists()
                or path.stat().st_size == 0
                or file_digest(path) != meta["output_hash"]
            ):
                corrupt.append(idx)
        if corrupt:
            raise CheckpointError(
                f"检查点输出损坏或截断，冲突块: {corrupt}",
                conflict_blocks=corrupt,
            )

    def is_committed(self, idx: int, chunk_input_digest: str) -> bool:
        meta = self.chunks.get(idx)
        return bool(meta and meta["input_digest"] == chunk_input_digest)

    def chunk_path(self, idx: int) -> Path:
        return self.root / f"chunk-{self.task_id}-{idx:04d}.npy"

    def commit_chunk(
        self,
        idx: int,
        frame_start: int,
        frame_end: int,
        chunk_input_digest: str,
        payload: bytes,
    ) -> dict:
        """先写临时产物，校验后原子提交索引。"""
        ensure_disk_space(self.root, len(payload) * 2)
        self.tmp_dir.mkdir(parents=True, exist_ok=True)
        tmp = self.tmp_dir / f"chunk-{idx:04d}.npy.part"
        tmp.write_bytes(payload)
        final = self.chunk_path(idx)
        os.replace(tmp, final)
        meta = {
            "frame_start": frame_start,
            "frame_end": frame_end,
            "input_digest": chunk_input_digest,
            "output_hash": file_digest(final),
            "file": final.name,
            "committed": True,
        }
        self.chunks[idx] = meta
        self._flush_index()
        return meta

    def _flush_index(self) -> None:
        data = {
            "task_id": self.task_id,
            "input_digest": self.input_digest,
            "chunks": {str(k): v for k, v in sorted(self.chunks.items())},
        }
        tmp = self.index_path.with_suffix(".json.part")
        tmp.write_text(json.dumps(data, indent=2, sort_keys=True))
        os.replace(tmp, self.index_path)

    @property
    def committed_count(self) -> int:
        return len(self.chunks)
