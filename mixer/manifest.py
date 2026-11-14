"""Declarative project manifest loading and validation."""

from __future__ import annotations

from dataclasses import dataclass
import json
import math
import os
from typing import Any

import soundfile as sf

from .automation import AutomationCurve, _db, _finite
from .errors import ManifestError, MixerIOError
from .graph import Node, build_graph, parse_nodes
from .manifest_nodes import canonical_nodes, validate_node_params


@dataclass
class Track:
    id: str
    file: str
    path: str
    file_rate: int
    samples_in: int
    channels: int
    gain_db: float
    pan: float
    automation: AutomationCurve | None
    meter_node: str | None
    resampled_frames: int

    def as_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "file": self.file,
            "path": self.path,
            "file_rate": self.file_rate,
            "samples_in": self.samples_in,
            "channels": self.channels,
            "gain_db": self.gain_db,
            "pan": self.pan,
            "meter_node": self.meter_node,
            "resampled_frames": self.resampled_frames,
            "automation": bool(self.automation),
        }


@dataclass
class Project:
    manifest_path: str
    sample_rate: int
    channels: int
    duration_s: float
    duration_frames: int
    tracks: list[Track]
    buses: list[dict[str, Any]]
    nodes: dict[str, Node]
    output_node: str
    warnings: list[dict[str, Any]]

    def track(self, track_id: str) -> Track:
        for track in self.tracks:
            if track.id == track_id:
                return track
        raise ManifestError(f"unknown track id {track_id!r}", details={"track_id": track_id})

    def graph(self) -> Any:
        return build_graph(self.nodes, self.output_node)


def _round_ratio(value: int, numerator: int, denominator: int) -> int:
    return (value * numerator + denominator // 2) // denominator


def _read_info(path: str, track_id: str) -> tuple[int, int, int]:
    try:
        info = sf.info(path)
    except Exception as exc:
        raise MixerIOError(
            f"cannot inspect WAV file for track {track_id!r}: {exc}",
            details={"track_id": track_id, "path": path},
        ) from exc
    if info.samplerate <= 0:
        raise ManifestError(
            f"track {track_id!r} has sample rate 0",
            details={"track_id": track_id, "path": path},
        )
    return int(info.frames), int(info.samplerate), int(info.channels)


def _load_json(path: str) -> dict[str, Any]:
    try:
        with open(path, "r", encoding="utf-8") as handle:
            raw = json.load(handle)
    except FileNotFoundError as exc:
        raise ManifestError("manifest file does not exist", details={"path": path}) from exc
    except (OSError, UnicodeError) as exc:
        raise MixerIOError(f"cannot read manifest: {exc}", details={"path": path}) from exc
    except json.JSONDecodeError as exc:
        raise ManifestError(
            f"invalid JSON: {exc.msg}", details={"path": path, "line": exc.lineno, "column": exc.colno}
        ) from exc
    if not isinstance(raw, dict):
        raise ManifestError("manifest root must be an object", details={"path": path})
    return raw


def _nonempty_string(value: Any, field: str) -> str:
    if not isinstance(value, str) or not value.strip():
        raise ManifestError(f"{field} must be a non-empty string", details={"field": field})
    return value


def load_project(path: str) -> Project:
    manifest_path = os.path.abspath(path)
    raw = _load_json(manifest_path)
    base_dir = os.path.dirname(manifest_path)

    version = raw.get("version", 1)
    if isinstance(version, bool) or version != 1:
        raise ManifestError("unsupported manifest version", details={"version": version})

    sample_rate = raw.get("sample_rate")
    if isinstance(sample_rate, bool) or not isinstance(sample_rate, int) or sample_rate <= 0:
        raise ManifestError(
            "sample_rate must be a positive integer",
            details={"field": "sample_rate", "value": sample_rate},
        )
    channels = raw.get("channels", 2)
    if isinstance(channels, bool) or not isinstance(channels, int) or channels not in (1, 2):
        raise ManifestError("channels must be 1 or 2", details={"field": "channels", "value": channels})

    raw_tracks = raw.get("tracks")
    if not isinstance(raw_tracks, list) or not raw_tracks:
        raise ManifestError("tracks must be a non-empty list", details={"field": "tracks"})
    track_ids: set[str] = set()
    provisional: list[dict[str, Any]] = []
    for index, item in enumerate(raw_tracks):
        field = f"tracks[{index}]"
        if not isinstance(item, dict):
            raise ManifestError(f"{field} must be an object")
        track_id = _nonempty_string(item.get("id"), f"{field}.id")
        if track_id in track_ids:
            raise ManifestError(f"duplicate track id {track_id!r}", details={"track_id": track_id})
        track_ids.add(track_id)
        file_name = _nonempty_string(item.get("file"), f"{field}.file")
        file_path = os.path.normpath(os.path.join(base_dir, file_name))
        if not os.path.isfile(file_path):
            raise ManifestError(
                f"missing audio file for track {track_id!r}",
                details={"track_id": track_id, "path": file_path},
            )
        gain_db = _db(item.get("gain_db", 0.0), f"{field}.gain_db")
        pan = _finite(item.get("pan", 0.0), f"{field}.pan")
        if not -1.0 <= pan <= 1.0:
            raise ManifestError(f"{field}.pan must be in [-1, 1]", details={"field": f"{field}.pan"})
        meter_node = item.get("meter_node")
        if meter_node is not None:
            meter_node = _nonempty_string(meter_node, f"{field}.meter_node")
        frames, file_rate, file_channels = _read_info(file_path, track_id)
        provisional.append(
            {
                "item": item,
                "id": track_id,
                "file": file_name,
                "path": file_path,
                "frames": frames,
                "rate": file_rate,
                "channels": file_channels,
                "gain_db": gain_db,
                "pan": pan,
                "meter_node": meter_node,
            }
        )

    duration_raw = raw.get("duration_s")
    if duration_raw is None:
        longest = max(_round_ratio(p["frames"], sample_rate, p["rate"]) for p in provisional)
        if longest <= 0:
            raise ManifestError("duration_s is required when every track is empty")
        duration_frames = longest
        duration_s = longest / sample_rate
    else:
        duration_s = _finite(duration_raw, "duration_s")
        if duration_s <= 0.0:
            raise ManifestError("duration_s must be positive", details={"duration_s": duration_s})
        duration_frames = int(math.floor(duration_s * sample_rate + 0.5))
        if duration_frames <= 0:
            raise ManifestError("duration_s rounds to zero samples", details={"duration_s": duration_s})

    tracks: list[Track] = []
    warnings: list[dict[str, Any]] = []
    for data in provisional:
        automation = AutomationCurve.parse(data["item"].get("automation"), duration_s)
        resampled_frames = _round_ratio(data["frames"], sample_rate, data["rate"])
        tracks.append(
            Track(
                id=data["id"],
                file=data["file"],
                path=data["path"],
                file_rate=data["rate"],
                samples_in=data["frames"],
                channels=data["channels"],
                gain_db=data["gain_db"],
                pan=data["pan"],
                automation=automation,
                meter_node=data["meter_node"],
                resampled_frames=resampled_frames,
            )
        )
        if resampled_frames < duration_frames:
            warnings.append(
                {
                    "track_id": data["id"],
                    "kind": "short_tail_padded",
                    "samples": duration_frames - resampled_frames,
                }
            )
        elif resampled_frames > duration_frames:
            warnings.append(
                {
                    "track_id": data["id"],
                    "kind": "long_track_truncated",
                    "samples": resampled_frames - duration_frames,
                }
            )

    raw_buses = raw.get("buses", [{"id": "mix", "gain_db": 0.0}])
    if not isinstance(raw_buses, list) or not raw_buses:
        raise ManifestError("buses must be a non-empty list", details={"field": "buses"})
    buses: list[dict[str, Any]] = []
    bus_ids: set[str] = set()
    for index, item in enumerate(raw_buses):
        if not isinstance(item, dict):
            raise ManifestError(f"buses[{index}] must be an object")
        bus_id = _nonempty_string(item.get("id", "mix"), f"buses[{index}].id")
        if bus_id in bus_ids:
            raise ManifestError(f"duplicate bus id {bus_id!r}", details={"bus_id": bus_id})
        bus_ids.add(bus_id)
        ceiling = _finite(item.get("ceiling", 1.0), f"buses[{index}].ceiling")
        softness = _finite(item.get("softness", 1.0), f"buses[{index}].softness")
        if not 0.0 < ceiling <= 1.0:
            raise ManifestError(
                f"buses[{index}].ceiling must be in (0, 1]",
                details={"field": f"buses[{index}].ceiling", "value": ceiling},
            )
        if softness < 1.0:
            raise ManifestError(
                f"buses[{index}].softness must be >= 1",
                details={"field": f"buses[{index}].softness", "value": softness},
            )
        buses.append(
            {
                "id": bus_id,
                "gain_db": _db(item.get("gain_db", 0.0), f"buses[{index}].gain_db"),
                "ceiling": ceiling,
                "softness": softness,
            }
        )

    if "nodes" in raw:
        nodes = parse_nodes(raw["nodes"])
        output_node = _nonempty_string(raw.get("output_node"), "output_node")
        validate_node_params(nodes)
        for node in nodes.values():
            if node.track is not None and node.track not in track_ids:
                raise ManifestError(
                    f"node {node.id!r} references unknown track {node.track!r}",
                    details={"node_id": node.id, "track_id": node.track},
                )
        for track in tracks:
            if track.meter_node is not None and track.meter_node not in nodes:
                raise ManifestError(
                    f"track {track.id!r} references unknown meter node {track.meter_node!r}",
                    details={"track_id": track.id, "meter_node": track.meter_node},
                )
    else:
        if len(buses) > 1:
            warnings.append({"kind": "implicit_bus_selected", "bus_id": buses[0]["id"]})
        nodes, output_node = canonical_nodes(tracks, buses, duration_frames)

    return Project(
        manifest_path=manifest_path,
        sample_rate=sample_rate,
        channels=channels,
        duration_s=duration_s,
        duration_frames=duration_frames,
        tracks=tracks,
        buses=buses,
        nodes=nodes,
        output_node=output_node,
        warnings=warnings,
    )
