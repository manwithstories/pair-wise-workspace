"""工程 JSON 的解析与校验。

工程示例见 fixtures 生成的 demo.mix.json。所有校验失败都抛出
ProjectError，消息携带形如 ``tracks[2].automation[0].time`` 的字段路径。
"""

from __future__ import annotations

import json
import math
from dataclasses import dataclass, field
from pathlib import Path

from .errors import ProjectError


@dataclass
class Keyframe:
    time: float
    db: float


@dataclass
class Track:
    name: str
    file: str
    channels: int
    bus: str
    gain_db: float
    muted: bool
    solo: bool
    automation: list[Keyframe] = field(default_factory=list)


@dataclass
class Bus:
    name: str
    output: str | None


@dataclass
class Project:
    target_sr: int
    duration: float
    hop: int
    master_bus: str
    buses: list[Bus]
    tracks: list[Track]
    clip_threshold: float = 1.0
    source_path: Path | None = None

    def base_dir(self) -> Path:
        if self.source_path is not None:
            return self.source_path.parent
        return Path.cwd()

    def bus_names(self) -> set[str]:
        return {bus.name for bus in self.buses}

    def track_names(self) -> list[str]:
        return [track.name for track in self.tracks]


def _is_number(value: object) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool)


def _require_type(obj: object, key: str, path: str, kinds: tuple) -> object:
    if not isinstance(obj.get(key), kinds) or (
        isinstance(obj.get(key), bool) and bool not in kinds
    ):
        kind_names = "/".join(getattr(kind, "__name__", str(kind)) for kind in kinds)
        raise ProjectError(f"{path}.{key}: 字段缺失或类型错误，应为 {kind_names}")
    return obj[key]


def _parse_automation(raw_auto: object, path: str) -> list[Keyframe]:
    if not isinstance(raw_auto, list):
        raise ProjectError(f"{path}.automation: 应为列表")
    if len(raw_auto) == 0:
        raise ProjectError(f"{path}.automation: 自动化曲线不能为空")
    frames: list[Keyframe] = []
    for index, raw in enumerate(raw_auto):
        sub = f"{path}.automation[{index}]"
        if not isinstance(raw, dict):
            raise ProjectError(f"{sub}: 关键帧应为对象")
        time_value = _require_type(raw, "time", sub, (int, float))
        db_value = _require_type(raw, "db", sub, (int, float))
        if time_value < 0:
            raise ProjectError(f"{sub}.time: 关键帧时间不能为负，得到 {time_value}")
        if not math.isfinite(db_value):
            raise ProjectError(f"{sub}.db: 拒绝 NaN 或 Inf，得到 {db_value}")
        frames.append(Keyframe(float(time_value), float(db_value)))
    frames.sort(key=lambda item: item.time)
    # 同一时间的重复关键帧以后者为准（保留 JSON 顺序中的最后一个）。
    deduped: dict[float, Keyframe] = {}
    for frame in frames:
        deduped[frame.time] = frame
    return list(deduped.values())


def _parse_track(raw: dict, index: int, bus_names: set[str]) -> Track:
    path = f"tracks[{index}]"
    name = _require_type(raw, "name", path, (str,))
    file_value = _require_type(raw, "file", path, (str,))
    channels = _require_type(raw, "channels", path, (int,))
    if channels <= 0:
        raise ProjectError(f"{path}.channels: 声道数必须为正整数，得到 {channels}")
    bus_name = _require_type(raw, "bus", path, (str,))
    if bus_name not in bus_names:
        raise ProjectError(f"{path}.bus: 未知总线 {bus_name!r}")
    gain_db = _require_type(raw, "gain_db", path, (int, float))
    if not math.isfinite(gain_db):
        raise ProjectError(f"{path}.gain_db: 拒绝 NaN 或 Inf")
    muted = bool(raw.get("muted", False))
    solo = bool(raw.get("solo", False))
    automation = _parse_automation(raw["automation"], path) if "automation" in raw else []
    return Track(
        name=str(name),
        file=str(file_value),
        channels=int(channels),
        bus=str(bus_name),
        gain_db=float(gain_db),
        muted=muted,
        solo=solo,
        automation=automation,
    )


def _parse_bus(raw: dict, index: int) -> Bus:
    path = f"buses[{index}]"
    name = _require_type(raw, "name", path, (str,))
    output = raw.get("output")
    if output is not None and not isinstance(output, str):
        raise ProjectError(f"{path}.output: 应为字符串或 null")
    return Bus(name=str(name), output=None if output is None else str(output))


def parse_project(raw: dict, source_path: Path | None = None) -> Project:
    if not isinstance(raw, dict):
        raise ProjectError("工程根节点必须是 JSON 对象")
    target_sr = _require_type(raw, "target_sr", "$", (int,))
    if target_sr <= 0:
        raise ProjectError(f"$.target_sr: 采样率必须为正数，得到 {target_sr}")
    duration = _require_type(raw, "duration", "$", (int, float))
    if duration <= 0:
        raise ProjectError(f"$.duration: 时长必须为正数，得到 {duration}")
    hop = int(raw.get("hop", 64))
    if hop <= 0:
        raise ProjectError(f"$.hop: hop 必须为正整数，得到 {hop}")
    master_bus = _require_type(raw, "master_bus", "$", (str,))
    clip_threshold = float(raw.get("clip_threshold", 1.0))
    if not math.isfinite(clip_threshold) or clip_threshold <= 0:
        raise ProjectError("$.clip_threshold: 必须为正的有限数值")

    raw_buses = _require_type(raw, "buses", "$", (list,))
    if len(raw_buses) == 0:
        raise ProjectError("$.buses: 至少需要一个总线")
    buses = [_parse_bus(item, idx) for idx, item in enumerate(raw_buses)]
    names = [bus.name for bus in buses]
    seen: set[str] = set()
    for idx, name in enumerate(names):
        if name in seen:
            raise ProjectError(f"buses[{idx}].name: 重复总线名 {name!r}")
        seen.add(name)
    bus_names = set(names)
    if master_bus not in bus_names:
        raise ProjectError(f"$.master_bus: 未知总线 {master_bus!r}")
    for idx, bus in enumerate(buses):
        if bus.output is not None and bus.output not in bus_names:
            raise ProjectError(f"buses[{idx}].output: 未知总线 {bus.output!r}")

    raw_tracks = _require_type(raw, "tracks", "$", (list,))
    if len(raw_tracks) == 0:
        raise ProjectError("$.tracks: 至少需要一条轨道")
    track_names: set[str] = set()
    tracks: list[Track] = []
    for idx, item in enumerate(raw_tracks):
        if not isinstance(item, dict):
            raise ProjectError(f"tracks[{idx}]: 轨道必须为对象")
        track = _parse_track(item, idx, bus_names)
        if track.name in track_names:
            raise ProjectError(f"tracks[{idx}].name: 重复轨道名 {track.name!r}")
        track_names.add(track.name)
        tracks.append(track)

    return Project(
        target_sr=int(target_sr),
        duration=float(duration),
        hop=hop,
        master_bus=str(master_bus),
        buses=buses,
        tracks=tracks,
        clip_threshold=clip_threshold,
        source_path=source_path,
    )


def load_project(path: str | Path) -> Project:
    project_path = Path(path)
    if not project_path.is_file():
        raise ProjectError(f"工程文件不存在: {project_path}")
    try:
        with project_path.open("r", encoding="utf-8") as handle:
            raw = json.load(handle)
    except json.JSONDecodeError as exc:
        raise ProjectError(f"工程 JSON 解析失败: {exc}") from exc
    return parse_project(raw, source_path=project_path.resolve())
