"""领域类型定义: 时间码、RGBA 图层、音频事件与合成结果。

时间一律以 :class:`fractions.Fraction` 表示秒数, 保证 24/30/60 fps
与任意整数采样率之间的换算精确可重复, 不使用浮点参与时间比较。
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from fractions import Fraction


# ---------------------------------------------------------------------------
# 退出码与错误类型
# ---------------------------------------------------------------------------

EXIT_OK = 0
EXIT_VALIDATION = 2
EXIT_RENDER = 3
EXIT_DIGEST_WRITE = 4


class ErrorType(str, Enum):
    """校验/运行期错误类型, 字符串值直接写入 JSON 报告。"""

    JSON_CORRUPT = "json_corrupt"
    MISSING_FIELD = "missing_field"
    BAD_TYPE = "bad_type"
    BAD_VALUE = "bad_value"
    SIZE_MISMATCH = "size_mismatch"
    ALPHA_OUT_OF_RANGE = "alpha_out_of_range"
    NON_MONOTONIC_TIME = "non_monotonic_time"
    NEGATIVE_DURATION = "negative_duration"
    FRAME_OUT_OF_RANGE = "frame_out_of_range"
    UNKNOWN_BLEND = "unknown_blend_mode"
    RENDER_FAILED = "render_failed"
    AUDIO_FAILED = "audio_failed"
    DIGEST_WRITE_FAILED = "digest_write_failed"


@dataclass(frozen=True)
class TimelineError:
    """单条可定位错误。location 形如 ``layers[2].alpha``。"""

    location: str
    error_type: ErrorType
    message: str

    def to_dict(self) -> dict:
        return {
            "location": self.location,
            "error_type": self.error_type.value,
            "message": self.message,
        }


class CompositorError(Exception):
    """渲染/音频处理阶段的硬错误(对应退出码 3)。"""

    def __init__(self, message: str, error_type: ErrorType = ErrorType.RENDER_FAILED):
        super().__init__(message)
        self.error_type = error_type


class DigestWriteError(CompositorError):
    """摘要落盘失败(对应退出码 4)。"""

    def __init__(self, message: str):
        super().__init__(message, ErrorType.DIGEST_WRITE_FAILED)


# ---------------------------------------------------------------------------
# 时间换算 —— 全部基于 Fraction, 结果精确确定
# ---------------------------------------------------------------------------

def frame_to_second(frame: int, fps: int) -> Fraction:
    """帧号 -> 该帧起始时刻(秒)。"""
    return Fraction(frame, fps)


def second_to_frame(t: Fraction, fps: int) -> int:
    """秒 -> 帧号(向下取整), 用于把任意有理数时间吸附到帧网格。"""
    return (t * fps).numerator // (t * fps).denominator


def sample_to_second(sample: int, sample_rate: int) -> Fraction:
    """采样点序号 -> 该采样点起始时刻(秒)。"""
    return Fraction(sample, sample_rate)


def second_to_sample(t: Fraction, sample_rate: int) -> int:
    """秒 -> 采样点序号(向下取整)。"""
    return (t * sample_rate).numerator // (t * sample_rate).denominator


def us(t: Fraction) -> int:
    """有理数秒 -> 整数微秒(向下取整), 用于 av_sync_error_us 等输出。"""
    return (t * 1_000_000).numerator // (t * 1_000_000).denominator


# ---------------------------------------------------------------------------
# 素材与图层
# ---------------------------------------------------------------------------

class BlendMode(str, Enum):
    SOURCE_OVER = "source-over"
    ADD = "add"
    MULTIPLY = "multiply"
    SCREEN = "screen"


@dataclass(frozen=True)
class Source:
    """程序化素材声明。spec 由 renderer 物化为 RGBA 数组, 不读取外部文件。

    kind:
      - ``gradient``: 对角线性渐变, spec={color_a:[r,g,b], color_b:[...]}
      - ``sprite``:   半透明径向精灵, spec={center_rgb, center_alpha, edge_alpha}
      - ``mask``:     椭圆遮罩, spec={rx_ratio, ry_ratio}(仅用作局部遮罩)
    """

    source_id: str
    kind: str
    width: int
    height: int
    spec: dict


@dataclass(frozen=True)
class Layer:
    """单个 RGBA 图层。

    时间区间为半开 ``[frame_in, frame_out)``: 边界帧归属后一图层,
    同一帧不会被重复合成。
    """

    layer_id: str
    source_id: str
    frame_in: int
    frame_out: int
    track: int
    order: int                      # 同轨道内的声明顺序(数组下标)
    x: int                          # 图层左上角在画布上的像素坐标(可为负)
    y: int
    scale: float
    rotation: float                 # 逆时针角度(度)
    alpha: float                    # 0.0~1.0 全局不透明度
    blend: BlendMode
    mask_source_id: str | None = None


@dataclass(frozen=True)
class AudioEvent:
    """音效事件: 有理数起始秒 + 持续秒, 运行期映射到采样点。"""

    event_id: str
    start: Fraction
    duration: Fraction

    @property
    def end(self) -> Fraction:
        return self.start + self.duration


@dataclass(frozen=True)
class Timeline:
    """通过校验的时间线模型, 渲染器与音频分析器只消费该对象。"""

    width: int
    height: int
    fps: int
    sample_rate: int
    frame_count: int
    sources: dict[str, Source]
    layers: list[Layer] = field(default_factory=list)
    audio_events: list[AudioEvent] = field(default_factory=list)

    @property
    def duration(self) -> Fraction:
        return frame_to_second(self.frame_count, self.fps)


# ---------------------------------------------------------------------------
# 合成结果与摘要
# ---------------------------------------------------------------------------

@dataclass(frozen=True)
class FrameDigest:
    """单帧摘要 —— 不落盘任何像素, 只记录哈希与统计量。"""

    frame_index: int
    pixel_sha256: str               # 64 位十六进制
    opaque_pixels: int
    transparent_pixels: int
    audio_start_sample: int         # 该帧起始时刻对应的采样点
    av_sync_error_us: int


@dataclass(frozen=True)
class AudioEventAnalysis:
    """单个音频事件相对视频帧网格的对齐分析。"""

    event_id: str
    start_second_numerator: int
    start_second_denominator: int
    first_sample: int               # 首个有效采样点
    last_sample: int                # 末个有效采样点(含)
    end_frame: int                  # 事件结束时刻所在/跨越到的帧号
    av_sync_error_us: int           # 起始点到最近帧边界的误差(微秒)


@dataclass(frozen=True)
class RenderSummary:
    """整次渲染的全局摘要。"""

    frame_count: int
    width: int
    height: int
    fps: int
    sample_rate: int
    summary_sha256: str
    max_av_sync_error_us: int
    total_opaque_pixels: int
    total_transparent_pixels: int
    audio_event_count: int
