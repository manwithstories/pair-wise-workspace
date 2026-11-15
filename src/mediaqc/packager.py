"""Segment manifests built from the existing BMFF sample timeline."""

from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any

from .bmff import BoxReader
from .model import Track
from .sniff import SniffError, sniff_file
from .tracks import parse_mp4_tracks


class PackError(RuntimeError):
    def __init__(self, message: str, *, kind: str = "bounds",
                 details: list[dict[str, Any]] | None = None) -> None:
        super().__init__(message)
        self.kind = kind
        self.details = details or []


def pack_file(path: str | Path, target_ms: int, out_dir: str | Path) -> dict[str, Any]:
    if target_ms <= 0:
        raise PackError("target-ms must be positive", kind="argument")
    source = Path(path)
    try:
        sniff = sniff_file(source)
    except SniffError as exc:
        raise PackError(str(exc), kind="unsupported") from exc
    if sniff.container not in {"mp4", "mov"}:
        raise PackError(f"unsupported_container: {sniff.container}", kind="unsupported")
    scan = BoxReader(source).scan()
    if scan.has_moof:
        raise PackError("unsupported_layout: fragmented MP4/MOV", kind="unsupported")
    if not scan.mdat_ranges:
        raise PackError("no mdat box", kind="bounds")
    with source.open("rb") as handle:
        tracks, findings, _movie_ts = parse_mp4_tracks(handle, scan)
    if not tracks:
        raise PackError("no tracks parsed", kind="bounds")
    unexpanded = [{
        "track_index": track.index,
        "sample_count": track.sample_count,
        "expanded": len(track.samples),
        "reason": "sample_table_not_expanded",
    } for track in tracks if track.sample_count != len(track.samples)]
    if unexpanded:
        raise PackError("sample table could not be expanded for safe packing",
                        details=unexpanded)
    bad_ranges = _bad_ranges(tracks, scan.mdat_ranges)
    if bad_ranges:
        raise PackError("sample byte ranges escape mdat", details=bad_ranges)
    primary = next((track for track in tracks if track.kind == "video"), None)
    if primary is None:
        primary = next((track for track in tracks if track.kind == "audio"), None)
    if primary is None:
        primary = tracks[0]
    if not primary.samples:
        raise PackError("primary track has no expandable sample timeline", kind="bounds")
    segments = _segment_primary(primary, target_ms)
    if not segments:
        raise PackError("no segments could be created", kind="bounds")
    media_end_ms = max(
        (_ticks_ms(sample.dts + sample.duration, track.timescale)
         for track in tracks for sample in track.samples),
        default=segments[-1]["end_ms"],
    )
    if media_end_ms > segments[-1]["end_ms"]:
        segments[-1]["end_ms"] = media_end_ms
    manifest_segments = []
    for number, segment in enumerate(segments, 1):
        item = {
            "segment": number,
            "start_ms": round(segment["start_ms"], 3),
            "end_ms": round(segment["end_ms"], 3),
            "duration_ms": round(segment["end_ms"] - segment["start_ms"], 3),
            "tracks": [],
        }
        for track in tracks:
            selected = _samples_in_segment(track, segment)
            if not selected:
                continue
            byte_ranges = [{
                "sample_index": sample.index,
                "offset": sample.offset,
                "size": sample.size,
            } for sample in selected]
            item["tracks"].append({
                "track_index": track.index,
                "kind": track.kind,
                "codec": track.codec,
                "start_ms": round(_ticks_ms(selected[0].dts, track.timescale), 3),
                "end_ms": round(_ticks_ms(selected[-1].dts + selected[-1].duration,
                                          track.timescale), 3),
                "bytes": sum(sample.size for sample in selected),
                "byte_ranges": byte_ranges,
            })
        manifest_segments.append(item)
    deviations = [abs(item["duration_ms"] - target_ms) for item in manifest_segments]
    manifest = {
        "source": str(source),
        "container": sniff.container,
        "target_ms": target_ms,
        "segments": len(manifest_segments),
        "max_duration_deviation_ms": round(max(deviations), 3),
        "findings_seen": len(findings),
        "segment_list": manifest_segments,
    }
    _write_manifest(out_dir, manifest)
    return manifest


def _ticks_ms(ticks: int, timescale: int) -> float:
    return ticks / timescale * 1000 if timescale else 0.0


def _bad_ranges(tracks: list[Track], mdat_ranges: list[tuple[int, int]]) -> list[dict[str, Any]]:
    bad: list[dict[str, Any]] = []
    for track in tracks:
        for sample in track.samples:
            end = sample.offset + sample.size
            inside = any(sample.offset >= low and end <= high for low, high in mdat_ranges)
            if sample.size <= 0 or not inside:
                bad.append({
                    "track_index": track.index,
                    "sample_index": sample.index,
                    "offset": sample.offset,
                    "end": end,
                    "size": sample.size,
                    "reason": "empty_range" if sample.size <= 0 else "outside_mdat",
                })
    return bad


def _segment_primary(track: Track, target_ms: int) -> list[dict[str, Any]]:
    target_ticks = target_ms * track.timescale / 1000
    if not track.timescale or target_ticks <= 0:
        raise PackError("primary track timescale is invalid", kind="bounds")
    sync = [sample for sample in track.samples if sample.sync]
    if not sync:
        raise PackError("primary track has no sync samples", kind="bounds")
    if not track.samples[0].sync:
        raise PackError("first primary sample is not a sync sample", kind="bounds")
    segments: list[dict[str, Any]] = []
    start = track.samples[0]
    elapsed = 0.0
    for sample in track.samples[1:]:
        if sample.sync and elapsed + sample.duration >= target_ticks:
            segments.append(_make_segment(track, start, sample))
            start = sample
            elapsed = 0.0
        elapsed += sample.duration
    last = track.samples[-1]
    segments.append(_make_segment(track, start, None, last=last))
    return segments


def _make_segment(track: Track, start, stop, last=None) -> dict[str, Any]:
    if stop is None:
        end_tick = last.dts + last.duration
        last_index = last.index
    else:
        end_tick = stop.dts
        last_index = stop.index - 1
    return {
        "start_index": start.index,
        "end_index": last_index,
        "start_ms": _ticks_ms(start.dts, track.timescale),
        "end_ms": _ticks_ms(end_tick, track.timescale),
    }


def _samples_in_segment(track: Track, segment: dict[str, Any]):
    selected = []
    for sample in track.samples:
        start_ms = _ticks_ms(sample.dts, track.timescale)
        if segment["start_ms"] <= start_ms < segment["end_ms"]:
            selected.append(sample)
    return selected


def _write_manifest(out_dir: str | Path, manifest: dict[str, Any]) -> None:
    root = Path(out_dir)
    root.mkdir(parents=True, exist_ok=True)
    files = {"manifest.json": manifest}
    for item in manifest["segment_list"]:
        files[f"segment_{item['segment']:04d}.json"] = item
    for name, payload in files.items():
        target = root / name
        if target.parent.resolve() != root.resolve() or \
                not target.resolve().is_relative_to(root.resolve()):
            raise PackError("refusing to write outside output directory", kind="argument")
        temporary = root / f".{name}.tmp"
        temporary.write_text(json.dumps(payload, ensure_ascii=False, indent=2) + "\n",
                             encoding="utf-8")
        os.replace(temporary, target)
