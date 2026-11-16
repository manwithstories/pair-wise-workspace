"""project.json 解析与字段校验。

解析失败统一抛 ConfigError(退出码 2),不向调用方泄漏底层异常类型。
"""

from __future__ import annotations

import json
import math
from pathlib import Path
from typing import Any

from .types import (
    AutomationPoint,
    ConfigError,
    ErrorPolicy,
    ProjectConfig,
    TrackSpec,
)


def _require(mapping: dict[str, Any], key: str, context: str) -> Any:
    if key not in mapping or mapping[key] is None:
        raise ConfigError(f"{context}: 缺少必需字段 '{key}'")
    return mapping[key]


def _parse_automation(raw: Any, context: str) -> tuple[AutomationPoint, ...]:
    if raw is None:
        return ()
    if not isinstance(raw, list):
        raise ConfigError(f"{context}: automation 必须是点对象数组")
    points: list[AutomationPoint] = []
    for idx, item in enumerate(raw):
        if not isinstance(item, dict):
            raise ConfigError(f"{context}.automation[{idx}]: 必须是对象")
        sub = f"{context}.automation[{idx}]"
        time_v = _require(item, "time", sub)
        value_v = _require(item, "value", sub)
        if not isinstance(time_v, int) or isinstance(time_v, bool) or time_v < 0:
            raise ConfigError(f"{sub}: time 必须是非负整数采样点")
        if not isinstance(value_v, (int, float)) or isinstance(value_v, bool):
            raise ConfigError(f"{sub}: value 必须是数值")
        value_f = float(value_v)
        if math.isnan(value_f) or math.isinf(value_f):
            raise ConfigError(f"{sub}: value 不允许 NaN 或无穷大")
        points.append(AutomationPoint(time=time_v, value=value_f))
    return tuple(points)


def _parse_channels(raw: Any, context: str) -> tuple[int, int]:
    if raw is None:
        return (0, 1)
    if not isinstance(raw, list) or len(raw) != 2:
        raise ConfigError(f"{context}: channels 必须是两个声道索引的数组")
    if not all(isinstance(v, int) and not isinstance(v, bool) and v >= 0 for v in raw):
        raise ConfigError(f"{context}: channels 索引必须是非负整数")
    return (int(raw[0]), int(raw[1]))


def parse_track(raw: Any, index: int) -> TrackSpec:
    """解析配置中的单个 track 对象,缺字段即抛 ConfigError。"""

    if not isinstance(raw, dict):
        raise ConfigError(f"tracks[{index}]: 必须是对象")
    context = f"tracks[{index}]"
    name = str(_require(raw, "name", context))
    path = str(_require(raw, "path", context))
    offset = raw.get("offset", 0)
    if not isinstance(offset, int) or isinstance(offset, bool) or offset < 0:
        raise ConfigError(f"{context}: offset 必须是非负整数")
    gain = raw.get("gain", 1.0)
    if not isinstance(gain, (int, float)) or isinstance(gain, bool):
        raise ConfigError(f"{context}: gain 必须是数值")
    gain_f = float(gain)
    if math.isnan(gain_f) or math.isinf(gain_f):
        raise ConfigError(f"{context}: gain 不允许 NaN 或无穷大")
    return TrackSpec(
        name=name,
        path=path,
        offset=offset,
        channels=_parse_channels(raw.get("channels"), context),
        gain=gain_f,
        automation=_parse_automation(raw.get("automation"), context),
    )


def load_config(path: str | Path) -> ProjectConfig:
    """从磁盘读取并校验 project.json。"""

    p = Path(path)
    try:
        text = p.read_text(encoding="utf-8")
    except FileNotFoundError as exc:
        raise ConfigError(f"配置文件不存在: {p}") from exc
    except OSError as exc:
        raise ConfigError(f"配置文件无法读取: {p}: {exc}") from exc
    try:
        raw = json.loads(text)
    except json.JSONDecodeError as exc:
        raise ConfigError(f"配置文件不是合法 JSON: {p}: {exc}") from exc
    if not isinstance(raw, dict):
        raise ConfigError("配置根节点必须是对象")

    sample_rate = _require(raw, "sample_rate", "project")
    if not isinstance(sample_rate, int) or isinstance(sample_rate, bool) or sample_rate <= 0:
        raise ConfigError("sample_rate 必须是正整数")
    duration = _require(raw, "duration_frames", "project")
    if not isinstance(duration, int) or isinstance(duration, bool) or duration < 0:
        raise ConfigError("duration_frames 必须是非负整数")

    policy_raw = raw.get("error_policy", ErrorPolicy.FAIL_FAST.value)
    try:
        policy = ErrorPolicy(policy_raw)
    except ValueError as exc:
        raise ConfigError(
            f"error_policy 仅支持 {[p.value for p in ErrorPolicy]}"
        ) from exc

    clip = float(raw.get("clip_threshold", 1.0))
    if math.isnan(clip) or math.isinf(clip) or clip <= 0:
        raise ConfigError("clip_threshold 必须是正的有限数值")
    block_frames = raw.get("block_frames", 1024)
    if not isinstance(block_frames, int) or isinstance(block_frames, bool) or block_frames <= 0:
        raise ConfigError("block_frames 必须是正整数")

    raw_tracks = _require(raw, "tracks", "project")
    if not isinstance(raw_tracks, list) or not raw_tracks:
        raise ConfigError("tracks 必须是非空数组")
    tracks = tuple(parse_track(item, i) for i, item in enumerate(raw_tracks))
    names = [t.name for t in tracks]
    if len(set(names)) != len(names):
        raise ConfigError("tracks 中 name 必须唯一")

    return ProjectConfig(
        sample_rate=sample_rate,
        duration_frames=duration,
        tracks=tracks,
        error_policy=policy,
        clip_threshold=clip,
        block_frames=block_frames,
    )
