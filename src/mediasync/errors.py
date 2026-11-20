"""可序列化的媒体错误模型。

所有模块统一通过 :class:`ErrorBook` 收集问题，错误可序列化为 JSON，
恢复流程复用同一套错误分类（fatal 决定退出码与是否隔离检查点）。
"""

from __future__ import annotations

import dataclasses
import enum
import json
import time
from typing import Any


class ErrorCode(str, enum.Enum):
    """全部可核对的错误类型。"""

    INDEX_OUT_OF_RANGE = "index_out_of_range"
    INDEX_REVERSED = "index_reversed"
    INDEX_DUPLICATE = "index_duplicate"
    TIMESTAMP_BACKWARD = "timestamp_backward"
    ENCODER_RESTART = "encoder_restart"
    BITSTREAM_TRUNCATED = "bitstream_truncated"
    BITSTREAM_CORRUPT = "bitstream_corrupt"
    COLOR_CONFLICT = "color_conflict"
    COLOR_MISSING = "color_missing"
    AUDIO_MISSING_CHANNEL = "audio_missing_channel"
    AUDIO_EXTRA_CHANNEL = "audio_extra_channel"
    AUDIO_PARTIAL_TAIL = "audio_partial_tail"
    ALIGNMENT_IMPOSSIBLE = "alignment_impossible"
    CHECKPOINT_CORRUPT = "checkpoint_corrupt"
    CHECKPOINT_SUMMARY_MISMATCH = "checkpoint_summary_mismatch"
    PATH_TRAVERSAL = "path_traversal"
    CONFIG_ERROR = "config_error"


# 默认致命错误：出现即无法继续（对齐/损坏/检查点类）。
FATAL_CODES = frozenset(
    {
        ErrorCode.BITSTREAM_TRUNCATED,
        ErrorCode.BITSTREAM_CORRUPT,
        ErrorCode.ALIGNMENT_IMPOSSIBLE,
        ErrorCode.CHECKPOINT_CORRUPT,
        ErrorCode.CHECKPOINT_SUMMARY_MISMATCH,
        ErrorCode.PATH_TRAVERSAL,
        ErrorCode.CONFIG_ERROR,
    }
)


@dataclasses.dataclass
class MediaError:
    """单条错误记录，字段全部可 JSON 序列化。"""

    code: str
    message: str
    source: str = ""
    offset: int | None = None
    timestamp_ms: int | None = None
    detail: dict[str, Any] = dataclasses.field(default_factory=dict)
    fatal: bool = False
    created_at: float = dataclasses.field(default_factory=time.time)

    def to_dict(self) -> dict[str, Any]:
        return dataclasses.asdict(self)

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "MediaError":
        known = {f.name for f in dataclasses.fields(cls)}
        return cls(**{k: v for k, v in data.items() if k in known})


class ErrorBook:
    """错误收集器，支持合并、过滤与 JSON 往返。"""

    def __init__(self, errors: list[MediaError] | None = None) -> None:
        self.errors: list[MediaError] = list(errors or [])

    def add(
        self,
        code: ErrorCode | str,
        message: str,
        *,
        source: str = "",
        offset: int | None = None,
        timestamp_ms: int | None = None,
        detail: dict[str, Any] | None = None,
        fatal: bool | None = None,
    ) -> MediaError:
        code_value = code.value if isinstance(code, ErrorCode) else str(code)
        if fatal is None:
            try:
                fatal = ErrorCode(code_value) in FATAL_CODES
            except ValueError:
                fatal = False
        err = MediaError(
            code=code_value,
            message=message,
            source=source,
            offset=offset,
            timestamp_ms=timestamp_ms,
            detail=dict(detail or {}),
            fatal=fatal,
        )
        self.errors.append(err)
        return err

    def has(self, code: ErrorCode | str) -> bool:
        wanted = code.value if isinstance(code, ErrorCode) else str(code)
        return any(e.code == wanted for e in self.errors)

    def any_fatal(self) -> bool:
        return any(e.fatal for e in self.errors)

    def fatal(self) -> list[MediaError]:
        return [e for e in self.errors if e.fatal]

    def by_code(self, code: ErrorCode | str) -> list[MediaError]:
        wanted = code.value if isinstance(code, ErrorCode) else str(code)
        return [e for e in self.errors if e.code == wanted]

    def merge(self, other: "ErrorBook") -> None:
        self.errors.extend(other.errors)

    def to_json(self) -> str:
        return json.dumps(
            {"errors": [e.to_dict() for e in self.errors]},
            ensure_ascii=False,
            sort_keys=True,
        )

    @classmethod
    def from_json(cls, text: str) -> "ErrorBook":
        payload = json.loads(text)
        return cls([MediaError.from_dict(item) for item in payload.get("errors", [])])

    def summary(self) -> dict[str, int]:
        counts: dict[str, int] = {}
        for err in self.errors:
            counts[err.code] = counts.get(err.code, 0) + 1
        return dict(sorted(counts.items()))

    def __len__(self) -> int:
        return len(self.errors)

    def __iter__(self):
        return iter(self.errors)
