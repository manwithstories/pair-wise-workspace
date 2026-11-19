"""共享错误记录：贯穿各阶段，携带分片来源、时间范围与可恢复性。"""
from __future__ import annotations

from dataclasses import dataclass


@dataclass
class MediaError:
    """统一错误记录。

    stage:       产生阶段（manifest/transport/decode/audio/pipeline）
    message:     人类可读描述
    segment:     分片来源 URI（可为空）
    time_range:  媒体时间范围（秒，清单时间基准）
    recoverable: 是否可恢复（可静音填充/跳过）；False 表示不可恢复媒体错误
    """

    stage: str
    message: str
    segment: str | None = None
    time_range: tuple[float, float] | None = None
    recoverable: bool = True

    def as_dict(self) -> dict:
        return {
            "stage": self.stage,
            "message": self.message,
            "segment": self.segment,
            "time_range": list(self.time_range) if self.time_range else None,
            "recoverable": self.recoverable,
        }


class ManifestError(Exception):
    """清单非法：时长非法、时间越界或分片引用无法解析。"""


class DecodeError(Exception):
    """分片解封装/解码失败。"""


class PipelineError(Exception):
    """流水线阶段失败或已取消。"""
