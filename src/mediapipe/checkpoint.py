"""包边界检查点：持久化安全续跑状态。

检查点在每个已提交包边界可写，记录输入指纹、已提交全局包序号、
各轨时间状态与临时输出长度。写入采用 “临时文件 + fsync + os.replace”
原子替换；重启时先校验输入指纹与映射指纹，任一不匹配则拒绝续跑
（退出码 4），输入变化绝不沿用旧状态。
"""
from __future__ import annotations

import hashlib
import json
import os
import tempfile
from dataclasses import asdict, dataclass, field
from typing import Any

from .errors import CheckpointIncompatibleError, Phase, InputValidationError

CHECKPOINT_VERSION = 1
# 大文件指纹采样块大小与上限，避免全量读入内存
_FINGERPRINT_BLOCK = 1 << 20
_FINGERPRINT_MAX_BLOCKS = 64


def file_fingerprint(path: str) -> dict[str, Any]:
    """文件指纹：大小 + 头部/尾部/抽样块 SHA-256（流式，不全量载入）。"""
    size = os.path.getsize(path)
    h = hashlib.sha256()
    h.update(str(size).encode())
    with open(path, "rb") as fh:
        if size <= _FINGERPRINT_BLOCK * _FINGERPRINT_MAX_BLOCKS:
            while True:
                chunk = fh.read(_FINGERPRINT_BLOCK)
                if not chunk:
                    break
                h.update(chunk)
        else:
            # 头、尾与均匀抽样块，足够发现截断/改写/替换
            step = max(1, size // _FINGERPRINT_MAX_BLOCKS)
            for i in range(_FINGERPRINT_MAX_BLOCKS):
                fh.seek(min(i * step, size - 1))
                h.update(fh.read(_FINGERPRINT_BLOCK))
            fh.seek(max(0, size - _FINGERPRINT_BLOCK))
            h.update(fh.read(_FINGERPRINT_BLOCK))
    return {"size": size, "sha256": h.hexdigest()}


def ensure_safe_path(path: str, *, input_path: str | None = None) -> str:
    """路径安全：拒绝目录、符号链接逃逸、与输入同路径。返回绝对路径。"""
    ap = os.path.abspath(os.fspath(path))
    if os.path.isdir(ap):
        raise InputValidationError(f"目标路径是目录，拒绝写入: {ap}", phase=Phase.CHECKPOINT)
    real = os.path.realpath(ap)
    parent = os.path.dirname(real) or "."
    if not os.path.isdir(parent):
        raise InputValidationError(f"目标父目录不存在: {parent}", phase=Phase.CHECKPOINT)
    if input_path is not None and real == os.path.realpath(os.path.abspath(input_path)):
        raise InputValidationError(
            "输出/检查点路径与输入文件相同，拒绝覆盖输入", phase=Phase.CHECKPOINT
        )
    return ap


@dataclass(slots=True)
class StreamState:
    last_pts: int | None = None
    last_dts: int | None = None
    last_pts_us: int | None = None
    last_dts_us: int | None = None
    packet_count: int = 0


@dataclass(slots=True)
class CheckpointState:
    version: int
    input_fingerprint: dict[str, Any]
    mapping_fingerprint: str
    committed_sequence: int          # 已提交的最大全局包序号；-1 表示尚无包
    stream_states: dict[str, StreamState]
    output_bytes: int
    packet_digests: list[str] = field(default_factory=list)

    def to_json(self) -> str:
        data = asdict(self)
        return json.dumps(data, sort_keys=True, ensure_ascii=False)

    @classmethod
    def from_json(cls, text: str) -> "CheckpointState":
        data = json.loads(text)
        if data.get("version") != CHECKPOINT_VERSION:
            raise CheckpointIncompatibleError(
                f"检查点版本 {data.get('version')} 与当前 {CHECKPOINT_VERSION} 不兼容",
                phase=Phase.CHECKPOINT,
            )
        states = {
            str(k): StreamState(**v) for k, v in data["stream_states"].items()
        }
        return cls(
            version=data["version"],
            input_fingerprint=data["input_fingerprint"],
            mapping_fingerprint=data["mapping_fingerprint"],
            committed_sequence=int(data["committed_sequence"]),
            stream_states=states,
            output_bytes=int(data["output_bytes"]),
            packet_digests=list(data.get("packet_digests", [])),
        )


class CheckpointStore:
    """检查点文件的原子读写与兼容性校验。"""

    def __init__(self, path: str, *, input_path: str | None = None) -> None:
        self.path = ensure_safe_path(path, input_path=input_path)
        self.input_path = input_path

    def exists(self) -> bool:
        return os.path.isfile(self.path)

    def save(self, state: CheckpointState) -> None:
        directory = os.path.dirname(self.path) or "."
        fd, tmp = tempfile.mkstemp(prefix=".ckpt-", dir=directory)
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as fh:
                fh.write(state.to_json())
                fh.flush()
                os.fsync(fh.fileno())
            os.replace(tmp, self.path)  # 原子替换
        except BaseException:
            if os.path.exists(tmp):
                os.unlink(tmp)
            raise

    def load(self) -> CheckpointState | None:
        if not self.exists():
            return None
        try:
            with open(self.path, encoding="utf-8") as fh:
                return CheckpointState.from_json(fh.read())
        except (json.JSONDecodeError, KeyError, TypeError, ValueError) as exc:
            raise CheckpointIncompatibleError(
                f"检查点文件损坏，无法续跑: {exc}", phase=Phase.CHECKPOINT
            ) from exc

    def verify_resume(
        self,
        state: CheckpointState,
        current_fingerprint: dict[str, Any],
        current_mapping_fingerprint: str,
    ) -> None:
        """重启时校验指纹；输入或映射变化则拒绝续跑（退出码 4）。"""
        if state.input_fingerprint != current_fingerprint:
            raise CheckpointIncompatibleError(
                "输入文件指纹与检查点不一致：输入已变化，拒绝续跑",
                phase=Phase.CHECKPOINT,
                detail={
                    "checkpoint": state.input_fingerprint,
                    "current": current_fingerprint,
                },
            )
        if state.mapping_fingerprint != current_mapping_fingerprint:
            raise CheckpointIncompatibleError(
                "轨道映射指纹与检查点不一致，拒绝续跑",
                phase=Phase.CHECKPOINT,
                detail={
                    "checkpoint": state.mapping_fingerprint,
                    "current": current_mapping_fingerprint,
                },
            )


def mapping_fingerprint(mappings: list[Any]) -> str:
    """映射配置指纹：规范化后求 SHA-256（映射顺序已在 validate 中规范化）。"""
    normalized = json.dumps(
        [m.to_dict() for m in mappings], sort_keys=True
    )
    return hashlib.sha256(normalized.encode()).hexdigest()
