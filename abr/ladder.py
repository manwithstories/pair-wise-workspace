"""码率阶梯：加载 JSON、校验单调性、分片字节换算。"""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path


class LadderValidationError(Exception):
    """阶梯配置非法（空、非正、非严格递增等）。"""


@dataclass(frozen=True)
class Ladder:
    bitrates_kbps: tuple[int, ...]
    chunk_sec: float = 4.0

    def __post_init__(self) -> None:
        if not self.bitrates_kbps:
            raise LadderValidationError("ladder must contain at least one bitrate")
        if any(b <= 0 for b in self.bitrates_kbps):
            raise LadderValidationError("bitrates must be positive")
        if any(b2 <= b1 for b1, b2 in zip(self.bitrates_kbps, self.bitrates_kbps[1:])):
            raise LadderValidationError("bitrates must be strictly increasing")
        if self.chunk_sec <= 0:
            raise LadderValidationError("chunk_sec must be positive")

    @property
    def lowest(self) -> int:
        return self.bitrates_kbps[0]

    @property
    def highest(self) -> int:
        return self.bitrates_kbps[-1]

    def chunk_bytes(self, bitrate_kbps: float) -> float:
        """指定码率一个分片的字节数。"""
        return bitrate_kbps * 1000.0 / 8.0 * self.chunk_sec

    @classmethod
    def from_json(cls, path: str | Path, chunk_sec: float | None = None) -> "Ladder":
        path = Path(path)
        if not path.exists():
            raise LadderValidationError(f"ladder file not found: {path}")
        try:
            data = json.loads(path.read_text(encoding="utf-8"))
        except json.JSONDecodeError as exc:
            raise LadderValidationError(f"invalid JSON in {path}: {exc}") from None
        if not isinstance(data, dict) or "bitrates_kbps" not in data:
            raise LadderValidationError("ladder JSON must contain 'bitrates_kbps'")
        bitrates = data["bitrates_kbps"]
        if not isinstance(bitrates, list) or not all(isinstance(b, (int, float)) for b in bitrates):
            raise LadderValidationError("'bitrates_kbps' must be a list of numbers")
        effective_chunk = chunk_sec if chunk_sec is not None else float(data.get("chunk_sec", 4.0))
        return cls(bitrates_kbps=tuple(int(b) for b in bitrates), chunk_sec=effective_chunk)
