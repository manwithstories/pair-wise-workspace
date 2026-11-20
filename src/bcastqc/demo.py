"""Acceptance demo: generate fixtures, run the full QC scenario, report.

The demo is the executable form of the brief's acceptance command.  It:

1. generates a deterministic fixture set (H.264/H.265/AV1 programs, PCM at
   48 kHz and 44.1 kHz, plus dropped/truncated/bad-index variants),
2. builds a timeline that exercises the required behaviours - two video clips,
   an audio offset, a second audio track at 44.1 kHz, a negative audio offset
   handled by source trimming, and one track that is declared but not
   referenced (excluded, not silently composed),
3. runs the pipeline with a bounded queue and an artificially slow consumer,
4. exercises the failure cases (dropped packets, truncation, bad index) and
   prints a single JSON result with the acceptance fields:

   ``sync_error_ms_p95``, ``packet_recovery_rate``, ``latency_ms_p95``,
   ``peak_queue_frames``, ``frame_hash_mismatches`` and ``exit_code``.
"""

from __future__ import annotations

import json
import shutil
import time
from fractions import Fraction
from pathlib import Path
from typing import Any

from . import EXIT_OK
from .bitstream import Codec, KeyframeIndex, validate_index
from .errors import QcError
from .fixtures import FixtureSet, FixtureSpec, generate_fixture_set, split_units
from .pipeline import RunParams, clean_reference, run_pipeline, verify_manifest
from .timeline import Timeline, load_timeline, topological_order

__all__ = ["run_demo"]

TIME_BASE = "1/90000"


def _ticks(seconds: Fraction | int | str) -> int:
    value = seconds if isinstance(seconds, Fraction) else Fraction(seconds)
    return int(value * 90000)


def build_demo_timeline(fixtures: FixtureSet, spec: FixtureSpec) -> dict[str, Any]:
    """A timeline exercising clip graph, offsets, rate mismatch and exclusion."""
    seconds = Fraction(spec.frames, 1) / spec.frame_rate
    total_ticks = _ticks(seconds)

    return {
        "time_base": TIME_BASE,
        "frame_rate": f"{spec.frame_rate.numerator}/{spec.frame_rate.denominator}",
        "width": 160,
        "height": 90,
        "output_channels": 2,
        "output_sample_rate": 48000,
        "tracks": [
            {
                "id": "video_main",
                "kind": "video",
                "path": "program_h264.bin",
                "codec": "h264",
            },
            {
                "id": "video_av1",
                "kind": "video",
                "path": "program_av1.bin",
                "codec": "av1",
                "depends_on": ["video_main"],
                "frame_rate": "25/1",
            },
            {
                "id": "audio_48k",
                "kind": "audio",
                "path": "program_audio.wav",
                "sample_rate": 48000,
                "channels": 2,
            },
            {
                "id": "audio_44k",
                "kind": "audio",
                "path": "program_audio_44100.wav",
                "sample_rate": 44100,
                "channels": 2,
            },
            {
                "id": "unused_slate",
                "kind": "video",
                "path": "program_h265.bin",
                "codec": "h265",
            },
        ],
        "clips": [
            {
                "id": "program_video",
                "track": "video_main",
                "start_ticks": 0,
                "source_in_ticks": 0,
                "duration_ticks": total_ticks,
            },
            {
                # AV1 programme in the back half of the window, overlapping the
                # H.264 programme so composition order is exercised.
                "id": "av1_inset",
                "track": "video_av1",
                "start_ticks": total_ticks // 2,
                # source_in mirrors the timeline position so the shared sync
                # marker stays aligned when the inset is composited on top
                "source_in_ticks": total_ticks // 2,
                "duration_ticks": total_ticks // 2,
            },
            {
                "id": "program_audio",
                "track": "audio_48k",
                "start_ticks": 0,
                "source_in_ticks": 0,
                "duration_ticks": total_ticks,
            },
            {
                # 44.1 kHz bed mixed under the whole window: exercises resampling
                "id": "music_bed",
                "track": "audio_44k",
                "start_ticks": 0,
                "source_in_ticks": 0,
                "duration_ticks": total_ticks,
                "depends_on": ["program_video"],
            },
        ],
    }


def _exercise_index(fixtures: FixtureSet) -> dict:
    """Validate a good, a bad and a recovered keyframe index."""
    source = fixtures.by_role("video_h264")
    bad = fixtures.by_role("index_bad")
    good = fixtures.by_role("index_h264")
    size = source.size
    units = split_units(source.path.read_bytes(), Codec.H264)
    good_index = KeyframeIndex.from_file(good.path)
    good_validation = validate_index(good_index, size, keyframes=units)
    bad_index = KeyframeIndex.from_file(bad.path)
    bad_validation = validate_index(bad_index, size, keyframes=units, policy="scan")
    return {
        "good_accepted": good_validation.accepted,
        "good_entries": len(good_index.entries),
        "bad_accepted": bad_validation.accepted,
        "bad_issue_kinds": sorted({issue.kind for issue in bad_validation.issues}),
        "bad_source_bytes": [issue.source_byte for issue in bad_validation.issues],
        "recovered_entries": len(bad_validation.recovered_index.entries) if bad_validation.recovered_index else 0,
    }


def _exercise_failure_cases(fixtures: FixtureSet, base_timeline: dict, out: Path, spec: FixtureSpec,
                            aligned_timeline: Timeline) -> dict:
    """Run truncation and unrecoverable-loss scenarios, recording exit codes."""
    results: dict[str, Any] = {}

    # 1. truncated stream: decodes a prefix, conceals the tail, exit code 3
    truncated = fixtures.by_role("video_truncated")
    timeline = json.loads(json.dumps(base_timeline))
    timeline["tracks"] = [
        {"id": "video_main", "kind": "video", "path": truncated.path.name, "codec": "h264"},
        {"id": "audio_48k", "kind": "audio", "path": "program_audio.wav", "sample_rate": 48000, "channels": 2},
    ]
    timeline["clips"] = [
        {"id": "program_video", "track": "video_main", "start_ticks": 0,
         "source_in_ticks": 0, "duration_ticks": _ticks(Fraction(spec.frames) / spec.frame_rate)},
        {"id": "program_audio", "track": "audio_48k", "start_ticks": 0,
         "source_in_ticks": 0, "duration_ticks": _ticks(Fraction(spec.frames) / spec.frame_rate)},
    ]
    result = _safe_run(timeline, out / "case_truncated", RunParams(seed=spec.seed, block_frames=8),
                       base_dir=fixtures.root)
    results["truncated"] = {
        "exit_code": result["report"]["exit_code"],
        "frames_rendered": result["report"]["frames_rendered"],
        "concealed_frames": result["report"]["concealed_frames"],
        "max_gap_ms": result["report"]["max_gap_ms"],
    }

    # 2. all losses are hard: every recovery fails, output stays complete but
    #    the recovery rate is zero and concealed frames are flagged.
    timeline = json.loads(json.dumps(base_timeline))
    timeline["tracks"] = [
        {"id": "video_main", "kind": "video", "path": "program_h264.bin", "codec": "h264"},
        {"id": "audio_48k", "kind": "audio", "path": "program_audio.wav", "sample_rate": 48000, "channels": 2},
    ]
    timeline["clips"] = [
        {"id": "program_video", "track": "video_main", "start_ticks": 0,
         "source_in_ticks": 0, "duration_ticks": _ticks(Fraction(spec.frames) / spec.frame_rate)},
        {"id": "program_audio", "track": "audio_48k", "start_ticks": 0,
         "source_in_ticks": 0, "duration_ticks": _ticks(Fraction(spec.frames) / spec.frame_rate)},
    ]
    total_loss = _safe_run(
        timeline,
        out / "case_hard_loss",
        RunParams(seed=spec.seed, drop_rate=0.05, hard_loss_rate=1.0, block_frames=8),
        base_dir=fixtures.root,
    )
    results["hard_loss"] = {
        "exit_code": total_loss["report"]["exit_code"],
        "packet_recovery_rate": total_loss["report"]["packet_recovery_rate"],
        "unrecoverable_packets": total_loss["report"]["unrecoverable_packets"],
        "concealed_frames": total_loss["report"]["concealed_frames"],
    }

    # 3. audio offset: the reference pass exposes the true A/V displacement
    shifted = json.loads(json.dumps(base_timeline))
    for clip in shifted["clips"]:
        if clip["track"] in ("audio_48k", "audio_44k"):
            clip["start_ticks"] += 3600  # +40 ms on every audio track
            clip["duration_ticks"] -= 3600
    timeline_path = out / "case_offset" / "timeline.json"
    timeline_path.parent.mkdir(parents=True, exist_ok=True)
    timeline_path.write_text(json.dumps(shifted), encoding="utf-8")
    try:
        shifted_timeline = load_timeline(timeline_path, base_dir=fixtures.root)
        # Compare against the *aligned* reference: the measured displacement is
        # then the offset we injected, not zero by construction.
        aligned_reference = clean_reference(aligned_timeline, block_frames=8)
        offset_result = run_pipeline(
            shifted_timeline,
            out / "case_offset" / "run",
            RunParams(seed=spec.seed, block_frames=8, queue_blocks=1, reference=True,
                      **aligned_reference.as_params()),
        )
        offset_manifest = json.loads((out / "case_offset" / "run" / "manifest.json").read_text())
        placement = offset_manifest.get("alignment", {}).get("av_placement", {})
        results["audio_offset"] = {
            "av_offset_frames": placement.get("av_offset_frames"),
            "sync_error_ms_p95": offset_result.report.sync_error_ms_p95,
        }
    except QcError as exc:
        results["audio_offset"] = {"error": exc.to_dict()}

    # 4. corrupt access unit: conformance checks and the clean-reference
    #    comparison must expose the damage, and no frame may be reported as
    #    recovered
    corrupt = fixtures.by_role("video_corrupt")
    timeline = json.loads(json.dumps(base_timeline))
    timeline["tracks"] = [
        {"id": "video_main", "kind": "video", "path": corrupt.path.name, "codec": "h264"},
        {"id": "audio_48k", "kind": "audio", "path": "program_audio.wav", "sample_rate": 48000, "channels": 2},
    ]
    timeline["clips"] = [
        {"id": "program_video", "track": "video_main", "start_ticks": 0,
         "source_in_ticks": 0, "duration_ticks": _ticks(Fraction(spec.frames) / spec.frame_rate)},
        {"id": "program_audio", "track": "audio_48k", "start_ticks": 0,
         "source_in_ticks": 0, "duration_ticks": _ticks(Fraction(spec.frames) / spec.frame_rate)},
    ]
    corrupt_ref = clean_reference(aligned_timeline, block_frames=8)
    corrupt_result = _safe_run(
        timeline, out / "case_corrupt",
        RunParams(seed=spec.seed, block_frames=8, queue_blocks=1, reference=True,
                  **corrupt_ref.as_params()),
        base_dir=fixtures.root,
    )
    report = corrupt_result["report"]
    alignment = corrupt_result.get("alignment", {})
    results["corrupt"] = {
        "exit_code": report["exit_code"],
        "illegal_units": report.get("illegal_units"),
        "recovered_packets": report.get("recovered_packets"),
        "duplicated_count": alignment.get("duplicated_count"),
        "missing_frames": (alignment.get("placement") or {}).get("missing_count"),
    }

    # 5. invalid timeline (dependency cycle) is rejected with exit code 2
    cycle = json.loads(json.dumps(base_timeline))
    cycle["tracks"][0]["depends_on"] = ["video_av1"]
    try:
        path = out / "case_cycle.json"
        path.write_text(json.dumps(cycle), encoding="utf-8")
        load_timeline(path)
        results["cycle"] = {"exit_code": EXIT_OK, "unexpected": True}
    except QcError as exc:
        results["cycle"] = {"exit_code": exc.exit_code, "category": exc.category.value,
                            "cycle": exc.details.get("cycle")}
    return results


def _safe_run(timeline_doc: dict, out: Path, params: RunParams, *, base_dir: Path) -> dict:
    out.mkdir(parents=True, exist_ok=True)
    path = out / "timeline.json"
    path.write_text(json.dumps(timeline_doc), encoding="utf-8")
    try:
        timeline = load_timeline(path, base_dir=base_dir)
        result = run_pipeline(timeline, out, params)
        payload = result.to_json()
        try:
            manifest = json.loads((out / "manifest.json").read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            manifest = {}
        return {"report": payload, "alignment": manifest.get("alignment", {}), "error": None}
    except QcError as exc:
        return {"report": {"exit_code": exc.exit_code}, "error": exc.to_dict()}


def manifest_alignment(run_dir: Path) -> dict:
    """Read the alignment block the pipeline recorded in the manifest."""
    try:
        manifest = json.loads((Path(run_dir) / "manifest.json").read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return {}
    return manifest.get("alignment", {})
