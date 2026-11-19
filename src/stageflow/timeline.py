"""Timeline manifest parsing and validation.

The manifest is JSON, all times in integer master ticks (see clock.py):

{
  "job": "show-001",
  "delivery": {
    "sample_rate": 48000, "channels": 2, "fps": 30,
    "width": 320, "height": 180,
    "start_tick": 0, "duration_ticks": 7056000,
    "tracks": ["cam_a", "cam_b"]
  },
  "tracks": [
    {"id": "cam_a", "path": "cam_a.wav", "offset_ticks": 0,
     "segments": [{"start_tick": 0, "duration_ticks": 7056000}]}
  ]
}

A track's segments describe where its file audio lands on the shared
timeline; the file is consumed sequentially across its segments.  Segment
ranges may overlap between tracks (that is the multicam overlap interval).

Validation rejects (exit code 2 via ManifestError):
  negative durations, unknown tracks in the mix order, out-of-range
  segments, non-monotonic cut points, and values beyond hard limits.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path

from .clock import MASTER_RATE
from .errors import ManifestError

# Hard limits — anything beyond is rejected up front with a LIMIT_* code.
MAX_SAMPLE_RATE = 192_000
MIN_SAMPLE_RATE = 8_000
MAX_CHANNELS = 8
MAX_FPS = 120
MAX_DURATION_TICKS = MASTER_RATE * 60 * 60 * 4  # 4 hours
MAX_FILE_BYTES = 8 * 1024**3  # 8 GiB per input file
MAX_TRACKS = 32


@dataclass(frozen=True)
class Segment:
    start_tick: int
    duration_ticks: int

    @property
    def end_tick(self) -> int:
        return self.start_tick + self.duration_ticks


@dataclass(frozen=True)
class Track:
    id: str
    path: str  # relative to manifest directory
    offset_ticks: int  # where file sample 0 lands on the timeline
    segments: tuple[Segment, ...]
    cut_ticks: tuple[int, ...] = ()


@dataclass(frozen=True)
class Delivery:
    sample_rate: int
    channels: int
    fps: int
    width: int
    height: int
    start_tick: int
    duration_ticks: int
    mix_order: tuple[str, ...]

    @property
    def end_tick(self) -> int:
        return self.start_tick + self.duration_ticks


@dataclass(frozen=True)
class Timeline:
    job: str
    base_dir: Path
    delivery: Delivery
    tracks: tuple[Track, ...]
    rounding_policy: str = "round-half-even"
    track_map: dict[str, Track] = field(default_factory=dict)

    def resolve(self, track: Track) -> Path:
        return self.base_dir / track.path


def _err(code: str, msg: str, **kw: object) -> ManifestError:
    return ManifestError(code, msg, **kw)


def _require_int(value: object, name: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise _err("MANIFEST_TYPE", f"{name} must be an integer, got {value!r}")
    return value


def _parse_track(raw: object, index: int) -> Track:
    if not isinstance(raw, dict):
        raise _err("MANIFEST_TYPE", f"tracks[{index}] must be an object")
    tid = raw.get("id")
    if not isinstance(tid, str) or not tid:
        raise _err("MANIFEST_TYPE", f"tracks[{index}].id must be a non-empty string")
    path = raw.get("path")
    if not isinstance(path, str) or not path:
        raise _err("MANIFEST_TYPE", f"track {tid}: path must be a non-empty string", track=tid)
    if Path(path).is_absolute():
        raise _err("MANIFEST_PATH", f"track {tid}: path must be relative", track=tid)
    offset = _require_int(raw.get("offset_ticks", 0), f"track {tid} offset_ticks")
    segments: list[Segment] = []
    prev_end: int | None = None
    for si, s in enumerate(raw.get("segments", [])):
        if not isinstance(s, dict):
            raise _err("MANIFEST_TYPE", f"track {tid} segment {si} must be an object", track=tid)
        start = _require_int(s.get("start_tick"), f"track {tid} segment {si} start_tick")
        dur = _require_int(s.get("duration_ticks"), f"track {tid} segment {si} duration_ticks")
        if dur < 0:
            raise _err(
                "MANIFEST_NEGATIVE_DURATION",
                f"track {tid} segment {si}: negative duration {dur}",
                track=tid,
                sample_offset=start,
            )
        if start < 0:
            raise _err("MANIFEST_RANGE", f"track {tid} segment {si}: negative start", track=tid)
        if prev_end is not None and start < prev_end:
            raise _err(
                "MANIFEST_NONMONOTONIC",
                f"track {tid}: segment {si} starts before previous segment ends",
                track=tid,
                sample_offset=start,
            )
        prev_end = start + dur
        segments.append(Segment(start, dur))
    cuts = tuple(_require_int(c, f"track {tid} cut") for c in raw.get("cut_ticks", []))
    for a, b in zip(cuts, cuts[1:]):
        if b <= a:
            raise _err(
                "MANIFEST_NONMONOTONIC",
                f"track {tid}: cut points not strictly increasing ({a} >= {b})",
                track=tid,
                sample_offset=b,
            )
    return Track(tid, path, offset, tuple(segments), cuts)


def _parse_delivery(raw: object) -> Delivery:
    if not isinstance(raw, dict):
        raise _err("MANIFEST_TYPE", "delivery must be an object")
    rate = _require_int(raw.get("sample_rate"), "delivery.sample_rate")
    channels = _require_int(raw.get("channels"), "delivery.channels")
    fps = _require_int(raw.get("fps"), "delivery.fps")
    width = _require_int(raw.get("width"), "delivery.width")
    height = _require_int(raw.get("height"), "delivery.height")
    start = _require_int(raw.get("start_tick", 0), "delivery.start_tick")
    dur = _require_int(raw.get("duration_ticks"), "delivery.duration_ticks")
    if not (MIN_SAMPLE_RATE <= rate <= MAX_SAMPLE_RATE):
        raise _err("LIMIT_SAMPLE_RATE", f"delivery sample_rate {rate} outside [{MIN_SAMPLE_RATE},{MAX_SAMPLE_RATE}]")
    if not (1 <= channels <= MAX_CHANNELS):
        raise _err("LIMIT_CHANNELS", f"delivery channels {channels} outside [1,{MAX_CHANNELS}]")
    if not (1 <= fps <= MAX_FPS):
        raise _err("LIMIT_FPS", f"delivery fps {fps} outside [1,{MAX_FPS}]")
    if width <= 0 or height <= 0 or width % 2 or height % 2:
        raise _err("MANIFEST_RANGE", f"delivery size {width}x{height} must be positive and even")
    if dur < 0:
        raise _err("MANIFEST_NEGATIVE_DURATION", f"delivery duration_ticks {dur} < 0", sample_offset=start)
    if start < 0:
        raise _err("MANIFEST_RANGE", f"delivery start_tick {start} < 0")
    if start + dur > MAX_DURATION_TICKS:
        raise _err("LIMIT_DURATION", f"delivery end exceeds {MAX_DURATION_TICKS} ticks (4h)")
    mix = raw.get("tracks", [])
    if not isinstance(mix, list) or not all(isinstance(t, str) for t in mix):
        raise _err("MANIFEST_TYPE", "delivery.tracks must be a list of track ids")
    return Delivery(rate, channels, fps, width, height, start, dur, tuple(mix))


def load_timeline(manifest_path: str | Path) -> Timeline:
    p = Path(manifest_path)
    try:
        raw = json.loads(p.read_text(encoding="utf-8"))
    except FileNotFoundError:
        raise _err("MANIFEST_NOT_FOUND", f"manifest not found: {p.name}", path=p.name)
    except json.JSONDecodeError as e:
        raise _err("MANIFEST_JSON", f"manifest is not valid JSON: {e}", path=p.name)
    if not isinstance(raw, dict):
        raise _err("MANIFEST_TYPE", "manifest root must be an object")
    if "master_tick_rate" in raw and raw["master_tick_rate"] != MASTER_RATE:
        raise _err(
            "MANIFEST_TIMEBASE",
            f"master_tick_rate {raw['master_tick_rate']} != pipeline time base {MASTER_RATE}",
        )
    delivery = _parse_delivery(raw.get("delivery"))
    raw_tracks = raw.get("tracks")
    if not isinstance(raw_tracks, list):
        raise _err("MANIFEST_TYPE", "tracks must be a list")
    if len(raw_tracks) > MAX_TRACKS:
        raise _err("LIMIT_TRACKS", f"{len(raw_tracks)} tracks exceeds limit {MAX_TRACKS}")
    tracks = tuple(_parse_track(t, i) for i, t in enumerate(raw_tracks))
    ids = [t.id for t in tracks]
    if len(set(ids)) != len(ids):
        raise _err("MANIFEST_TYPE", "duplicate track ids")
    track_map = {t.id: t for t in tracks}
    for tid in delivery.mix_order:
        if tid not in track_map:
            raise _err("MANIFEST_UNKNOWN_TRACK", f"delivery references unknown track '{tid}'", track=tid)
    for t in tracks:
        for s in t.segments:
            if s.start_tick < t.offset_ticks:
                raise _err(
                    "MANIFEST_RANGE",
                    f"track {t.id} segment starts at {s.start_tick}, before the track "
                    f"offset {t.offset_ticks} (negative file position)",
                    track=t.id,
                    sample_offset=s.start_tick,
                )
            if s.start_tick < delivery.start_tick or s.end_tick > delivery.end_tick:
                raise _err(
                    "MANIFEST_RANGE",
                    f"track {t.id} segment [{s.start_tick},{s.end_tick}) outside delivery "
                    f"[{delivery.start_tick},{delivery.end_tick})",
                    track=t.id,
                    sample_offset=s.start_tick,
                )
    job = raw.get("job", p.stem)
    if not isinstance(job, str):
        raise _err("MANIFEST_TYPE", "job must be a string")
    return Timeline(job, p.parent, delivery, tracks, track_map=track_map)
