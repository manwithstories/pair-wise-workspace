"""Delivery manifest: JSON summary plus post-run verification.

The manifest records per-track sources (relative paths only), segment
timestamps in master ticks, per-frame SHA-256 hashes, bitrate statistics
and audio quality metrics.  After writing, the outputs are re-checked
against the manifest: video frame count/timestamps and WAV length.
"""

from __future__ import annotations

import json
from pathlib import Path

import soundfile as sf

from .clock import MASTER_RATE, ticks_to_ms
from .errors import StageflowError
from .pipeline import PipelineResult
from .render import verify_video
from .timeline import Timeline


def build_manifest(
    timeline: Timeline,
    result: PipelineResult,
    out_dir: Path,
    target_bitrate: int,
) -> dict[str, object]:
    d = timeline.delivery
    actual = result.renderer.actual_bitrate()
    error_pct = (
        (actual - target_bitrate) / target_bitrate * 100.0 if target_bitrate else 0.0
    )
    tracks = []
    for t in timeline.tracks:
        tracks.append(
            {
                "id": t.id,
                "source": t.path,  # relative to the input manifest directory
                "offset_ticks": t.offset_ticks,
                "segments": [
                    {"start_tick": s.start_tick, "duration_ticks": s.duration_ticks}
                    for s in t.segments
                ],
            }
        )
    return {
        "job": timeline.job,
        "master_tick_rate": MASTER_RATE,
        "delivery": {
            "sample_rate": d.sample_rate,
            "channels": d.channels,
            "fps": d.fps,
            "width": d.width,
            "height": d.height,
            "start_tick": d.start_tick,
            "duration_ticks": d.duration_ticks,
        },
        "tracks": tracks,
        "muted_ranges": result.muted_ranges,
        "audio": {
            "peak": round(result.stats.peak, 6),
            "rms_dbfs": round(result.stats.rms_dbfs, 3),
            "clipping_pct": round(result.stats.clipping_pct, 6),
        },
        "video": {
            "frames": len(result.renderer.records),
            "frame_sha256": [
                {"index": r.index, "pts_ticks": r.pts_ticks, "sha256": r.sha256}
                for r in result.renderer.records
            ],
        },
        "bitrate": {
            "target": target_bitrate,
            "actual": actual,
            "error_pct": round(error_pct, 3),
        },
        "blocks": result.blocks,
        "missing_blocks": result.missing_blocks,
        "alignment_error_ms": round(
            ticks_to_ms(int(result.stats.max_align_error_ticks)), 6
        ),
        "backpressure_wait_ms": round(result.backpressure_wait_ms, 3),
        "stall_count": result.stall_count,
        "resumed_from_block": result.resumed_from,
    }


def write_manifest(manifest: dict[str, object], out_dir: Path, job: str) -> Path:
    path = out_dir / f"{job}.manifest.json"
    path.write_text(json.dumps(manifest, indent=2, ensure_ascii=False) + "\n",
                    encoding="utf-8")
    return path


def verify_outputs(timeline: Timeline, manifest: dict[str, object], out_dir: Path) -> None:
    """Cross-check manifest timestamps and the output frame sequence."""
    job = timeline.job
    video = out_dir / f"{job}.mp4"
    audio = out_dir / f"{job}.wav"
    expected_frames = int(manifest["video"]["frames"])  # type: ignore[index]
    count, ok = verify_video(video, expected_frames, timeline.delivery.fps)
    if not ok:
        raise StageflowError(
            "VERIFY_FRAMES",
            f"rendered video has {count} frames / broken timestamps, "
            f"expected {expected_frames}",
            stage="manifest", path=video.name,
        )
    info = sf.info(audio)
    expected_samples = int(
        timeline.delivery.duration_ticks * timeline.delivery.sample_rate // MASTER_RATE
    )
    if info.frames != expected_samples:
        raise StageflowError(
            "VERIFY_AUDIO",
            f"delivered WAV has {info.frames} frames, expected {expected_samples}",
            stage="manifest", path=audio.name,
        )
    # Frame pts must be the exact shared-time-base timestamps in the manifest.
    for rec in manifest["video"]["frame_sha256"]:  # type: ignore[index]
        if rec["pts_ticks"] * timeline.delivery.fps != rec["index"] * MASTER_RATE:
            raise StageflowError(
                "VERIFY_TIMESTAMPS",
                f"frame {rec['index']} pts {rec['pts_ticks']} off the shared time base",
                stage="manifest", path=video.name,
            )
