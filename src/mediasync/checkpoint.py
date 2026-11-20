"""分块检查点与本地索引管理。

提交协议（每块）：
1. 写 ``blocks/<n>.tmp`` 临时记录并 fsync；
2. 重算 SHA-256 校验，与块内声明比对；
3. 校验通过后原子 rename 为 ``blocks/<n>.json``（已确认块）；
4. 更新 ``manifest.json.tmp`` -> ``manifest.json``（已确认块列表）。

恢复时校验输入摘要、配置摘要与每个已确认块的校验和；任一不匹配则
把整个检查点目录隔离到 ``quarantine/``，从首个安全边界重算。
"""

from __future__ import annotations

import dataclasses
import hashlib
import json
import os
import shutil
import time
from pathlib import Path
from typing import Any

from .errors import ErrorBook, ErrorCode
from .safety import resolve_within

CHECKSUM_ALGO = "sha256"


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: str | Path) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 16), b""):
            digest.update(chunk)
    return digest.hexdigest()


def canonical_json(payload: Any) -> bytes:
    """排序键、无多余空白的确定性 JSON，用于摘要与重复运行一致性。"""
    return json.dumps(payload, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()


@dataclasses.dataclass(frozen=True)
class Block:
    sequence: int
    records: list[dict[str, Any]]
    checksum: str
    created_at: float

    @classmethod
    def build(cls, sequence: int, records: list[dict[str, Any]]) -> "Block":
        body = {"sequence": sequence, "records": records}
        return cls(
            sequence=sequence,
            records=records,
            checksum=sha256_bytes(canonical_json(body)),
            created_at=time.time(),
        )

    def body(self) -> dict[str, Any]:
        return {"sequence": self.sequence, "records": self.records}

    def to_dict(self) -> dict[str, Any]:
        return dataclasses.asdict(self)


def _atomic_write(path: Path, payload: bytes) -> None:
    tmp = path.with_suffix(path.suffix + ".tmp")
    with open(tmp, "wb") as handle:
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(tmp, path)


class CheckpointStore:
    """工作目录内的检查点存储，所有路径经安全边界解析。"""

    def __init__(self, root: str | Path, workdir: str | Path, errors: ErrorBook) -> None:
        self.dir = resolve_within(workdir, root)
        self.blocks_dir = self.dir / "blocks"
        self.manifest_path = self.dir / "manifest.json"
        self.errors = errors
        self.dir.mkdir(parents=True, exist_ok=True)
        self.blocks_dir.mkdir(exist_ok=True)

    # ---- 写入 ----------------------------------------------------------

    def commit_block(self, block: Block) -> Path:
        """临时写入 -> 校验 -> 原子替换；失败只影响当前未确认块。"""
        target = self.blocks_dir / f"{block.sequence:06d}.json"
        tmp = self.blocks_dir / f"{block.sequence:06d}.tmp"
        payload = canonical_json(block.to_dict())
        with open(tmp, "wb") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())

        with open(tmp, "rb") as handle:
            written = json.loads(handle.read())
        actual = sha256_bytes(
            canonical_json({"sequence": written["sequence"], "records": written["records"]})
        )
        declared = written["checksum"]
        if actual != declared or declared != block.checksum:
            tmp.unlink(missing_ok=True)
            self.errors.add(
                ErrorCode.CHECKPOINT_CORRUPT,
                f"块 {block.sequence} 临时记录校验失败: {actual} != {declared}",
                offset=block.sequence,
                fatal=True,
            )
            raise ValueError(f"block {block.sequence} checksum mismatch")
        os.replace(tmp, target)
        self._refresh_manifest()
        return target

    def _extract_checksum(self, path: Path) -> str:
        with open(path, "rb") as handle:
            return json.loads(handle.read())["checksum"]

    def _refresh_manifest(self) -> None:
        confirmed = sorted(p.name for p in self.blocks_dir.glob("*.json"))
        payload = canonical_json(
            {
                "checksum_algo": CHECKSUM_ALGO,
                "confirmed_blocks": confirmed,
                "updated_at": time.time(),
            }
        )
        _atomic_write(self.manifest_path, payload)

    # ---- 读取与校验 -----------------------------------------------------

    def load_manifest(self) -> dict[str, Any] | None:
        if not self.manifest_path.exists():
            return None
        with open(self.manifest_path, "rb") as handle:
            return json.loads(handle.read())

    def load_block(self, sequence: int) -> Block | None:
        path = self.blocks_dir / f"{sequence:06d}.json"
        if not path.exists():
            return None
        with open(path, "rb") as handle:
            data = json.loads(handle.read())
        return Block(
            sequence=data["sequence"],
            records=data["records"],
            checksum=data["checksum"],
            created_at=data.get("created_at", 0.0),
        )

    def verify_blocks(self) -> list[Block]:
        """校验全部已确认块；损坏块记录致命错误并抛出。

        未确认的 ``.tmp`` 是中断残留，不属于检查点状态，恢复前直接清理。
        """
        for stale in self.blocks_dir.glob("*.tmp"):
            stale.unlink(missing_ok=True)
        verified: list[Block] = []
        for path in sorted(self.blocks_dir.glob("*.json")):
            with open(path, "rb") as handle:
                raw = handle.read()
            data = json.loads(raw)
            body = canonical_json({"sequence": data["sequence"], "records": data["records"]})
            if sha256_bytes(body) != data["checksum"]:
                self.errors.add(
                    ErrorCode.CHECKPOINT_CORRUPT,
                    f"已确认块 {path.name} 校验和不匹配，块边界损坏",
                    source=str(path),
                    fatal=True,
                )
                raise ValueError(f"corrupt confirmed block {path.name}")
            verified.append(self.load_block(data["sequence"]))
        return verified

    def committed_count(self) -> int:
        return len(list(self.blocks_dir.glob("*.json")))

    # ---- 隔离 -----------------------------------------------------------

    def quarantine(self, reason: str) -> Path:
        """把当前检查点整体移动到 quarantine/ 下，返回隔离目录。"""
        qroot = self.dir.parent / "quarantine"
        qroot.mkdir(exist_ok=True)
        stamp = time.strftime("%Y%m%d-%H%M%S")
        dest = qroot / f"{self.dir.name}-{stamp}"
        shutil.move(str(self.dir), str(dest))
        (dest / "QUARANTINE_REASON.txt").write_text(reason + "\n")
        self.dir.mkdir(parents=True, exist_ok=True)
        self.blocks_dir.mkdir(exist_ok=True)
        return dest
