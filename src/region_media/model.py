"""统一媒体时钟、分片状态与错误模型。

所有模块共享这里的数据结构与常量：
* 时间戳内部统一使用微秒（us）整数，以首个音频采样点为原点；
* 传输层 33 位时间戳回绕周期随时钟频率变化，由 :class:`MediaClock`
  负责区分「回绕」与「真实时钟突变」；
* 错误以 :class:`ErrorEvent` 结构化记录，指标模块直接读取同一模型，
  避免各处重复计数。
"""

from __future__ import annotations

import time
from dataclasses import dataclass, field
from enum import Enum

# ---- 本地夹具安全限制（约束：限制单片字节数与清单引用数量） ----
MAX_SEGMENT_BYTES = 8 * 1024 * 1024
MAX_MANIFEST_REFS = 256
# 时钟突变判定：回绕补偿后仍超过该残差即视为真实跳变（us）
JUMP_RESIDUAL_US = 250_000
# 相邻时间戳正常增量上限（us），超出且不构成回绕视为断流
MAX_SANE_DELTA_US = 10_000_000
WRAP_BITS = 33
WRAP_MOD = 1 << WRAP_BITS


def now_us() -> int:
    """单调墙钟微秒，用于到达时间与处理延迟测量。"""
    return time.monotonic_ns() // 1_000


class TrackKind(str, Enum):
    AUDIO = "audio"
    VIDEO = "video"


class ManifestFormat(str, Enum):
    HLS = "hls"
    DASH = "dash"


class SegmentStatus(str, Enum):
    PENDING = "pending"
    RECEIVED = "received"
    DUPLICATE = "duplicate"
    RECOVERED = "recovered"
    MISSING = "missing"  # 不可恢复缺口
    REJECTED = "rejected"  # 清单/引用层面拒绝


class ErrorKind(str, Enum):
    PATH_TRAVERSAL = "path_traversal"
    DUPLICATE_SEQUENCE = "duplicate_sequence"
    MISSING_REFERENCE = "missing_reference"
    TRUNCATED_MANIFEST = "truncated_manifest"
    CORRUPT_PACKET = "corrupt_packet"
    EMPTY_TRACK = "empty_track"
    TIMESTAMP_WRAP = "timestamp_wrap"
    CLOCK_JUMP = "clock_jump"
    STREAM_GAP = "stream_gap"
    LATE_OUT_OF_WINDOW = "late_out_of_window"
    GAP_UNRECOVERABLE = "gap_unrecoverable"
    CHECKPOINT_MISMATCH = "checkpoint_mismatch"
    DISK_WRITE_FAILED = "disk_write_failed"
    INTERRUPTED = "interrupted"
    LIMIT_EXCEEDED = "limit_exceeded"


@dataclass(frozen=True)
class ErrorEvent:
    """结构化错误记录，location 形如 'manifest:seg#7' 便于定位。"""

    kind: ErrorKind
    location: str
    detail: str
    segment_seq: int | None = None
    at_us: int = field(default_factory=now_us)


@dataclass
class ManifestRef:
    """清单中的单条分片引用。"""

    seq: int
    uri: str
    duration_us: int
    byte_size: int = 0
    kind: TrackKind | None = None  # DASH 分轨清单区分音/视频
    rejected: bool = False  # 校验失败（越界/缺失/超限/冲突）标记


@dataclass
class SegmentState:
    """单个分片在流水线中的处理状态。"""

    seq: int
    uri: str
    status: SegmentStatus = SegmentStatus.PENDING
    packets: int = 0
    recovered_packets: int = 0
    errors: list[ErrorEvent] = field(default_factory=list)
    first_pts_us: int | None = None
    last_pts_us: int | None = None
    checksum: int = 0  # 内容指纹，检查点续跑一致性校验用

    def fail(self, kind: ErrorKind, location: str, detail: str) -> ErrorEvent:
        evt = ErrorEvent(kind, location, detail, segment_seq=self.seq)
        self.errors.append(evt)
        return evt


@dataclass(frozen=True)
class PacketModel:
    """解复用后的统一包模型（时间戳均已换算为微秒）。"""

    track: TrackKind
    seq: int  # 包在对应轨道内的序号
    segment_seq: int
    raw_pts: int  # 节点原始时钟刻度（可能已回绕）
    pts_us: int  # 解回绕后的统一时间线微秒
    dts_us: int
    duration_us: int
    arrival_us: int
    size_bytes: int
    keyframe: bool = False
    synthetic: bool = False  # 恢复模块补出的包标记为 True


class WrapResult(str, Enum):
    OK = "ok"
    WRAPPED = "wrapped"
    JUMP = "jump"


@dataclass
class MediaClock:
    """单轨道媒体时钟：33 位回绕解算与突变识别。

    timescale 为原始时钟每秒刻度数（如音频 48000、视频 90000）。
    解回绕输出以首个时间戳为原点的微秒时间线。
    """

    timescale: int
    track: TrackKind
    wrap_mod: int = WRAP_MOD
    _base_raw: int | None = field(default=None, init=False)
    _wraps: int = field(default=0, init=False)
    _last_raw: int | None = field(default=None, init=False)
    wrap_events: int = field(default=0, init=False)
    jump_events: int = field(default=0, init=False)

    @property
    def wrap_period_us(self) -> int:
        return self.wrap_mod * 1_000_000 // self.timescale

    def _to_us(self, extended_raw: int) -> int:
        assert self._base_raw is not None
        return (extended_raw - self._base_raw) * 1_000_000 // self.timescale

    def unwrap(self, raw_pts: int) -> tuple[int, WrapResult]:
        """提交一个原始时间戳，返回 (统一时间线 us, 事件类型)。"""
        raw_pts %= self.wrap_mod
        if self._base_raw is None:
            self._base_raw = raw_pts
            self._last_raw = raw_pts
            return 0, WrapResult.OK
        period = self.wrap_mod
        delta = raw_pts - self._last_raw
        result = WrapResult.OK
        if delta < -period // 2:
            # 候选回绕：补偿一个周期后检查残差是否落在合理增量内
            residual = delta + period
            if 0 <= residual <= MAX_SANE_DELTA_US * self.timescale // 1_000_000:
                self._wraps += 1
                self.wrap_events += 1
                delta = residual
                result = WrapResult.WRAPPED
            else:
                self.jump_events += 1
                result = WrapResult.JUMP
        elif delta > MAX_SANE_DELTA_US * self.timescale // 1_000_000:
            self.jump_events += 1
            result = WrapResult.JUMP
        elif delta < 0:
            # 小幅倒退（乱序/重传），不改时钟状态
            result = WrapResult.OK
        if result != WrapResult.WRAPPED:
            pass
        extended = raw_pts + self._wraps * period
        pts_us = self._to_us(extended)
        if result == WrapResult.JUMP:
            expected = self._to_us((self._last_raw or 0) + self._wraps * period)
            if abs(pts_us - expected) < JUMP_RESIDUAL_US:
                result = WrapResult.OK
                self.jump_events -= 1
        self._last_raw = raw_pts
        return pts_us, result

    def reset(self) -> None:
        self._base_raw = None
        self._wraps = 0
        self._last_raw = None
        self.wrap_events = 0
        self.jump_events = 0
