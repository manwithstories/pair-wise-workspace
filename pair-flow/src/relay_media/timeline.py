"""整数微秒媒体时间基准与跨模块错误模型。

所有模块只通过本模块的时间戳交换时间信息：内部一律使用整数微秒，
避免浮点漂移；无法可靠换算的片段返回 None 并登记错误，不得静默并入时间线。
"""

from __future__ import annotations

import enum
import hashlib
import statistics
from dataclasses import dataclass, field

US_PER_SECOND = 1_000_000


class Stage(enum.StrEnum):
    """流水线阶段名称，错误记录必须携带其中之一。"""

    PARSE = "parse"
    INDEX = "index"
    TIMELINE = "timeline"
    SEGMENT = "segment"
    SILENCE = "silence"
    FINGERPRINT = "fingerprint"
    TRANSPORT = "transport"
    VERIFY = "verify"


class ErrorKind(enum.StrEnum):
    TRUNCATED_UNIT = "truncated_unit"
    CORRUPT_UNIT = "corrupt_unit"
    CODEC_MISMATCH = "codec_mismatch"
    INDEX_MISSING_PAGE = "index_missing_page"
    INDEX_OFFSET_OUT_OF_RANGE = "index_offset_out_of_range"
    INDEX_NON_MONOTONIC = "index_non_monotonic"
    INDEX_DUPLICATE = "index_duplicate"
    TIMESTAMP_MISSING = "timestamp_missing"
    TIMESTAMP_WRAP = "timestamp_wrap"
    TIMESTAMP_NON_MONOTONIC = "timestamp_non_monotonic"
    UNMAPPABLE_FRAGMENT = "unmappable_fragment"
    DEGRADED_BOUNDARY = "degraded_boundary"
    UNRECOVERED_RANGE = "unrecovered_range"
    CHECKSUM_MISMATCH = "checksum_mismatch"


@dataclass(slots=True)
class MediaError:
    """跨模块统一错误记录，offset 为输入文件字节偏移，无字节语义时为 -1。"""

    stage: str
    kind: str
    offset: int
    message: str
    recovered: bool = False
    context: dict = field(default_factory=dict)

    def to_dict(self) -> dict:
        return {
            "stage": self.stage,
            "kind": self.kind,
            "offset": self.offset,
            "message": self.message,
            "recovered": self.recovered,
            "context": self.context,
        }

    @classmethod
    def from_dict(cls, data: dict) -> "MediaError":
        return cls(
            stage=data["stage"],
            kind=data["kind"],
            offset=data["offset"],
            message=data["message"],
            recovered=data.get("recovered", False),
            context=dict(data.get("context", {})),
        )


@dataclass(slots=True)
class TimelinePoint:
    ordinal: int
    pts_us: int
    dts_us: int | None
    keyframe: bool
    source_offset: int
    estimated: bool = False
    clock_wrapped: bool = False


def sha256_hex(data: bytes | bytearray) -> str:
    return hashlib.sha256(data).hexdigest()


def samples_to_us(sample_index: int, sample_rate: int) -> int:
    """采样序号 -> 整数微秒（四舍五入）。"""

    return (sample_index * US_PER_SECOND + sample_rate // 2) // sample_rate


def seconds_to_us(value: float) -> int:
    return int(round(value * US_PER_SECOND))


class TimestampNormalizer:
    """把原始滴答时间戳归一化为整数微秒时间轴。

    处理：时间戳缺失（按帧间隔中位数补点并标记）、回绕（已知模数）、
    非单调（钳制并标记）、可变帧率（保留真实间隔，仅缺帧时估值）。
    无法可靠换算（无锚点缺帧等）的输入返回 None，由调用方隔离。
    """

    def __init__(self, tick_rate: int, wrap_modulus: int | None = None) -> None:
        if tick_rate <= 0:
            raise ValueError("tick_rate 必须为正整数")
        self.tick_rate = tick_rate
        self.wrap_modulus = wrap_modulus
        self.points: list[TimelinePoint] = []
        self.errors: list[MediaError] = []
        self._cycle = 0
        self._last_tick: int | None = None
        self._last_us: int | None = None
        self._deltas_us: list[int] = []

    def _median_delta_us(self) -> int:
        if self._deltas_us:
            return int(statistics.median(self._deltas_us))
        return US_PER_SECOND // 25  # 缺省 25fps，仅用于首个丢点后的兜底

    def feed(
        self,
        ordinal: int,
        tick: int | None,
        source_offset: int,
        keyframe: bool = False,
    ) -> TimelinePoint | None:
        estimated = False
        wrapped = False
        if tick is None:
            if self._last_us is None:
                self.errors.append(
                    MediaError(
                        Stage.TIMELINE,
                        ErrorKind.TIMESTAMP_MISSING,
                        source_offset,
                        "首帧即缺失时间戳，无锚点可换算",
                        recovered=False,
                    )
                )
                return None
            tick_us = self._last_us + self._median_delta_us()
            estimated = True
            self.errors.append(
                MediaError(
                    Stage.TIMELINE,
                    ErrorKind.TIMESTAMP_MISSING,
                    source_offset,
                    "时间戳缺失，按帧间隔中位数补点",
                    recovered=True,
                    context={"pts_us": tick_us},
                )
            )
        else:
            raw = int(tick)
            if self.wrap_modulus and self._last_tick is not None:
                gap = self._last_tick - raw
                if gap > self.wrap_modulus // 2:
                    self._cycle += 1
                    wrapped = True
                    self.errors.append(
                        MediaError(
                            Stage.TIMELINE,
                            ErrorKind.TIMESTAMP_WRAP,
                            source_offset,
                            f"时钟回绕（模数 {self.wrap_modulus}）",
                            recovered=True,
                            context={"cycle": self._cycle},
                        )
                    )
            unwrapped = raw + (
                self._cycle * self.wrap_modulus if self.wrap_modulus else 0
            )
            tick_us = (unwrapped * US_PER_SECOND) // self.tick_rate
            if self._last_us is not None and tick_us < self._last_us:
                self.errors.append(
                    MediaError(
                        Stage.TIMELINE,
                        ErrorKind.TIMESTAMP_NON_MONOTONIC,
                        source_offset,
                        "非单调时间戳，钳制到上一帧之后",
                        recovered=True,
                        context={"raw_pts_us": tick_us},
                    )
                )
                tick_us = self._last_us
            self._last_tick = raw
        if self._last_us is not None and tick_us > self._last_us:
            self._deltas_us.append(tick_us - self._last_us)
        point = TimelinePoint(
            ordinal=ordinal,
            pts_us=tick_us,
            dts_us=tick_us,
            keyframe=keyframe,
            source_offset=source_offset,
            estimated=estimated,
            clock_wrapped=wrapped,
        )
        self.points.append(point)
        self._last_us = tick_us
        return point
