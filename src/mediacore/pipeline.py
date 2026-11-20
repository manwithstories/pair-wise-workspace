"""Pipeline wiring: read -> scan -> clock repair -> index/audio -> manifest.

Stages communicate through bounded queues on a producer thread, so a slow
consumer blocks the reader instead of buffering more of the stream.  Only the
access units needed by the current stage are in flight; the timeline repair
keeps scalar records (not payloads), and per-segment aggregates are derived
from those records.  Peak resident *unread* file data is therefore bounded by
the configured queue depth, independent of fixture duration.

Checkpoint semantics (see :mod:`mediacore.checkpoint`):

* ``scan`` is committed after every access unit has been scanned;
* ``derive`` is committed after timeline repair, index rebuild and audio
  analysis, and the derived artifacts are written atomically right after it;
* ``manifest`` is committed after the manifest is written;
* ``interrupt_after`` stops after that many committed stages by raising
  :class:`Interrupted`, which the CLI maps to exit code 3;
* re-running with identical inputs and config regenerates the same derived data
  deterministically, so the manifest digest is unchanged (see
  :func:`manifest_digest_payload`).
"""

from __future__ import annotations

import hashlib
import json
import platform
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from . import clock as clockmod
from .audio import AudioAnalysis, align_audio, analyze_wav
from .bitstream import AccessUnit, ScanReport, scan
from .checkpoint import (
    Checkpoint, fingerprint_inputs, write_json_atomic,
)
from .decode import decode_keyframe, decode_tail_probe, score_quality
from .errors import (
    Interrupted, MediaCorruptError, MediaInputError, MEMORY_LIMIT_BYTES,
)
from .index import RandomAccessIndex, random_access_index, verify_seeks
from .manifest import Manifest, build_segments
from .queues import BoundedQueue, ByteBudget

#: Raw-stream chunk size for the reader stage (bytes).
READ_CHUNK = 64 * 1024
#: Maximum access units' worth of chunks buffered between reader and scanner.
QUEUE_MAX_UNITS = 8
#: Maximum audio blocks buffered between the soundfile reader and analysis.
QUEUE_MAX_AUDIO_BLOCKS = 8
#: Maximum decoded picture bytes retained for quality scoring.
FRAME_CACHE_BYTES = 24 * 1024 * 1024


@dataclass
class PipelineConfig:
    """Everything that participates in the checkpoint configuration digest."""

    codec: str = "h264"
    target_bps: int = 900_000
    gop: int = 15
    fps: int = 30
    seed: int = 41
    width: int = 320
    height: int = 180
    duration_s: int = 12
    audio_block_frames: int = 4096
    quality_limit: int = 90
    memory_limit: int = MEMORY_LIMIT_BYTES
    interrupt_after: int = 0
    corrupt_tail: bool = False

    def to_dict(self) -> dict[str, Any]:
        return {
            "codec": self.codec,
            "target_bps": self.target_bps,
            "gop": self.gop,
            "fps": self.fps,
            "seed": self.seed,
            "width": self.width,
            "height": self.height,
            "duration_s": self.duration_s,
            "audio_block_frames": self.audio_block_frames,
            "quality_limit": self.quality_limit,
            "memory_limit": self.memory_limit,
            "read_chunk": READ_CHUNK,
            "queue_max_units": QUEUE_MAX_UNITS,
            "frame_cache_bytes": FRAME_CACHE_BYTES,
            # interrupt_after / corrupt_tail are deliberately excluded: they
            # change how a run stops or which fixture is cut, not the derived
            # data, and a resumed run must accept a different interrupt point.
        }


@dataclass
class StageResult:
    units: list[AccessUnit]
    report: ScanReport
    scan_seconds: float = 0.0
    queue_stats: dict[str, Any] = field(default_factory=dict)
    peak_read_buffer_bytes: int = 0


@dataclass
class PipelineResult:
    manifest: Manifest
    index: RandomAccessIndex
    checkpoint: Checkpoint
    resumed: bool = False
    run_id: str = ""
    scan: ScanReport | None = None
    audio: AudioAnalysis | None = None
    timings: dict[str, float] = field(default_factory=dict)
    resource: dict[str, Any] = field(default_factory=dict)


def _sha256_file(path: Path) -> str:
    hasher = hashlib.sha256()
    with open(path, "rb") as handle:
        for block in iter(lambda: handle.read(1 << 20), b""):
            hasher.update(block)
    return hasher.hexdigest()


def scan_stream(path: Path, codec: str, *, chunk: int = READ_CHUNK,
                queue_depth: int = QUEUE_MAX_UNITS * 4) -> StageResult:
    """Scan a raw bitstream fed through a bounded producer/consumer queue.

    The reader thread blocks when the queue is full; the scanner is the only
    consumer.  Access units are collected because later stages need their bytes
    for decoding, but the *file read-ahead* is bounded by ``queue_depth``.
    """
    started = time.perf_counter()
    queue: BoundedQueue[bytes] = BoundedQueue(queue_depth, name="read")
    producer_error: list[BaseException] = []
    peak = [0]

    def producer() -> None:
        try:
            with open(path, "rb") as handle:
                while True:
                    data = handle.read(chunk)
                    if not data:
                        break
                    queue.put(data)
        except BaseException as exc:  # re-raised on the consumer side
            producer_error.append(exc)
        finally:
            queue.close()

    thread = __import__("threading").Thread(
        target=producer, name="mediacore-reader", daemon=True)
    thread.start()

    def chunks():
        pending = bytearray()
        while True:
            try:
                item = queue.get(timeout=30.0)
            except TimeoutError as exc:
                raise MediaInputError(
                    "reader stalled while scanning", code="reader_stalled") from exc
            if item is None:
                break
            pending += item
            peak[0] = max(peak[0], len(pending))
            while len(pending) >= chunk:
                yield bytes(pending[:chunk])
                del pending[:chunk]
        if pending:
            yield bytes(pending)

    units: list[AccessUnit] = []
    report: ScanReport | None = None
    try:
        for item in scan(chunks(), codec=codec):
            if isinstance(item, ScanReport):
                report = item
            else:
                units.append(item)
    finally:
        thread.join(timeout=10.0)
    if producer_error:
        raise producer_error[0]
    if report is None:
        raise MediaInputError("scanner produced no report", code="scan_failed")
    return StageResult(
        units=units, report=report, scan_seconds=time.perf_counter() - started,
        queue_stats=queue.stats(), peak_read_buffer_bytes=peak[0])


def repair_timeline(units: list[AccessUnit], *, fps: int):
    """Synthesise and repair presentation timestamps.

    Raw H.264/H.265 streams carry no timestamps, so one tick per access unit in
    scan order is the native timeline.  Repair still runs on that sequence so
    the same code path handles container/index timestamps that are reordered,
    duplicated, missing or wrapped.
    """
    raw = [unit.index for unit in units]
    return clockmod.repair_timestamps(
        raw, time_base=clockmod.TimeBase(1, fps), wrap_period=None,
        nominal_step=1, strict=False)


def _observe_keyframes(index: RandomAccessIndex, units: list[AccessUnit],
                       codec: str, *, limit: int = 8) -> tuple[list[float], list[str]]:
    """Decode up to ``limit`` keyframes and confirm the index targets them.

    Raw streams have no container PTS, so the observation is structural: the AU
    at the index offset must decode to a keyframe, and its stream position must
    equal the index's recorded unit.  A mismatch is reported with the offset.
    """
    observed: list[float] = []
    problems: list[str] = []
    by_offset = {unit.offset: unit for unit in units}
    for entry in index.entries[:limit]:
        unit = by_offset.get(entry.offset)
        if unit is None:
            problems.append(f"index offset {entry.offset} has no access unit")
            observed.append(entry.pts_seconds * 1000.0)
            continue
        if unit.index != entry.unit_index:
            problems.append(
                f"index unit {entry.unit_index} resolves to unit {unit.index}")
        picture = decode_keyframe(unit, codec)
        if picture is None:
            problems.append(f"keyframe at offset {entry.offset} produced no picture")
            observed.append(entry.pts_seconds * 1000.0)
            continue
        if not picture.key:
            problems.append(f"AU at offset {entry.offset} is not a keyframe")
        observed.append(entry.pts_seconds * 1000.0)
    return observed, problems


def run_pipeline(
    video_path: str | Path,
    audio_path: str | Path,
    out_dir: str | Path,
    config: PipelineConfig,
    *,
    index_payload: Any | None = None,
) -> PipelineResult:
    """Run all stages for one fixture (filled in by the follow-up patch)."""
    raise NotImplementedError("pipeline stages are added by the next patch")
