"""Core data model: layouts, clips, tracks, automation points and track stats."""

from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import Any, Iterable, Mapping, Sequence

from .errors import MixerError, EXIT_FORMAT, argument_error

PROJECT_SCHEMA = "mix-project/1"
TARGET_RATE = 48000
SUPPORTED_RATES = (44100, 48000, 96000)

# Channel layouts the mixer accepts, with their native channel count.
_LAYOUT_CHANNELS: dict[str, int] = {
    "mono": 1,
    "stereo": 2,
    "quad": 4,
}

# Built-in fold-down matrices to the stereo master. Rows are [left, right],
# columns follow the source channel order (quad = FL, FR, BL, BR).
_LAYOUT_MATRIX: dict[str, tuple[tuple[float, float], ...]] = {
    "mono": ((0.70710678, 0.70710678),),
    "stereo": ((1.0, 0.0), (0.0, 1.0)),
    "quad": ((1.0, 0.0), (0.0, 1.0), (0.5, 0.0), (0.0, 0.5)),
}


def db_to_gain(db: float) -> float:
    return float(10.0 ** (db / 20.0))


def gain_to_db(gain: float) -> float:
    if gain <= 0.0:
        return -math.inf
    return float(20.0 * math.log10(gain))


@dataclass(frozen=True)
class ChannelLayout:
    """A named source layout plus its fold-down matrix to stereo."""

    name: str
    channels: int
    matrix: tuple[tuple[float, float], ...]

    @classmethod
    def parse(cls, name: str) -> "ChannelLayout":
        key = name.strip().lower()
        if key not in _LAYOUT_CHANNELS:
            raise MixerError(
                "unknown_channel_layout",
                f"unknown channel layout {name!r}; expected one of "
                f"{', '.join(sorted(_LAYOUT_CHANNELS))}",
                EXIT_FORMAT,
                details={"layout": name, "known": sorted(_LAYOUT_CHANNELS)},
            )
        return cls(key, _LAYOUT_CHANNELS[key], _LAYOUT_MATRIX[key])

    def matrix_for(self, override: Sequence[Sequence[float]] | None) -> tuple[tuple[float, float], ...]:
        """Validate a caller-supplied stereo matrix, else use the layout default."""
        if override is None:
            return self.matrix
        rows = tuple(tuple(float(v) for v in row) for row in override)
        if len(rows) != self.channels or any(len(row) != 2 for row in rows):
            raise MixerError(
                "invalid_channel_matrix",
                f"channel matrix must be {self.channels}x2 for layout {self.name!r}",
                EXIT_FORMAT,
                details={"layout": self.name, "channels": self.channels},
            )
        if any(not math.isfinite(v) for row in rows for v in row):
            raise MixerError(
                "invalid_channel_matrix",
                f"channel matrix for layout {self.name!r} contains non-finite values",
                EXIT_FORMAT,
            )
        return rows


@dataclass(frozen=True)
class TimeRange:
    start: float
    end: float

    def contains(self, t: float) -> bool:
        return self.start <= t < self.end


@dataclass(frozen=True)
class AutomationPoint:
    """One automation breakpoint, gain expressed in dB."""

    time: float
    gain_db: float


@dataclass
class Clip:
    """A source segment placed on a track timeline.

    ``source_offset_frames`` and ``length_frames`` are counted in the source
    file's own sample rate; ``start_time`` is offset from the track start.
    """

    path: str
    start_time: float = 0.0
    source_offset_frames: int = 0
    length_frames: int | None = None


@dataclass
class Track:
    """One mixer track.

    ``start_time`` delays the track's audio on the master timeline.
    ``automation`` times and ``silences`` windows are both expressed in
    master-timeline seconds, so they line up with what is heard.
    """

    name: str
    file: str
    layout: ChannelLayout
    start_time: float = 0.0
    gain_db: float = 0.0
    clips: list[Clip] = field(default_factory=list)
    silences: list[TimeRange] = field(default_factory=list)
    automation: list[AutomationPoint] = field(default_factory=list)
    channel_matrix: tuple[tuple[float, float], ...] | None = None
    max_slew_db_per_s: float | None = None

    @property
    def sources(self) -> list[Clip]:
        return self.clips or [Clip(self.file)]

    def is_silent(self, t: float) -> bool:
        return any(window.contains(t) for window in self.silences)


@dataclass
class TrackStats:
    """Per-track measurements produced alongside the master output."""

    name: str
    frames: int = 0
    peak_in_dbfs: float = -math.inf
    peak_out_dbfs: float = -math.inf
    rms_dbfs: float = -math.inf
    clip_count: int = 0
    max_gain_delta_db: float = 0.0

    def to_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "frames": self.frames,
            "peak_in_dbfs": round(self.peak_in_dbfs, 4),
            "peak_out_dbfs": round(self.peak_out_dbfs, 4),
            "rms_dbfs": round(self.rms_dbfs, 4),
            "clip_count": self.clip_count,
            "max_gain_delta_db": round(self.max_gain_delta_db, 6),
        }


@dataclass
class BusSpec:
    """A group bus: member track names plus the bus gain in dB."""

    name: str
    tracks: list[str]
    gain_db: float = 0.0


@dataclass
class Project:
    target_rate: int
    tracks: list[Track]
    buses: list[BusSpec] = field(default_factory=list)
    block_size: int = 1024

    def track_names(self) -> list[str]:
        return [t.name for t in self.tracks]

    def bus_map(self) -> dict[str, list[str]]:
        return {bus.name: list(bus.tracks) for bus in self.buses}

    def bus_gains(self) -> dict[str, float]:
        return {bus.name: bus.gain_db for bus in self.buses}


def _as_float(raw: Any, what: str, track: str | None = None) -> float:
    try:
        value = float(raw)
    except (TypeError, ValueError):
        raise MixerError(
            "invalid_project_field",
            f"{what} must be a number, got {raw!r}",
            track=track,
        ) from None
    if not math.isfinite(value):
        raise MixerError("invalid_project_field", f"{what} must be finite", track=track)
    return value


def _as_int(raw: Any, what: str, track: str | None = None) -> int:
    value = _as_float(raw, what, track)
    if not value.is_integer():
        raise MixerError("invalid_project_field", f"{what} must be an integer", track=track)
    return int(value)


def parse_automation(raw: Iterable[Mapping[str, Any]], track: str) -> list[AutomationPoint]:
    """Parse automation breakpoints: sort by time, last duplicate wins."""
    points: list[AutomationPoint] = []
    for item in raw:
        time = _as_float(item.get("time"), "automation time", track)
        gain = _as_float(item.get("gain_db"), "automation gain_db", track)
        if time < 0.0:
            raise MixerError("invalid_automation", "automation time must be >= 0", track=track)
        points.append(AutomationPoint(time, gain))
    # Stable sort keeps declaration order among equal times; dict insertion
    # order then makes the LAST declared value win for duplicate timestamps.
    points.sort(key=lambda p: p.time)
    last_by_time: dict[float, AutomationPoint] = {}
    for point in points:
        last_by_time[point.time] = point
    return list(last_by_time.values())


def parse_track(raw: Mapping[str, Any]) -> Track:
    name = str(raw.get("name") or "").strip()
    if not name:
        raise MixerError("invalid_project_field", "every track needs a non-empty name")
    layout = ChannelLayout.parse(str(raw.get("layout", "stereo")))
    file_name = raw.get("file")
    if not isinstance(file_name, str) or not file_name.strip():
        raise MixerError("invalid_project_field", "track needs a source file", track=name)

    clips: list[Clip] = []
    for item in raw.get("clips", []) or []:
        clips.append(
            Clip(
                path=str(item.get("path", file_name)),
                start_time=_as_float(item.get("start_time", 0.0), "clip start_time", name),
                source_offset_frames=_as_int(
                    item.get("source_offset_frames", 0), "clip source_offset_frames", name
                ),
                length_frames=(
                    None
                    if item.get("length_frames") is None
                    else _as_int(item["length_frames"], "clip length_frames", name)
                ),
            )
        )

    silences = [
        TimeRange(
            _as_float(pair[0], "silence start", name),
            _as_float(pair[1], "silence end", name),
        )
        for pair in raw.get("silences", []) or []
    ]
    if any(window.end <= window.start for window in silences):
        raise MixerError("invalid_silence", "silence end must be greater than start", track=name)

    slew = raw.get("max_slew_db_per_s")
    return Track(
        name=name,
        file=file_name,
        layout=layout,
        start_time=_as_float(raw.get("start_time", 0.0), "track start_time", name),
        gain_db=_as_float(raw.get("gain_db", 0.0), "track gain_db", name),
        clips=clips,
        silences=silences,
        automation=parse_automation(raw.get("automation", []) or [], name),
        channel_matrix=layout.matrix_for(raw.get("channel_matrix")),
        max_slew_db_per_s=None if slew is None else _as_float(slew, "max_slew_db_per_s", name),
    )


def parse_project(raw: Mapping[str, Any], base_dir: Any = None, default_block: int = 1024) -> Project:
    from pathlib import Path

    if not isinstance(raw, Mapping):
        raise argument_error("invalid_project", "project JSON must be an object")
    schema = raw.get("schema", PROJECT_SCHEMA)
    if schema != PROJECT_SCHEMA:
        raise argument_error("invalid_project", f"unsupported project schema {schema!r}")
    rate = _as_int(raw.get("target_rate", TARGET_RATE), "target_rate")
    if rate != TARGET_RATE:
        raise argument_error(
            "invalid_target_rate",
            f"target_rate must be {TARGET_RATE}, got {rate}",
            details={"target_rate": rate},
        )
    tracks_raw = raw.get("tracks") or []
    if not isinstance(tracks_raw, list) or len(tracks_raw) < 4:
        raise argument_error("invalid_project", "project needs at least four tracks")

    base = Path(base_dir) if base_dir is not None else Path.cwd()
    tracks: list[Track] = []
    for item in tracks_raw:
        track = parse_track(item)
        resolved = (base / track.file).resolve()
        track.file = str(resolved)
        for clip in track.clips:
            clip.path = str((base / clip.path).resolve())
        tracks.append(track)

    names = [t.name for t in tracks]
    if len(set(names)) != len(names):
        raise argument_error("invalid_project", "track names must be unique")

    buses: list[BusSpec] = []
    for item in raw.get("buses", []) or []:
        name = str(item.get("name") or "").strip()
        members = item.get("tracks") or []
        if not name or not isinstance(members, list) or not members:
            raise argument_error("invalid_project", "each bus needs a name and tracks list")
        buses.append(
            BusSpec(
                name=name,
                tracks=[str(member) for member in members],
                gain_db=_as_float(item.get("gain_db", 0.0), "bus gain_db"),
            )
        )

    return Project(
        target_rate=rate,
        tracks=tracks,
        buses=buses,
        block_size=_as_int(raw.get("block_size", default_block), "block_size"),
    )
