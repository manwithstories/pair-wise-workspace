"""End-to-end restoration orchestration wiring every stage together.

Data flow (mirrors the package architecture)::

    generator/local manifest -> parser -> unified timeline -> segment aligner
        -> bounded back-pressured stream -> recovery/checkpoint -> metrics

The pipeline runs its worker stages on bounded queues with a hard byte budget,
so a slow consumer pauses the upstream reader rather than buffering without
limit.  Outputs are written to a temp file and atomically renamed on success.
"""

from __future__ import annotations

import json
import os
import tempfile
import time
from dataclasses import asdict
from pathlib import Path

from .errors import ErrorCollector, ErrorKind, ErrorStage, PipelineError, skippable
from .manifest import parse_manifest
from .metrics import (
    MetricsReport,
    av_sync_error,
    media_timeline_spans,
    packet_recovery_rate,
    percentile_latency,
)
from .recovery import CheckpointStore, RecoveryOutcome, digest_inputs, recover_representation
from .segment import generate_windows
from .stream import Pipeline, PipelineCancelled, StreamItem
from .timeline import build_timelines

RECOVERY_MIN = 0.95


def _atomic_replace(tmp: Path, final: Path) -> None:
    final.parent.mkdir(parents=True, exist_ok=True)
    os.replace(tmp, final)


def process_asset(manifest_path: Path, output_dir: Path, *,
                  queue_bytes: int, consumer_delay_ms: int,
                  max_items: int = 64, cancel_after: int | None = None) -> dict:
    """Restore one manifest and return its JSON-serialisable result."""
    asset_id = manifest_path.parent.name
    errors = ErrorCollector()
    root = manifest_path.parent
    started = time.perf_counter()

    parsed = parse_manifest(manifest_path)
    errors.extend(parsed.errors.records)
    if parsed.has_fatal():
        return _abort(asset_id, output_dir, errors, started, reason="fatal manifest error")

    timeline_result = build_timelines(parsed.representations, errors)
    alignment = generate_windows(timeline_result.timelines, errors=errors)

    # Recovery per representation: dedup, reorder, fill gaps from peers.
    timelines = list(timeline_result.timelines.values())
    outcomes: list[RecoveryOutcome] = []
    for tl in timelines:
        outcomes.append(recover_representation(tl, timelines, errors))

    input_digest = digest_inputs(root)
    checkpoint = CheckpointStore(output_dir, input_digest)
    try:
        checkpoint.load()
    except Exception as exc:  # digest mismatch / corrupt checkpoint
        errors.add(skippable(ErrorStage.CHECKPOINT, asset_id,
                             f"checkpoint discarded: {exc}"))

    pipeline = Pipeline(max_items=max_items, max_bytes=queue_bytes)
    processed = _wire_stages(pipeline, checkpoint, output_dir, consumer_delay_ms)

    pipeline.run()
    submitted = 0
    try:
        for win in alignment.windows:
            if cancel_after is not None and submitted >= cancel_after:
                pipeline.cancel()
                break
            payload = json.dumps({"window": win.index, "start_us": win.start_us,
                                  "end_us": win.end_us, "members": win.members}).encode()
            pipeline.submit(StreamItem(f"{asset_id}:w{win.index}", payload, len(payload)))
            submitted += 1
        pipeline.finish()
    except PipelineCancelled:
        errors.add(skippable(ErrorStage.STREAM, asset_id, "pipeline cancelled by signal"))
    except PipelineError as exc:
        errors.add(exc.record)

    _write_recovered(output_dir, outcomes)

    video_dir = _first_dir(parsed, "video")
    audio_dir = _first_dir(parsed, "audio")
    spans = media_timeline_spans(video_dir, audio_dir, errors, asset_id)
    sync_ms, sync_status = av_sync_error(spans)
    report = MetricsReport(
        av_sync_error_ms=sync_ms,
        av_sync_status=sync_status,
        packet_recovery_rate=packet_recovery_rate(outcomes),
        p95_latency_ms=percentile_latency(list(pipeline.latencies_ms)),
        peak_memory_bytes=pipeline.peak_queue_bytes,
        aligned_representations=alignment.aligned_representations,
        max_alignment_deviation_us=alignment.max_deviation_us,
        gaps=[g.to_dict() for o in outcomes for g in o.gaps],
        missing_tracks=[k for k in ("video", "audio") if not spans[k].present],
    )

    result = {
        "asset_id": asset_id,
        "status": _status(report, errors),
        "duration_ms": round((time.perf_counter() - started) * 1000, 3),
        "submitted_windows": submitted,
        "processed_windows": processed["count"],
        "input_digest": input_digest,
        "metrics": report.to_dict(),
        "errors": errors.to_list(),
        "error_summary": _summarise(errors),
    }
    _atomic_write_json(output_dir / "result.json", result)
    return result


def _wire_stages(pipeline: Pipeline, checkpoint: CheckpointStore,
                 output_dir: Path, consumer_delay_ms: int) -> dict:
    state = {"count": 0}
    data_path = output_dir / "recovered.jsonl"

    def prepare(item: StreamItem, token) -> StreamItem:
        return item

    def persist(item: StreamItem, token) -> StreamItem | None:
        if consumer_delay_ms:
            time.sleep(consumer_delay_ms / 1000.0)
        output_dir.mkdir(parents=True, exist_ok=True)
        record = item.payload + b"\n"
        # Append under the same ordering as the bounded queue; checkpoints and
        # summary documents are published via temp-file + atomic replace.
        offset = data_path.stat().st_size if data_path.exists() else 0
        with open(data_path, "ab") as out:
            out.write(record)
            out.flush()
            os.fsync(out.fileno())
        checkpoint.mark_done(item.key, offset + len(record))
        state["count"] += 1
        return None

    pipeline.add_stage("prepare", prepare).add_stage("persist", persist)
    return state


def _write_recovered(output_dir: Path, outcomes: list[RecoveryOutcome]) -> None:
    summary = []
    for o in outcomes:
        summary.append({
            "rep_id": o.rep_id,
            "ordered_keys": [s.key for s in o.ordered],
            "duplicates_removed": o.duplicates_removed,
            "reordered": o.reordered,
            "recovered_from_peers": o.recovered,
            "gaps": [g.to_dict() for g in o.gaps],
        })
    _atomic_write_json(output_dir / "recovery.json", {"representations": summary})


def _first_dir(parsed, kind: str) -> Path:
    for rep in parsed.representations:
        if rep.kind == kind and rep.segments:
            return rep.segments[0].path.parent
    return Path("/nonexistent")


def _status(report: MetricsReport, errors: ErrorCollector) -> str:
    if errors.has_fatal():
        return "unrecoverable"
    ok, _ = report.meets_thresholds(RECOVERY_MIN)
    return "ok" if ok else "degraded"


def _summarise(errors: ErrorCollector) -> dict:
    return {kind.value: errors.count(kind) for kind in ErrorKind}


def _atomic_write_json(path: Path, payload: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=path.parent, prefix=".tmp-", suffix=".json")
    try:
        with os.fdopen(fd, "w") as fh:
            json.dump(payload, fh, indent=2, sort_keys=True)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, path)
    except BaseException:
        Path(tmp).unlink(missing_ok=True)
        raise


def _abort(asset_id: str, output_dir: Path, errors: ErrorCollector,
           started: float, reason: str) -> dict:
    result = {
        "asset_id": asset_id, "status": "unrecoverable",
        "duration_ms": round((time.perf_counter() - started) * 1000, 3),
        "reason": reason, "metrics": None, "errors": errors.to_list(),
        "error_summary": {kind.value: errors.count(kind) for kind in ErrorKind},
    }
    _atomic_write_json(output_dir / "result.json", result)
    return result
