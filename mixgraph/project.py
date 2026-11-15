"""Manifest parsing, validation, and graph construction."""

from __future__ import annotations

from dataclasses import dataclass, field
import json
from pathlib import Path

import soundfile as sf
import yaml

from .automation import AutomationCurve, automation_from_manifest
from .errors import ManifestError
from .graph import Graph
from .util import require_finite


@dataclass
class Project:
    path: Path
    manifest: dict
    name: str
    sample_rate: int
    channels: int
    duration: float
    duration_frames: int
    block_size: int
    clip_threshold_db: float
    max_clip_samples: int
    graph: Graph
    tracks: list[dict] = field(default_factory=list)
    master_bus: str = ""
    output_node: str = ""


def _mapping(value, field_name: str) -> dict:
    if not isinstance(value, dict):
        raise ManifestError("must be a mapping", field_name)
    return value


def _positive_int(value, field_name: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value <= 0:
        raise ManifestError("must be a positive integer", field_name)
    return value


def _nonnegative_int(value, field_name: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        raise ManifestError("must be a non-negative integer", field_name)
    return value


def _bool(value, field_name: str) -> bool:
    if not isinstance(value, bool):
        raise ManifestError("must be true or false", field_name)
    return value


def _parse_document(path: Path) -> dict:
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as exc:
        raise ManifestError(str(exc), "project") from exc
    try:
        if path.suffix.lower() in {".yaml", ".yml"}:
            data = yaml.safe_load(text)
        else:
            data = json.loads(text)
    except (json.JSONDecodeError, yaml.YAMLError) as exc:
        raise ManifestError(f"cannot parse {path.name}: {exc}", "project") from exc
    return _mapping(data, "project")


def _derive_duration(manifest: dict) -> float:
    if "duration" in manifest:
        return require_finite("duration", manifest["duration"], field="duration")
    ends = []
    for index, node in enumerate(manifest.get("nodes", [])):
        if isinstance(node, dict) and node.get("type") == "source":
            event = _mapping(node.get("event", {}), f"nodes[{index}].event")
            ends.append(require_finite("end", event.get("end"), field=f"nodes[{index}].event.end"))
    if not ends:
        raise ManifestError("project duration is required", "duration")
    return max(ends)


def _enrich_source(node, root: Path, sample_rate: int, channels: int) -> None:
    params = node.params
    field = f"nodes.{node.id}"
    raw_path = params.get("path")
    if not isinstance(raw_path, str) or not raw_path:
        raise ManifestError("source path must be a non-empty string", f"{field}.path")
    path = (root / raw_path).resolve()
    if not path.is_file():
        raise ManifestError(f"source file not found: {path}", f"{field}.path")
    event = _mapping(params.get("event", {}), f"{field}.event")
    event_start = require_finite("start", event.get("start", 0.0), field=f"{field}.event.start")
    event_end = require_finite("end", event.get("end"), field=f"{field}.event.end")
    if event_start < 0.0 or event_end <= event_start:
        raise ManifestError("event must satisfy 0 <= start < end", f"{field}.event")
    try:
        info = sf.info(str(path))
    except (OSError, RuntimeError) as exc:
        raise ManifestError(str(exc), f"{field}.path") from exc
    declared_rate = params.get("sample_rate", info.samplerate)
    declared_channels = params.get("channels", info.channels)
    declared_rate = _positive_int(declared_rate, f"{field}.sample_rate")
    declared_channels = _positive_int(declared_channels, f"{field}.channels")
    if declared_rate != info.samplerate:
        raise ManifestError(
            f"declares {declared_rate} Hz but file is {info.samplerate} Hz",
            f"{field}.sample_rate",
        )
    if declared_channels != channels or info.channels != channels:
        raise ManifestError(
            f"file has {info.channels} channels, project expects {channels}",
            f"{field}.channels",
        )
    native_start = int(round(event_start * declared_rate))
    native_frames = int(round((event_end - event_start) * declared_rate))
    if native_start >= info.frames:
        raise ManifestError("event starts beyond end of source file", f"{field}.event.start")
    params.update(
        {
            "_path": str(path),
            "_source_rate": declared_rate,
            "_channels": declared_channels,
            "_file_offset": native_start,
            "_event_frames": max(1, native_frames),
            "_event_start": event_start,
            "_event_end": event_end,
        }
    )


def _enrich_gain(node) -> None:
    params = node.params
    field = f"nodes.{node.id}"
    params["gain_db"] = require_finite("gain_db", params.get("gain_db", 0.0), field=f"{field}.gain_db")
    params["_automation"] = automation_from_manifest(
        params.get("automation"), f"{field}.automation"
    )


def _enrich_resample(node, project_rate: int) -> None:
    field = f"nodes.{node.id}"
    rate = _positive_int(node.params.get("rate", project_rate), f"{field}.rate")
    if rate != project_rate:
        raise ManifestError(
            f"resample target must equal project sample rate {project_rate}",
            f"{field}.rate",
        )
    node.params["rate"] = rate


def _normalise_tracks(raw_tracks, graph: Graph) -> list[dict]:
    if not isinstance(raw_tracks, list) or not raw_tracks:
        raise ManifestError("must be a non-empty list", "tracks")
    tracks: list[dict] = []
    seen: set[str] = set()
    for index, raw in enumerate(raw_tracks):
        track = _mapping(raw, f"tracks[{index}]")
        track_id = track.get("id")
        if not isinstance(track_id, str) or not track_id:
            raise ManifestError("must be a non-empty string", f"tracks[{index}].id")
        if track_id in seen:
            raise ManifestError("duplicate track id", f"tracks[{index}].id")
        seen.add(track_id)
        refs = {}
        for key, expected_type in (
            ("source", "source"),
            ("gain", "gain"),
            ("bus", "bus"),
        ):
            node_id = track.get(key)
            if not isinstance(node_id, str) or node_id not in graph.nodes:
                raise ManifestError(f"unknown node {node_id!r}", f"tracks[{index}].{key}")
            if graph.nodes[node_id].type != expected_type:
                raise ManifestError(
                    f"node must have type {expected_type}", f"tracks[{index}].{key}"
                )
            refs[key] = node_id
        if refs["source"] not in graph.ancestors(refs["gain"]):
            raise ManifestError("gain is not downstream of source", f"tracks[{index}].gain")
        tracks.append(
            {
                "id": track_id,
                "source_node": refs["source"],
                "gain_node": refs["gain"],
                "bus_node": refs["bus"],
                "mute": _bool(track.get("mute", False), f"tracks[{index}].mute"),
                "solo": _bool(track.get("solo", False), f"tracks[{index}].solo"),
            }
        )
    any_solo = any(track["solo"] for track in tracks)
    for track in tracks:
        track["active"] = track["solo"] if any_solo else not track["mute"]
        graph.nodes[track["gain_node"]].params["_active"] = track["active"]
    return tracks


def load_project(path: str | Path) -> Project:
    project_path = Path(path).expanduser().resolve()
    manifest = _parse_document(project_path)
    sample_rate = _positive_int(manifest.get("sample_rate"), "sample_rate")
    channels = _positive_int(manifest.get("channels"), "channels")
    duration = require_finite("duration", _derive_duration(manifest), field="duration")
    if duration <= 0.0:
        raise ManifestError("must be positive", "duration")
    block_size = _positive_int(manifest.get("block_size", 2048), "block_size")
    clip_threshold_db = require_finite(
        "clip_threshold_db", manifest.get("clip_threshold_db", 0.0), field="clip_threshold_db"
    )
    max_clip_samples = _nonnegative_int(
        manifest.get("max_clip_samples", 0), "max_clip_samples"
    )

    graph = Graph().from_manifest(
        manifest.get("nodes", []), manifest.get("edges", [])
    )
    graph.validate()
    root = project_path.parent
    for node in graph.nodes.values():
        if node.type == "source":
            _enrich_source(node, root, sample_rate, channels)
        elif node.type in {"gain", "bus"}:
            _enrich_gain(node)
        elif node.type == "resample":
            _enrich_resample(node, sample_rate)
        elif node.type == "output":
            output_channels = node.params.get("channels", channels)
            output_channels = _positive_int(output_channels, f"nodes.{node.id}.channels")
            if output_channels != channels:
                raise ManifestError(
                    f"output has {output_channels} channels, project expects {channels}",
                    f"nodes.{node.id}.channels",
                )

    tracks = _normalise_tracks(manifest.get("tracks"), graph)
    master_bus = manifest.get("master_bus")
    if not isinstance(master_bus, str) or master_bus not in graph.nodes:
        raise ManifestError("unknown master bus node", "master_bus")
    if graph.nodes[master_bus].type != "bus":
        raise ManifestError("master_bus must reference a bus node", "master_bus")
    output_node = manifest.get("output")
    if not isinstance(output_node, str) or output_node not in graph.nodes:
        raise ManifestError("unknown output node", "output")
    if graph.nodes[output_node].type != "output":
        raise ManifestError("output must reference an output node", "output")
    return Project(
        path=project_path,
        manifest=manifest,
        name=str(manifest.get("name", project_path.stem)),
        sample_rate=sample_rate,
        channels=channels,
        duration=duration,
        duration_frames=max(1, int(round(duration * sample_rate))),
        block_size=block_size,
        clip_threshold_db=clip_threshold_db,
        max_clip_samples=max_clip_samples,
        graph=graph,
        tracks=tracks,
        master_bus=master_bus,
        output_node=output_node,
    )


def track_automation(project: Project, track: dict) -> AutomationCurve:
    return project.graph.nodes[track["gain_node"]].params["_automation"]

