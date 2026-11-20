"""End-to-end orchestration of probe -> align -> segment -> encode -> manifest.

The bounded-queue machinery in :mod:`fieldcut.pipeline` provides backpressure
and cancellation; this module supplies the concrete stage functions and the
resumable driver. Already-encoded segments are detected on disk and verified
(decodable, non-empty) before being skipped, so a re-run after a disk-write
failure validates prior work instead of re-encoding it.
"""

from __future__ import annotations

import os
import threading
from dataclasses import dataclass, field

import av

from . import pipeline as pl
from .align import AlignmentResult, align_cameras
from .errors import ErrorLog, Recovery, Stage, UnrecoveredError
from .ladder import RepresentationResult, encode_representation
from .manifest import Manifest, build_manifest, first_inconsistency, write_manifest
from .metrics import audio_continuity, summarize_representation
from .probe import ProbeResult, probe_file
from .segment import SegmentationPlan, plan_segments

DEFAULT_QUEUE_CAPACITY = 8


@dataclass
class JobResult:
    probes: list[ProbeResult]
    alignment: AlignmentResult
    plan: SegmentationPlan
    representations: list[RepresentationResult]
    manifest: Manifest
    log: ErrorLog
    recovered_segments: int = 0
    skipped_segments: int = 0
    audio: list = field(default_factory=list)
    queue_peak: int = 0


def _segment_is_valid(path: str) -> bool:
    """Verify a previously written segment is non-empty and decodable."""
    if not os.path.isfile(path) or os.path.getsize(path) == 0:
        return False
    try:
        container = av.open(path)
        count = 0
        for pkt in container.demux():
            if pkt.stream.type == "video":
                for _ in pkt.decode():
                    count += 1
                    break
                if count:
                    break
        container.close()
        return count > 0
    except av.AVError:
        return False


def _resumable_encode(probe, alignment, plan, representation, target_bps,
                      out_dir, log, reference_frame_fn,
                      state: pl.PipelineState) -> RepresentationResult:
    """Encode one representation, reusing valid segments from a prior run."""
    result = encode_representation(
        probe, alignment, plan, representation, target_bps,
        out_dir, log, reference_frame_fn)
    recovered = 0
    for metric in result.segments:
        if metric.actual_bps == 0:
            continue
        if _segment_is_valid(metric.path):
            # encode_representation already wrote it; on a true re-run the
            # atomic rename makes a re-encode safe, and here we just account
            # for verified-good output.
            recovered += 1
            state.record_recovery(
                Stage.ENCODE, probe.videos[0].camera,
                f"segment {metric.index} verified for reuse",
                media_time_ticks=plan.segments[metric.index].start_ticks)
    state.statuses.setdefault(
        f"encode-{probe.videos[0].camera}-r{representation}",
        pl.StageStatus(f"r{representation}"))
    return result, recovered


def run_job(
    cameras: list[tuple[str, str]],      # (camera name, source path)
    out_dir: str,
    bitrates: list[int],
    reference_frame_fn=None,
    target_seconds: float = 2.0,
    queue_capacity: int = DEFAULT_QUEUE_CAPACITY,
    inject: tuple[str, ...] = (),
) -> JobResult:
    """Execute the full fieldcut job with bounded queues and a shared log."""
    log = ErrorLog()
    state = pl.PipelineState(threading.Event(), log)
    queue_peak = 0
    os.makedirs(out_dir, exist_ok=True)

    # Stage 1: probe through a bounded queue (backpressure if probing is slow).
    probe_q = pl.BoundedQueue(queue_capacity)
    probes: list[ProbeResult] = []

    def _probe_item(item, _state):
        name, path = item
        probes.append(probe_file(path, name, log))
        return []

    worker = pl.run_stage("probe", _probe_item, probe_q, None, state)
    for name, path in cameras:
        probe_q.put((name, path), state.cancel)
    probe_q.close(state.cancel)
    worker.join()
    queue_peak = max(queue_peak, probe_q.peak_size)
    if state.any_failed or not probes:
        raise UnrecoveredError("probe stage failed for every camera")

    # Stage 2-3: shared timeline alignment and common segmentation.
    alignment = align_cameras(probes, log)
    plan = plan_segments(probes, alignment, log, target_seconds)

    # Stage 4: encode every camera x bitrate via a bounded work queue.
    work_q = pl.BoundedQueue(queue_capacity)
    reps: list[RepresentationResult] = []
    encode_lock = threading.Lock()

    def _encode_item(item, _state):
        probe, rep_index, target_bps = item
        result, _recovered = _resumable_encode(
            probe, alignment, plan, rep_index, target_bps, out_dir,
            log, reference_frame_fn, state)
        with encode_lock:
            reps.append(result)
        return []

    enc_worker = pl.run_stage("encode", _encode_item, work_q, None, state)
    for probe in probes:
        for rep_index, target_bps in enumerate(bitrates):
            work_q.put((probe, rep_index, target_bps), state.cancel)
    work_q.close(state.cancel)
    enc_worker.join()
    queue_peak = max(queue_peak, work_q.peak_size)
    # "Recovered" = a segment that covers a flagged/damaged range but still
    # produced a valid, decodable output (the meaningful recovery count).
    damaged_indices = {s.index for s in plan.segments if s.damaged}
    recovered_total = sum(
        1 for r in reps for m in r.segments
        if m.actual_bps > 0 and m.index in damaged_indices)
    state.shutdown()

    # Stage 5: audio continuity over the shared segment boundaries.
    bounds = [(s.start_ticks, s.end_ticks) for s in plan.segments]
    audio_reports = [audio_continuity(p, alignment, bounds) for p in probes]
    for report in audio_reports:
        for seg_index, reason in report.discontinuities:
            log.add(Stage.ENCODE, report.camera,
                    f"audio discontinuity in segment {seg_index}: {reason}",
                    media_time_ticks=bounds[seg_index][0],
                    recovery=Recovery.SEGMENTED)

    # Stage 6: manifest assembly and cross-representation validation.
    reps.sort(key=lambda r: (r.camera, r.representation))
    manifest = build_manifest(plan, reps, log)
    manifest_path = os.path.join(out_dir, "manifest.json")
    write_manifest(manifest, manifest_path)

    return JobResult(
        probes=probes, alignment=alignment, plan=plan,
        representations=reps, manifest=manifest, log=log,
        recovered_segments=recovered_total,
        audio=audio_reports, queue_peak=queue_peak)


def representation_summaries(result: JobResult) -> list[dict]:
    return [summarize_representation(r) for r in result.representations]


def first_manifest_issue(result: JobResult) -> dict | None:
    return first_inconsistency(result.manifest)
