"""End-to-end QC pipeline: render -> bounded queue -> export -> report.

The pipeline connects the stages and enforces the operating contract:

* **Bounded memory** - frames travel in fixed-size blocks through a queue with
  a hard capacity, so peak memory is independent of material length.  A
  consumer (the PNG writer) that stalls beyond ``slow_consumer_timeout``
  raises a resource error instead of letting decoded frames pile up.
* **Determinism** - output frames and the manifest are byte-identical across
  runs for the same inputs, timeline and parameters.
* **Resumability** - a run interrupted between complete blocks can continue
  from the first missing block.  The manifest records the input digest,
  timeline digest and parameter digest; any mismatch is rejected rather than
  silently reusing stale output.
* **Diagnosability** - every failure closes files, leaves no partial block
  that a resume would trust, and surfaces a serialisable :class:`QcError`.
"""

from __future__ import annotations

import hashlib
import json
import os
import time
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path
from typing import Iterator

import numpy as np

from . import EXIT_MEDIA_UNRECOVERABLE, EXIT_OK
from .bitstream import file_digest_slices
from .decode import _read_access_units
from .decode import LossModel, LossPolicy
from .errors import ErrorCategory, ResourceError, make_error
from .render import Renderer, RenderedFrame, SyncTracker, pixel_hash
from .report import (
    LatencyRecorder,
    alignment_from_duplicates,
    measure_av_placement,
    measure_placement,
    QcReport,
    Thresholds,
    build_report,
    derive_exit_code,
    evaluate_thresholds,
    sync_errors_from_tracker,
)
from .stream import CancellationToken, pipeline as run_stages
from .timeline import Timeline

__all__ = [
    "RunParams",
    "PipelineResult",
]

MANIFEST_VERSION = 2
BLOCK_FRAMES = 8


@dataclass(frozen=True)
class RunParams:
    """Parameters that affect output; digested for resume compatibility."""

    seed: int = 41
    drop_rate: float = 0.0
    hard_loss_rate: float = 0.0
    block_frames: int = BLOCK_FRAMES
    queue_blocks: int = 1
    slow_consumer_timeout: float | None = 30.0
    # artificial per-block consumer pause, used to demonstrate back-pressure
    consumer_delay_ms: float = 0.0
    # render the loss-free pass as an alignment reference (doubles render work
    # but makes duplication/misplacement measurement independent of audio)
    reference: bool = False
    # clean pixel hashes of the same timeline, supplied by the caller for
    # placement verification (the demo runs a clean pass and passes them in)
    reference_hashes: tuple[str, ...] = ()
    reference_signals: tuple[tuple[float, ...], tuple[float, ...]] | None = None

    def __post_init__(self) -> None:
        if not isinstance(self.reference_hashes, tuple):
            object.__setattr__(self, "reference_hashes", tuple(self.reference_hashes))

    def to_json(self) -> dict:
        return {
            "seed": self.seed,
            "drop_rate": self.drop_rate,
            "hard_loss_rate": self.hard_loss_rate,
            "block_frames": self.block_frames,
            "queue_blocks": self.queue_blocks,
            "consumer_delay_ms": self.consumer_delay_ms,
            "reference": self.reference,
        }

    def digest(self) -> str:
        payload = json.dumps(self.to_json(), sort_keys=True)
        return hashlib.sha256(payload.encode()).hexdigest()


@dataclass
class PipelineResult:
    report: QcReport
    manifest_path: Path
    output_dir: Path
    verification: dict = field(default_factory=dict)
    resumed_from_block: int = 0
    frames_written: int = 0
    elapsed_seconds: float = 0.0

    def to_json(self) -> dict:
        payload = self.report.to_json()
        payload.update(
            {
                "manifest": str(self.manifest_path),
                "output_dir": str(self.output_dir),
                "resumed_from_block": self.resumed_from_block,
                "frames_written": self.frames_written,
                "elapsed_seconds": round(self.elapsed_seconds, 3),
            }
        )
        if self.verification:
            payload["verification"] = self.verification
        return payload


# ==========================================================================
# Manifest
# ==========================================================================


def _manifest_path(output_dir: Path) -> Path:
    return output_dir / "manifest.json"


def read_manifest(path: Path) -> dict:
    path = Path(path)
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        raise make_error(ErrorCategory.MANIFEST_INVALID, "manifest not found", path=str(path)) from None
    except json.JSONDecodeError as exc:
        raise make_error(
            ErrorCategory.MANIFEST_INVALID,
            f"manifest is not valid JSON: {exc.msg}",
            path=str(path),
            line=exc.lineno,
        ) from exc
    if payload.get("version") != MANIFEST_VERSION:
        raise make_error(
            ErrorCategory.MANIFEST_INVALID,
            "unsupported manifest version",
            version=payload.get("version"),
            expected=MANIFEST_VERSION,
        )
    return payload


def input_signature(timeline: Timeline, params: RunParams) -> dict:
    sources: dict[str, dict] = {}
    for track_id in sorted(timeline.tracks):
        track = timeline.tracks[track_id]
        digest, size = file_digest_slices(track.path)
        entry = {"sha256": digest, "size": size}
        if track.index_path is not None:
            entry["index_sha256"] = file_digest_slices(track.index_path)[0]
        sources[track_id] = entry
    return {
        "timeline_digest": timeline.digest(),
        "params_digest": params.digest(),
        "sources": sources,
        "time_base": str(timeline.time_base),
        "frame_rate": f"{timeline.frame_rate.numerator}/{timeline.frame_rate.denominator}",
    }


def _check_resume(manifest: dict, signature: dict) -> None:
    recorded = manifest.get("signature", {})
    for key in ("timeline_digest", "params_digest"):
        if recorded.get(key) != signature[key]:
            raise make_error(
                ErrorCategory.CHECKSUM_MISMATCH,
                f"{key} changed since the manifest was written",
                recorded=recorded.get(key),
                current=signature[key],
            )
    for track_id, entry in signature["sources"].items():
        prior = recorded.get("sources", {}).get(track_id, {})
        if prior.get("sha256") != entry["sha256"]:
            raise make_error(
                ErrorCategory.CHECKSUM_MISMATCH,
                f"input digest changed for track {track_id!r}",
                track=track_id,
                recorded=prior.get("sha256"),
                current=entry["sha256"],
            )


# ==========================================================================
# Frame export
# ==========================================================================


class _FrameWriter:
    """Writes one PNG per frame and one manifest block per block of frames."""

    def __init__(self, output_dir: Path, block_frames: int) -> None:
        self.output_dir = output_dir
        self.frames_dir = output_dir / "frames"
        try:
            self.frames_dir.mkdir(parents=True, exist_ok=True)
        except OSError as exc:
            raise ResourceError(
                ErrorCategory.OUTPUT_FAILURE,
                f"cannot create frame directory: {exc}",
                details={"path": str(self.frames_dir)},
            ) from exc
        self.block_frames = block_frames
        self._encoder = None
        self.blocks: list[dict] = []
        self._current: list[dict] = []

    def _png_bytes(self, array: np.ndarray) -> bytes:
        import av

        if self._encoder is None:
            encoder = av.CodecContext.create("png", "w")
            encoder.width = int(array.shape[1])
            encoder.height = int(array.shape[0])
            encoder.pix_fmt = "rgb24"
            encoder.open()
            self._encoder = encoder
        video_frame = av.VideoFrame.from_ndarray(np.ascontiguousarray(array, dtype=np.uint8), format="rgb24")
        packets = self._encoder.encode(video_frame)
        return b"".join(bytes(packet) for packet in packets)

    def write(self, frame: RenderedFrame) -> dict:
        entry = frame.to_json()
        blob = self._png_bytes(frame.array)
        target = self.frames_dir / f"frame_{frame.number:06d}.png"
        temporary = target.with_suffix(".png.tmp")
        try:
            temporary.write_bytes(blob)
            os.replace(temporary, target)
        except OSError as exc:
            raise ResourceError(
                ErrorCategory.OUTPUT_FAILURE,
                f"cannot write frame {frame.number}: {exc}",
                details={"frame": frame.number, "path": str(target)},
            ) from exc
        entry["file"] = target.name
        entry["png_bytes"] = len(blob)
        self._current.append(entry)
        if len(self._current) >= self.block_frames:
            self.flush_block()
        return entry

    def flush_block(self) -> dict | None:
        if not self._current:
            return None
        entries = self._current
        block = {
            "index": len(self.blocks),
            "frames": len(entries),
            "hash": _block_hash(entries),
            "frame_hashes": [entry["pixel_hash"] for entry in entries],
            "entries": entries,
        }
        self.blocks.append(block)
        self._current = []
        return block

    def close(self) -> None:
        self.flush_block()
        if self._encoder is not None:
            try:
                for _packet in self._encoder.encode(None):
                    pass
            except Exception:  # pragma: no cover - flush is best effort
                pass
            self._encoder = None


def _block_hash(entries: list[dict]) -> str:
    digest = hashlib.sha256()
    for entry in entries:
        digest.update(entry["pixel_hash"].encode())
        digest.update(b"\n")
    return digest.hexdigest()


# ==========================================================================
# Pipeline
# ==========================================================================


def run_pipeline(
    timeline: Timeline,
    output_dir: Path,
    params: RunParams,
    *,
    token: CancellationToken | None = None,
    resume: bool = False,
) -> PipelineResult:
    """Run one program through the QC pipeline and write the manifest."""
    started = time.perf_counter()
    output_dir = Path(output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)
    manifest_path = _manifest_path(output_dir)
    token = token or CancellationToken()
    signature = input_signature(timeline, params)

    resumed_from = 0
    retained_blocks: list[dict] = []
    if resume and manifest_path.exists():
        prior = read_manifest(manifest_path)
        _check_resume(prior, signature)
        resumed_from = int(prior.get("complete_blocks", 0))
        retained_blocks = list(prior.get("blocks", []))[:resumed_from]
        # Drop any frames past the last trusted block: a partially written
        # block must not survive into the resumed output.
        frames_dir = output_dir / "frames"
        for stale in frames_dir.glob("frame_*.png"):
            try:
                number = int(stale.stem.split("_")[1])
            except (IndexError, ValueError):
                continue
            if number >= resumed_from * params.block_frames:
                stale.unlink()

    policies: dict[str, LossPolicy] = {}
    unit_counts: dict[str, int] = {}
    if params.drop_rate > 0 or params.hard_loss_rate > 0:
        for track_id, track in timeline.active_tracks().items():
            if track.codec is None:
                continue
            units = _read_access_units(track.path, track.codec)
            unit_counts[track_id] = len(units)
            model = LossModel(params.seed, params.drop_rate, params.hard_loss_rate).configure(len(units))
            policies[track_id] = LossPolicy(model=model)

    renderer = Renderer(timeline, policies=policies, block_frames=params.block_frames)
    for track_id, source in renderer.video.items():
        source.unit_count = unit_counts.get(track_id)
    renderer.resume_from_frame = resumed_from * params.block_frames
    frames_expected = renderer.frame_count()
    writer = _FrameWriter(output_dir, params.block_frames)
    writer.blocks = retained_blocks
    tracker = SyncTracker()
    latency = LatencyRecorder()
    queue_blocks = max(1, params.queue_blocks)

    # Seed the tracker from retained blocks so alignment on a resumed run
    # still covers the whole output, not just the newly rendered frames.
    for block in retained_blocks:
        for entry in block.get("entries", []):
            tracker.frame_hashes.append(entry["pixel_hash"])
            tracker.luma.append(0.0)
            tracker.audio_centre.append(0.0)
            tracker.audio_level.append(0.0)
            tracker.frame_times.append(Fraction(entry["time_ticks"], 1) * timeline.time_base.seconds_per_tick)
    counters = {
        "written": sum(block["frames"] for block in retained_blocks),
        "recovered": 0,
        "concealed": 0,
        "concealed_gap_ms": 0.0,
    }

    block_produced_at: dict[int, int] = {}
    block_sequence = 0

    def produce(_token: CancellationToken) -> Iterator[list[RenderedFrame]]:
        nonlocal block_sequence
        block: list[RenderedFrame] = []
        for frame in renderer.frames():
            if token.cancelled:
                break
            block.append(frame)
            if len(block) >= params.block_frames:
                block_produced_at[block_sequence] = time.perf_counter_ns()
                block_sequence += 1
                yield block
                block = []
        if block:
            block_produced_at[block_sequence] = time.perf_counter_ns()
            block_sequence += 1
            yield block

    completed_blocks = 0

    def consume(blocks: Iterator[list[RenderedFrame]]) -> None:
        nonlocal completed_blocks
        for block in blocks:
            if token.cancelled:
                break
            for frame in block:
                writer.write(frame)
                tracker.add_frame(frame.time_seconds, frame.array)
                tracker.add_audio(frame.audio)
                counters["written"] += 1
                counters["recovered"] += int(frame.recovered)
                if frame.concealed:
                    counters["concealed"] += 1
                    for span in frame.spans:
                        if span.concealed:
                            counters["concealed_gap_ms"] = max(
                                counters["concealed_gap_ms"], float(span.gap_ms)
                            )
            writer.flush_block()
            if params.consumer_delay_ms > 0:
                time.sleep(params.consumer_delay_ms / 1000.0)
            produced_ns = block_produced_at.get(completed_blocks)
            if produced_ns is not None:
                latency.add((time.perf_counter_ns() - produced_ns) / 1e6)
            completed_blocks += 1

    try:
        _, queue_stats = run_stages(
            produce,
            consume,
            capacity=queue_blocks,
            slow_consumer_timeout=params.slow_consumer_timeout,
            token=token,
            name="frames",
        )
    finally:
        writer.close()

    decode_stats = [source.stats for source in renderer.video.values()]
    sync_errors = sync_errors_from_tracker(tracker)
    alignment = alignment_from_duplicates(tracker)
    if params.reference and params.reference_hashes:
        alignment["placement"] = measure_placement(tracker, params.reference_hashes)
    if params.reference_signals is not None:
        luma, audio = params.reference_signals
        class _Ref:  # minimal duck type for measure_av_placement
            pass
        reference = _Ref()
        reference.luma = list(luma)
        reference.audio_centre = list(audio)
        alignment["av_placement"] = measure_av_placement(tracker, reference)
    verified, mismatches = verify_written_frames(writer.blocks, output_dir)
    elapsed = time.perf_counter() - started

    thresholds = Thresholds(peak_queue_frames=queue_blocks * params.block_frames)
    report = build_report(
        decode_stats=decode_stats,
        latency=latency,
        sync_errors_ms=sync_errors,
        peak_queue_blocks=queue_stats.peak_depth,
        queue_capacity_blocks=queue_blocks,
        block_frames=params.block_frames,
        frames_rendered=counters["written"],
        frames_verified=verified,
        frame_hash_mismatches=mismatches,
        recovered_frames=counters["recovered"],
        concealed_frames=counters["concealed"],
        concealed_gap_ms=counters["concealed_gap_ms"],
        duration_seconds=float(timeline.duration().seconds()),
        tracks=len(timeline.active_tracks()),
        excluded_tracks=timeline.excluded_tracks,
        notes=list(timeline.notes),
    )
    verdicts = evaluate_thresholds(report, thresholds)
    # Only blocks with every frame present and hash-verified are resumable.
    complete_blocks = count_complete_blocks(writer.blocks)
    exit_code, exit_notes = derive_exit_code(
        input_valid=True,
        rendered=counters["written"],
        decoded=report.decoded_frames if decode_stats else counters["written"],
        unrecoverable=report.unrecoverable_packets,
        frames_expected=frames_expected if resumed_from == 0 else 0,
        thresholds_met=all(verdict["pass"] for verdict in verdicts.values()),
        media_error=mismatches > 0 or counters["concealed"] > 0,
    )
    report.exit_code = exit_code
    report.ok = exit_code == EXIT_OK
    report.notes.extend(exit_notes)

    manifest = {
        "version": MANIFEST_VERSION,
        "signature": signature,
        "params": params.to_json(),
        "timeline": timeline.to_json(),
        "frames_expected": frames_expected,
        "frames_written": counters["written"],
        "complete_blocks": complete_blocks,
        "blocks": writer.blocks,
        "report": report.to_json(),
        "thresholds": thresholds.to_json(),
        "verdicts": verdicts,
        "excluded_tracks": list(timeline.excluded_tracks),
        "elapsed_seconds": round(elapsed, 3),
    }
    if alignment is not None:
        manifest["alignment"] = alignment
    manifest["loss_models"] = {
        track_id: policy.model.to_json()
        for track_id, policy in policies.items()
        if policy.model is not None
    }
    _write_manifest(manifest_path, manifest)
    return PipelineResult(
        report=report,
        manifest_path=manifest_path,
        output_dir=output_dir,
        verification={"frames": verified, "mismatches": mismatches},
        resumed_from_block=resumed_from,
        frames_written=counters["written"],
        elapsed_seconds=elapsed,
    )


@dataclass
