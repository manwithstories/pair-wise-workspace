"""核心数据类型与错误层级(无 IO、无解析逻辑)。

错误码与进程退出码对齐(见 cli.py):
    参数/配置错误 -> 2, 输入媒体错误 -> 3,
    输出写入失败 -> 4, 离线/流式不一致 -> 5
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from typing import Any, Optional


class ErrorPolicy(str, Enum):
    """轨道级错误的两种处理策略。"""

    FAIL_FAST = "fail_fast"
    BEST_EFFORT = "best_effort"


class MixerError(Exception):
    """所有混音器错误的基类,携带稳定的机器可读 code 与退出码。"""

    code: str = "mixer_error"
    exit_code: int = 1

    def __init__(self, message: str, details: Optional[dict] = None) -> None:
        super().__init__(message)
        self.message = message
        self.details = details or {}


class ConfigError(MixerError):
    """配置字段缺失、非法取值等可在运行前发现的问题。"""

    code = "config_error"
    exit_code = 2


class MediaError(MixerError):
    """轨道级输入媒体问题:空文件、损坏 WAV、非法采样率。"""

    code = "media_error"
    exit_code = 3


class OutputError(MixerError):
    """输出 WAV/报告无法写入(目录不可写、写入中断)。"""

    code = "output_error"
    exit_code = 4


class ConsistencyError(MixerError):
    """离线与流式两条路径数值差异超出阈值。"""

    code = "consistency_error"
    exit_code = 5


@dataclass(frozen=True)
class AutomationPoint:
    """自动化控制点,time 为轨道本地采样点(非秒)。"""

    time: int
    value: float


@dataclass(frozen=True)
class TrackSpec:
    """单轨声明:来源 WAV、偏移、目标声道、增益与自动化。"""

    name: str
    path: str
    offset: int = 0
    channels: tuple[int, int] = (0, 1)
    gain: float = 1.0
    automation: tuple[AutomationPoint, ...] = ()


@dataclass(frozen=True)
class ProjectConfig:
    """一份 project.json 的完整解析结果。"""

    sample_rate: int
    duration_frames: int
    tracks: tuple[TrackSpec, ...]
    error_policy: ErrorPolicy = ErrorPolicy.FAIL_FAST
    clip_threshold: float = 1.0
    block_frames: int = 1024

    @property
    def track_count(self) -> int:
        return len(self.tracks)


@dataclass
class TrackStat:
    """单轨混音前统计与 best-effort 错误记录。"""

    name: str
    source_sample_rate: int = 0
    source_channels: int = 0
    source_frames: int = 0
    project_frames: int = 0
    peak: float = 0.0
    clipped_samples: int = 0
    error: Optional[str] = None
    reason: Optional[str] = None


@dataclass
class StatsResult:
    """mix 命令写出的 JSON 统计结果。"""

    sample_rate: int
    channels: int
    frames: int
    tracks: int
    automation_samples: int
    peak_dbfs: float
    rms_dbfs: float
    effective_rms_dbfs: float
    clipped_samples: int
    clip_threshold: float
    mode: str
    block_frames: int
    error_policy: str
    track_stats: list[TrackStat] = field(default_factory=list)
    skipped_tracks: list[TrackStat] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        def track_dict(stat: TrackStat) -> dict[str, Any]:
            return {
                "name": stat.name,
                "source_sample_rate": stat.source_sample_rate,
                "source_channels": stat.source_channels,
                "source_frames": stat.source_frames,
                "project_frames": stat.project_frames,
                "peak": stat.peak,
                "clipped_samples": stat.clipped_samples,
                "error": stat.error,
                "reason": stat.reason,
            }

        return {
            "sample_rate": self.sample_rate,
            "channels": self.channels,
            "frames": self.frames,
            "tracks": self.tracks,
            "automation_samples": self.automation_samples,
            "peak_dbfs": self.peak_dbfs,
            "rms_dbfs": self.rms_dbfs,
            "effective_rms_dbfs": self.effective_rms_dbfs,
            "clipped_samples": self.clipped_samples,
            "clip_threshold": self.clip_threshold,
            "mode": self.mode,
            "block_frames": self.block_frames,
            "error_policy": self.error_policy,
            "track_stats": [track_dict(t) for t in self.track_stats],
            "skipped_tracks": [track_dict(t) for t in self.skipped_tracks],
        }


@dataclass
class Event:
    """时间线上的一段轨道素材(重采样到项目采样率后的规划)。"""

    track: TrackSpec
    start: int
    length: int


@dataclass
class MixBlock:
    """引擎处理的一个固定块,start 为项目时间线帧号。"""

    index: int
    start: int
    frames: int
    buffer: Any = None  # numpy.ndarray, 形状 (frames, 2)
