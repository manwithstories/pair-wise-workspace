"""End-to-end pipeline orchestration and restart flow.

Stages run manifest -> clock-normalized decode -> composition ->
offscreen render -> audio/stats, sharing :class:`ErrorBook` and the clock
state.  Checkpoint persistence, orphan cleanup and statistics live in
:mod:`relay_timeline.checkpoint` and :mod:`relay_timeline.stats`.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

import numpy as np

from .audio import AudioAssembler
from .checkpoint import (Checkpoint, cleanup_orphan_frames,
                         peak_memory_mb)
from .compose import Composer, attach_events
from .config import (DEFAULT_DRIFT_PPM_LIMIT, DEFAULT_QUEUE_SIZE,
                     POLICY_GAP_FILL, Limits)
from .decode import DecodeWorker
from .errors import ErrorBook, ErrorCode, RecoverableError
from .manifest import ManifestPlan, parse_manifest
from .render import FrameExporter
from .stats import compute_stats
from .streamio import (AudioItem, BoundedQueue, CancellationToken,
                       VideoItem)


@dataclass(slots=True)
class PipelineConfig:
    queue_size: int = DEFAULT_QUEUE_SIZE
    drift_ppm_limit: float = DEFAULT_DRIFT_PPM_LIMIT
    policy: str = POLICY_GAP_FILL
    target_bitrate: int = 0
    abort_after: int | None = None  # segments before simulated crash


@dataclass(slots=True)
class PipelineResult:
    plan: ManifestPlan
    errors: ErrorBook
    frames: int
    frame_hashes: list[str]
    audio_stats: dict
    bitrate_actual: int
    bitrate_error_pct: float
    psnr_db: float
    alignment_ms: float
    peak_memory_mb: float
    resumed: bool
    summary: dict = field(default_factory=dict)


def run_pipeline(manifest_path: str, out_dir: str | Path,
                 config: PipelineConfig, scenario: dict,
                 limits: Limits | None = None) -> PipelineResult:
    """Execute one reconstruction run, recovering from any valid checkpoint."""
    limits = limits or Limits()
    out_dir = Path(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    errors = ErrorBook()
    plan = parse_manifest(manifest_path, limits)
    checkpoint = Checkpoint(out_dir)
    resumed = checkpoint.load(plan)
    if resumed:
        cleanup_orphan_frames(out_dir, checkpoint.frame_count)

    modulus = int(scenario["modulus"])
    fps = int(scenario["nominal_fps"])
    shape = (int(scenario["height"]), int(scenario["width"]), 3)
    sample_rate = int(scenario["sample_rate"])
    token = CancellationToken()
    queue = BoundedQueue(min(config.queue_size, limits.max_queue_size), token)
    worker = DecodeWorker(plan, modulus, queue, token)
    composer = Composer(fps, modulus, config.policy, errors)
    audio = AudioAssembler(sample_rate, config.drift_ppm_limit)
    exporter = FrameExporter(out_dir, limits.max_output_frames)
    if resumed:
        exporter.records = exporter.records[: checkpoint.frame_count]
    segment_start_pts: list[Fraction] = []
    seg_duration = plan.segments[0].duration if plan.segments else Fraction(2)
    plan_by_seq = {seg.sequence: seg for seg in plan.segments}
    processed: list[int] = []
    current_seq: int | None = None
    failed: set[int] = set()

    def apply_missing(sequence: int, exc: RecoverableError) -> None:
        seg = plan_by_seq[sequence]
        start_s = sequence * seg_duration
        end_s = start_s + seg.duration
        if config.policy == "fail":
            errors.add(exc)
            token.cancel()
            worker.join()
            raise exc
        errors.add(exc, recovered=True,
                   time_range=(int(start_s * 90_000), int(end_s * 90_000)))
        composer.handle_missing(sequence, seg.path.name, start_s, end_s, shape)
        for frame in composer.frames[len(exporter.records):]:
            exporter.export(frame)
        audio.add(AudioItem(
            pcm=np.zeros(int(seg_duration * sample_rate), dtype=np.int16),
            pts_seconds=start_s,
            nominal_seconds=start_s,
            sample_rate=sample_rate,
            segment=seg.path.name, sequence=sequence,
        ), start_s)
        segment_start_pts.append(start_s)
        failed.add(sequence)

    worker.start()
    while True:
        item = queue.pop(timeout=30.0)
        if isinstance(item, tuple):
            kind, seq, payload = item
            if kind == "fatal":
                token.cancel()
                worker.join()
                raise payload
            if kind == "error":
                apply_missing(seq, payload)
                continue
            if kind == "done":
                break
        if isinstance(item, VideoItem):
            if current_seq != item.sequence:
                if current_seq is not None and current_seq not in failed:
                    processed.append(current_seq)
                    checkpoint.save(
                        plan, processed,
                        worker.decoder.total_wraps if worker.decoder else 0,
                        len(exporter.records))
                    if (config.abort_after is not None
                            and len(processed) >= config.abort_after):
                        token.cancel()
                        worker.join()
                        raise RecoverableError(
                            ErrorCode.WRITE_INTERRUPTED,
                            f"simulated crash after {len(processed)} segment(s)",
                        )
                current_seq = item.sequence
                segment_start_pts.append(item.pts_seconds)
            composer.handle_video(item)
            exporter.export(composer.frames[-1])
        elif isinstance(item, AudioItem):
            # Independent undrifted reference grid vs the audio node clock:
            # regressing observed audio PTS on this reference recovers ppm.
            audio.add(item, item.nominal_seconds)
    worker.join()
    if current_seq is not None and current_seq not in failed:
        processed.append(current_seq)
        checkpoint.save(
            plan, processed,
            worker.decoder.total_wraps if worker.decoder else 0,
            len(exporter.records))

    frames = composer.sort_timeline()
    exporter.write_index()
    events_path = Path(manifest_path).parent / "events.json"
    bound_events = attach_events(frames, json.loads(
        events_path.read_text(encoding="utf-8")))
    audio_stats = audio.write_wav(out_dir / "audio.wav").to_dict()
    if audio_stats["isolated"]:
        errors.add(RecoverableError(
            ErrorCode.DRIFT_EXCEEDED,
            f"audio drift {audio_stats['drift_ppm']:.1f} ppm exceeds limit "
            f"{config.drift_ppm_limit:.1f} ppm; track isolated",
        ))
    stats = compute_stats(plan, frames, exporter, audio_stats,
                          segment_start_pts, queue, resumed, bound_events,
                          composer, errors)
    stats["peak_memory_mb"] = peak_memory_mb()
    (out_dir / "report.json").write_text(
        json.dumps(stats, indent=2, default=float), encoding="utf-8")
    return PipelineResult(
        plan=plan, errors=errors, frames=len(exporter.records),
        frame_hashes=exporter.hashes, audio_stats=audio_stats,
        bitrate_actual=stats["bitrate_actual_bps"],
        bitrate_error_pct=stats["bitrate_error_pct"],
        psnr_db=stats["quality_psnr_db"],
        alignment_ms=stats["manifest_alignment_ms"],
        peak_memory_mb=stats["peak_memory_mb"], resumed=resumed,
        summary=stats,
    )

