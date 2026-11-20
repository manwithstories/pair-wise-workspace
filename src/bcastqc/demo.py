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
