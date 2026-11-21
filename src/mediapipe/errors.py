"""流水线错误模型。

所有阶段错误都携带：
* ``phase``        —— 解析/映射/变换/写出/检查点/回读验证中的哪个阶段；
* ``stream_index`` —— 相关源流号（可空）；
* ``offset``       —— 可定位的源文件字节偏移，精确点或半开区间；
* ``exit_code``    —— CLI 退出码（2/3/4/5）。

无法精确定位首个坏字节时，必须给出偏移区间并置 ``exact=False``，
禁止伪称精确定位。
"""
from __future__ import annotations

from dataclasses import dataclass
from enum import Enum
from typing import Any


class Phase(str, Enum):
    DEMUX = "demux"
    MAPPING = "mapping"
    TRANSFORM = "transform"
    REMUX = "remux"
    CHECKPOINT = "checkpoint"
    VALIDATE = "validate"


# 验收约定的退出码
EXIT_OK = 0
EXIT_INPUT_INVALID = 2
EXIT_MEDIA_CORRUPT = 3
EXIT_CHECKPOINT_INCOMPATIBLE = 4
EXIT_INTERNAL = 5


@dataclass(frozen=True)
class SourceOffset:
    """源文件字节偏移。

    * ``end is None`` —— 精确定位到 ``start`` 这一个字节；
    * 否则表示半开区间 ``[start, end)``，即首个坏字节必在该范围内。
    """

    start: int
    end: int | None = None
    note: str = ""

    @property
    def exact(self) -> bool:
        return self.end is None

    def describe(self) -> str:
        if self.exact:
            text = f"byte {self.start}"
        else:
            text = f"bytes [{self.start}, {self.end})"
        return f"{text} ({self.note})" if self.note else text

    def to_dict(self) -> dict[str, Any]:
        return {
            "start": self.start,
            "end": self.end,
            "exact": self.exact,
            "note": self.note,
        }


class MediaError(Exception):
    """流水线错误基类。"""

    exit_code: int = EXIT_INTERNAL

    def __init__(
        self,
        message: str,
        *,
        phase: Phase | str | None = None,
        stream_index: int | None = None,
        offset: SourceOffset | None = None,
        detail: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.phase = Phase(phase) if isinstance(phase, str) and phase else phase
        self.stream_index = stream_index
        self.offset = offset
        self.detail = dict(detail or {})

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {
            "error": type(self).__name__,
            "message": str(self),
            "exit_code": self.exit_code,
            "phase": self.phase.value if isinstance(self.phase, Phase) else None,
            "stream_index": self.stream_index,
            "source_error_offset": self.offset.describe() if self.offset else None,
        }
        if self.offset is not None:
            out["source_offset"] = self.offset.to_dict()
        if self.detail:
            out["detail"] = self.detail
        return out


class InputValidationError(MediaError):
    """输入或映射配置无效（退出码 2）。"""

    exit_code = EXIT_INPUT_INVALID


class MediaCorruptionError(MediaError):
    """可定位的媒体损坏：截断、坏 box、坏包、缺失索引（退出码 3）。"""

    exit_code = EXIT_MEDIA_CORRUPT


class CheckpointIncompatibleError(MediaError):
    """续跑状态与当前输入不兼容（退出码 4）。"""

    exit_code = EXIT_CHECKPOINT_INCOMPATIBLE


class PipelineInternalError(MediaError):
    """内部写出或验证失败（退出码 5）。"""

    exit_code = EXIT_INTERNAL
