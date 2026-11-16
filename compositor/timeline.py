"""JSON 时间线解析与校验。

职责:
- 把 JSON 数据解析为 :class:`~compositor.types.Timeline` 不可变模型;
- 统一帧号(整数)与采样点(由音频模块按有理数换算);
- 收集式校验: 一次运行报出全部错误, 每条带 location/error_type/message,
  并统计仍可继续处理的条目数。
"""

from __future__ import annotations

import json
from fractions import Fraction
from pathlib import Path

from .types import (
    AudioEvent,
    BlendMode,
    ErrorType,
    Layer,
    Source,
    Timeline,
    TimelineError,
)

_ALLOWED_FPS = (24, 30, 60)
_SOURCE_KINDS = ("gradient", "sprite", "mask")
_BLEND_VALUES = {m.value for m in BlendMode}


class ValidationReport:
    """收集校验错误并统计可继续处理的条目。"""

    def __init__(self) -> None:
        self.errors: list[TimelineError] = []
        self.valid_sources = 0
        self.valid_layers = 0
        self.valid_audio_events = 0

    def add(self, location: str, error_type: ErrorType, message: str) -> None:
        self.errors.append(TimelineError(location, error_type, message))

    @property
    def recoverable_entries(self) -> int:
        """通过校验、仍可被下游消费的条目总数。"""
        return self.valid_sources + self.valid_layers + self.valid_audio_events

    def to_dict(self) -> dict:
        return {
            "ok": not self.errors,
            "errors": [e.to_dict() for e in self.errors],
            "recoverable_entries": self.recoverable_entries,
            "valid_sources": self.valid_sources,
            "valid_layers": self.valid_layers,
            "valid_audio_events": self.valid_audio_events,
        }


def _parse_rational(value, location: str, report: ValidationReport) -> Fraction | None:
    """把 int/float/``"a/b"`` 字符串解析为精确 Fraction。"""
    if isinstance(value, bool):
        report.add(location, ErrorType.BAD_TYPE, "期望有理数, 实际为布尔值")
        return None
    if isinstance(value, int):
        return Fraction(value)
    if isinstance(value, float):
        return Fraction(value)
    if isinstance(value, str):
        try:
            return Fraction(value)
        except (ValueError, ZeroDivisionError):
            report.add(location, ErrorType.BAD_VALUE, f"无法解析的有理数: {value!r}")
            return None
    report.add(location, ErrorType.BAD_TYPE, f"期望有理数, 实际类型为 {type(value).__name__}")
    return None


def _require(mapping: dict, key: str, location: str, report: ValidationReport):
    if key not in mapping:
        report.add(f"{location}.{key}", ErrorType.MISSING_FIELD, f"缺少必填字段 {key!r}")
        return None
    return mapping[key]


def _is_int(value) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def _is_number(value) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool)


def _parse_sources(raw_sources, report: ValidationReport) -> dict[str, Source]:
    sources: dict[str, Source] = {}
    if not isinstance(raw_sources, list):
        report.add("sources", ErrorType.BAD_TYPE, "sources 必须是数组")
        return sources
    for i, item in enumerate(raw_sources):
        loc = f"sources[{i}]"
        if not isinstance(item, dict):
            report.add(loc, ErrorType.BAD_TYPE, "素材条目必须是对象")
            continue
        sid = _require(item, "id", loc, report)
        kind = _require(item, "kind", loc, report)
        width = _require(item, "width", loc, report)
        height = _require(item, "height", loc, report)
        spec = item.get("spec", {})
        ok = True
        if not isinstance(sid, str) or not sid:
            report.add(f"{loc}.id", ErrorType.BAD_VALUE, "素材 id 必须是非空字符串")
            ok = False
        if kind not in _SOURCE_KINDS:
            report.add(f"{loc}.kind", ErrorType.BAD_VALUE, f"未知素材类型 {kind!r}")
            ok = False
        if not _is_int(width) or width <= 0:
            report.add(f"{loc}.width", ErrorType.BAD_VALUE, "width 必须是正整数")
            ok = False
        if not _is_int(height) or height <= 0:
            report.add(f"{loc}.height", ErrorType.BAD_VALUE, "height 必须是正整数")
            ok = False
        if not isinstance(spec, dict):
            report.add(f"{loc}.spec", ErrorType.BAD_TYPE, "spec 必须是对象")
            ok = False
        if ok and sid in sources:
            report.add(f"{loc}.id", ErrorType.BAD_VALUE, f"素材 id 重复: {sid!r}")
            ok = False
        if ok:
            sources[sid] = Source(sid, kind, width, height, spec)
            report.valid_sources += 1
    return sources


def _parse_layers(raw_layers, sources: dict[str, Source], frame_count: int,
                  report: ValidationReport) -> list[Layer]:
    layers: list[Layer] = []
    if not isinstance(raw_layers, list):
        report.add("layers", ErrorType.BAD_TYPE, "layers 必须是数组")
        return layers
    for i, item in enumerate(raw_layers):
        loc = f"layers[{i}]"
        if not isinstance(item, dict):
            report.add(loc, ErrorType.BAD_TYPE, "图层条目必须是对象")
            continue
        ok = True
        lid = _require(item, "id", loc, report)
        source_id = _require(item, "source_id", loc, report)
        frame_in = _require(item, "frame_in", loc, report)
        frame_out = _require(item, "frame_out", loc, report)
        if not isinstance(lid, str) or not lid:
            report.add(f"{loc}.id", ErrorType.BAD_VALUE, "图层 id 必须是非空字符串")
            ok = False
        if source_id not in sources:
            report.add(f"{loc}.source_id", ErrorType.BAD_VALUE,
                       f"引用了不存在的素材: {source_id!r}")
            ok = False
        if not _is_int(frame_in) or not _is_int(frame_out):
            report.add(loc, ErrorType.BAD_TYPE, "frame_in/frame_out 必须是整数")
            ok = False
        elif frame_in < 0 or frame_out < 0 or frame_in > frame_count or frame_out > frame_count:
            report.add(loc, ErrorType.FRAME_OUT_OF_RANGE,
                       f"帧区间 [{frame_in}, {frame_out}) 越界, 合法范围 [0, {frame_count})")
            ok = False
        elif frame_in >= frame_out:
            report.add(loc, ErrorType.NEGATIVE_DURATION,
                       f"半开区间非法: frame_in={frame_in} >= frame_out={frame_out}")
            ok = False
        track = item.get("track", 0)
        if not _is_int(track) or track < 0:
            report.add(f"{loc}.track", ErrorType.BAD_VALUE, "track 必须是非负整数")
            ok = False
        x, y = item.get("x", 0), item.get("y", 0)
        if not _is_int(x) or not _is_int(y):
            report.add(loc, ErrorType.BAD_TYPE, "x/y 必须是整数像素坐标")
            ok = False
        scale = item.get("scale", 1.0)
        if not _is_number(scale) or scale <= 0:
            report.add(f"{loc}.scale", ErrorType.BAD_VALUE, "scale 必须是正数")
            ok = False
        rotation = item.get("rotation", 0.0)
        if not _is_number(rotation):
            report.add(f"{loc}.rotation", ErrorType.BAD_TYPE, "rotation 必须是数值")
            ok = False
        alpha = item.get("alpha", 1.0)
        if not _is_number(alpha) or not (0.0 <= alpha <= 1.0):
            report.add(f"{loc}.alpha", ErrorType.ALPHA_OUT_OF_RANGE,
                       f"alpha 必须落在 [0, 1], 实际为 {alpha!r}")
            ok = False
        blend = item.get("blend", BlendMode.SOURCE_OVER.value)
        if blend not in _BLEND_VALUES:
            report.add(f"{loc}.blend", ErrorType.UNKNOWN_BLEND, f"未知混合模式 {blend!r}")
            ok = False
        mask_id = item.get("mask_source_id")
        if mask_id is not None:
            if mask_id not in sources:
                report.add(f"{loc}.mask_source_id", ErrorType.BAD_VALUE,
                           f"遮罩素材不存在: {mask_id!r}")
                ok = False
            elif source_id in sources and sources[mask_id].kind != "mask":
                report.add(f"{loc}.mask_source_id", ErrorType.BAD_VALUE,
                           f"素材 {mask_id!r} 不是 mask 类型")
                ok = False
            elif source_id in sources:
                src, msk = sources[source_id], sources[mask_id]
                if (src.width, src.height) != (msk.width, msk.height):
                    report.add(loc, ErrorType.SIZE_MISMATCH,
                               f"遮罩尺寸 {msk.width}x{msk.height} 与素材 "
                               f"{src.width}x{src.height} 不匹配")
                    ok = False
        if ok:
            layers.append(Layer(
                layer_id=lid, source_id=source_id, frame_in=frame_in, frame_out=frame_out,
                track=track, order=i, x=x, y=y, scale=float(scale), rotation=float(rotation),
                alpha=float(alpha), blend=BlendMode(blend), mask_source_id=mask_id,
            ))
            report.valid_layers += 1
    return layers


def _parse_audio(raw_events, fps: int, sample_rate: int, frame_count: int,
                 report: ValidationReport) -> list[AudioEvent]:
    events: list[AudioEvent] = []
    if not isinstance(raw_events, list):
        report.add("audio_events", ErrorType.BAD_TYPE, "audio_events 必须是数组")
        return events
    timeline_end = Fraction(frame_count, fps)
    prev_start: Fraction | None = None
    for i, item in enumerate(raw_events):
        loc = f"audio_events[{i}]"
        if not isinstance(item, dict):
            report.add(loc, ErrorType.BAD_TYPE, "音频事件必须是对象")
            continue
        eid = _require(item, "id", loc, report)
        raw_start = _require(item, "start", loc, report)
        raw_dur = _require(item, "duration", loc, report)
        ok = True
        if not isinstance(eid, str) or not eid:
            report.add(f"{loc}.id", ErrorType.BAD_VALUE, "事件 id 必须是非空字符串")
            ok = False
        start = _parse_rational(raw_start, f"{loc}.start", report)
        duration = _parse_rational(raw_dur, f"{loc}.duration", report)
        if start is None or duration is None:
            ok = False
        else:
            if start < 0:
                report.add(f"{loc}.start", ErrorType.BAD_VALUE, "起始时间不得为负")
                ok = False
            if duration <= 0:
                report.add(f"{loc}.duration", ErrorType.NEGATIVE_DURATION,
                           "音频事件持续时间必须为正")
                ok = False
            if prev_start is not None and start < prev_start:
                report.add(f"{loc}.start", ErrorType.NON_MONOTONIC_TIME,
                           f"事件起始 {start}s 早于前一事件 {prev_start}s")
                ok = False
            if start is not None and duration is not None and start + duration > timeline_end:
                report.add(loc, ErrorType.FRAME_OUT_OF_RANGE,
                           f"事件结束时刻 {start + duration}s 超出时间线总长 {timeline_end}s")
                ok = False
        if ok:
            events.append(AudioEvent(eid, start, duration))
            report.valid_audio_events += 1
            prev_start = start
    return events


def parse_timeline(raw: dict, fps_override: int | None = None,
                   sample_rate_override: int | None = None
                   ) -> tuple[Timeline | None, ValidationReport]:
    """解析内存中的时间线 dict。返回 (模型或 None, 校验报告)。"""
    report = ValidationReport()
    if not isinstance(raw, dict):
        report.add("$", ErrorType.BAD_TYPE, "时间线根节点必须是 JSON 对象")
        return None, report

    canvas = raw.get("canvas")
    tl_cfg = raw.get("timeline")
    if not isinstance(canvas, dict):
        report.add("canvas", ErrorType.MISSING_FIELD, "缺少 canvas 对象")
        canvas = {}
    if not isinstance(tl_cfg, dict):
        report.add("timeline", ErrorType.MISSING_FIELD, "缺少 timeline 对象")
        tl_cfg = {}

    width = canvas.get("width")
    height = canvas.get("height")
    if not _is_int(width) or width <= 0:
        report.add("canvas.width", ErrorType.BAD_VALUE, "画布 width 必须是正整数")
    if not _is_int(height) or height <= 0:
        report.add("canvas.height", ErrorType.BAD_VALUE, "画布 height 必须是正整数")

    fps = fps_override if fps_override is not None else tl_cfg.get("fps")
    sample_rate = (sample_rate_override if sample_rate_override is not None
                   else tl_cfg.get("sample_rate"))
    if not _is_int(fps) or fps not in _ALLOWED_FPS:
        report.add("timeline.fps", ErrorType.BAD_VALUE, f"fps 仅支持 {_ALLOWED_FPS}")
    if not _is_int(sample_rate) or sample_rate <= 0:
        report.add("timeline.sample_rate", ErrorType.BAD_VALUE, "采样率必须是正整数")
    frame_count = tl_cfg.get("frame_count")
    if not _is_int(frame_count) or frame_count <= 0:
        report.add("timeline.frame_count", ErrorType.BAD_VALUE, "frame_count 必须是正整数")

    if report.errors:
        # 顶层参数缺失时无法安全解析条目, 仍尝试统计素材条目数。
        _parse_sources(raw.get("sources", []), report)
        return None, report

    sources = _parse_sources(raw.get("sources", []), report)
    layers = _parse_layers(raw.get("layers", []), sources, frame_count, report)
    events = _parse_audio(raw.get("audio_events", []), fps, sample_rate,
                          frame_count, report)
    if report.errors:
        return None, report
    return Timeline(width, height, fps, sample_rate, frame_count,
                    sources, layers, events), report


def load_timeline(path: str | Path, fps_override: int | None = None,
                  sample_rate_override: int | None = None
                  ) -> tuple[Timeline | None, ValidationReport]:
    """读取并解析时间线文件。JSON 损坏作为可定位错误返回, 不抛异常。"""
    report = ValidationReport()
    try:
        text = Path(path).read_text(encoding="utf-8")
    except OSError as exc:
        report.add(str(path), ErrorType.JSON_CORRUPT, f"文件无法读取: {exc}")
        return None, report
    try:
        raw = json.loads(text)
    except json.JSONDecodeError as exc:
        report.add(str(path), ErrorType.JSON_CORRUPT,
                   f"JSON 损坏: 第 {exc.lineno} 行第 {exc.colno} 列: {exc.msg}")
        return None, report
    return parse_timeline(raw, fps_override, sample_rate_override)
