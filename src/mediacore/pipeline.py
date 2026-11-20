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
from collections import deque
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from . import clock as clockmod
from .clock import TimelineRepair
from .audio import AudioAnalysis, align_audio
from .bitstream import AccessUnit, ScanReport, scan
from .checkpoint import (
    Checkpoint, compact_digest, fingerprint_inputs, write_json_atomic,
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
    payload_bytes_released: int = 0


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
                queue_depth: int = QUEUE_MAX_UNITS * 4,
                keep_first: int = 0, tail_window: int = 32) -> StageResult:
    """Scan a raw bitstream fed through a bounded producer/consumer queue.

    The reader thread blocks when the queue is full; the scanner is the only
    consumer, so read-ahead is bounded by ``queue_depth``.

    Access-unit *metadata* is collected (needed for indexing and the manifest),
    but payloads are dropped as soon as they can no longer be needed:

    * the first ``keep_first`` units are kept for quality scoring,
    * the last ``tail_window`` units are kept for the strict tail probe.

    Everything else, keyframes included, is released.  Keyframe payloads are
    only needed for random-access observation, and that stage calls
    :meth:`AccessUnit.payload`, which re-reads the exact byte range from the
    source file.  Peak resident payload is therefore bounded by the two windows
    plus the metadata list, not by fixture duration.
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
    retained = 0
    released = 0
    tail: deque[AccessUnit] = deque()
    try:
        for item in scan(chunks(), codec=codec):
            if isinstance(item, ScanReport):
                report = item
                continue
            item.source_path = str(path)
            units.append(item)
            tail.append(item)
            if len(tail) > tail_window:
                old = tail.popleft()
                if old.index >= keep_first and old.data:
                    released += old.release_payload()
            retained = max(retained, sum(len(u.data) for u in units))
    finally:
        thread.join(timeout=10.0)
    if producer_error:
        raise producer_error[0]
    if report is None:
        raise MediaInputError("scanner produced no report", code="scan_failed")
    for unit in tail:
        if unit.index >= keep_first and unit.data:
            released += unit.release_payload()
    result = StageResult(
        units=units, report=report, scan_seconds=time.perf_counter() - started,
        queue_stats=queue.stats(), peak_read_buffer_bytes=peak[0])
    result.payload_bytes_released = released
    return result


def analyze_wav_queued(path: Path, *, block_frames: int,
                       queue_depth: int = QUEUE_MAX_AUDIO_BLOCKS):
    """Run audio analysis with a bounded queue between reader and analyser.

    A producer thread reads PCM blocks with ``soundfile`` and blocks when the
    queue is full; the analysis stage consumes them directly.  The queue depth
    (``QUEUE_MAX_AUDIO_BLOCKS``) is therefore the ceiling on how much decoded
    PCM can be resident at once, independent of clip length.
    """
    from .audio import analyze_wav

    queue: BoundedQueue[Any] = BoundedQueue(queue_depth, name="audio")
    error: list[BaseException] = []

    def producer() -> None:
        try:
            import soundfile as sf

            for block in sf.blocks(str(path), blocksize=block_frames,
                                   dtype="float64", always_2d=True):
                queue.put(block)
        except BaseException as exc:
            error.append(exc)
        finally:
            queue.close()

    thread = __import__("threading").Thread(
        target=producer, name="mediacore-audio-reader", daemon=True)
    thread.start()

    def blocks():
        while True:
            try:
                item = queue.get(timeout=30.0)
            except TimeoutError as exc:
                raise MediaInputError(
                    "audio reader stalled", code="reader_stalled") from exc
            if item is None:
                return
            yield item

    try:
        analysis = analyze_wav(path, block_frames=block_frames,
                               collect_block_starts=0, block_iter=blocks())
    finally:
        thread.join(timeout=10.0)
    if error:
        raise error[0]
    return analysis, queue.stats()


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


def _units_digest(units: list[AccessUnit]) -> str:
    """Digest of the scan block's metadata, stored in the checkpoint commit."""
    return compact_digest(b"mediacore-scan-block-v1\x00", [
        [u.index, u.offset, u.size, u.keyframe, u.is_irap, u.complete,
         u.truncated, list(u.nal_types)]
        for u in units
    ])


def _persist_scan(out_dir: Path, stage: StageResult) -> str:
    """Persist scan metadata (no payloads); returns the block digest."""
    digest = _units_digest(stage.units)
    write_json_atomic(out_dir / "scan.json", {
        "digest": digest,
        "report": stage.report.to_dict(),
        "units": [unit.metadata() for unit in stage.units],
        "payload_bytes_released": stage.payload_bytes_released,
        "tail_probe_truncated": bool(
            getattr(stage.report, "tail_probe_truncated", False)),
    })
    return digest


def _restore_scan(out_dir: Path, video_path: Path,
                  expected_digest: str | None) -> tuple[StageResult | None, str]:
    """Restore a committed scan block, re-attaching the live source path.

    Returns ``(stage, "")`` on success or ``(None, reason)`` when the record is
    missing, incomplete or fails its digest.  A bad block is never used: the
    caller rescans and replaces it, so a partial write cannot be mistaken for a
    completed stage.
    """
    if not expected_digest:
        return None, "checkpoint has no scan-block digest"
    path = out_dir / "scan.json"
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
        units = [AccessUnit.from_metadata(item) for item in raw["units"]]
        report = ScanReport.from_dict(raw["report"])
    except (OSError, json.JSONDecodeError, KeyError, TypeError, ValueError) as exc:
        return None, f"scan block unreadable: {exc}"
    if not units or len(units) != int(report.units):
        return None, "scan block does not match its report"
    if raw.get("digest") != expected_digest or _units_digest(units) != expected_digest:
        return None, "scan block digest mismatch"
    size = video_path.stat().st_size
    previous_start = -1
    for position, unit in enumerate(units):
        unit.source_path = str(video_path)
        if unit.index != position:
            return None, f"scan block unit {position} has index {unit.index}"
        # Units are contiguous but may overlap by the zero padding that a
        # 4-byte start code shares with the previous unit's trailing zero.
        if unit.offset < previous_start or unit.offset + unit.size > size:
            return None, (f"scan block unit {unit.index} has an invalid byte "
                          f"range ({unit.offset}..{unit.offset + unit.size})")
        previous_start = unit.offset
    report.tail_probe_truncated = bool(raw.get("tail_probe_truncated", False))
    return StageResult(
        units=units, report=report, scan_seconds=0.0,
        queue_stats={}, peak_read_buffer_bytes=0,
        payload_bytes_released=int(raw.get("payload_bytes_released", 0))), ""


def _restore_quality(out_dir: Path, limit: int, unit_count: int):
    """Reuse a previously computed quality record for the same limits.

    Quality is a pure function of the decoded prefix and the deterministic
    reference frames; the record carries the frame count it was produced from,
    so a changed limit or a changed unit count forces recomputation.
    """
    path = out_dir / "derived.json"
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None
    quality = raw.get("quality")
    if not isinstance(quality, dict):
        return None
    if int(raw.get("quality_limit", -1)) != int(limit):
        return None
    if int(quality.get("frames", -1)) != min(limit, unit_count):
        return None
    return quality


def _restore_derived(out_dir: Path, units: list[AccessUnit], file_size: int,
                     expected_index_digest: str | None):
    """Restore derived.json/index.json if they describe exactly this unit set.

    Returns ``(result, "")`` or ``(None, reason)``.  The index's digest is
    checked against the one recorded in the checkpoint commit, and every index
    offset must still lie inside the input, so an incompletely written or stale
    artifact is recomputed rather than trusted.
    """
    if not expected_index_digest:
        return None, "checkpoint has no index digest"
    derived_path = out_dir / "derived.json"
    index_path = out_dir / "index.json"
    try:
        derived = json.loads(derived_path.read_text(encoding="utf-8"))
        index_raw = json.loads(index_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        return None, f"derived artifacts unreadable: {exc}"
    actual = hashlib.sha256(
        json.dumps(index_raw, sort_keys=True).encode()).hexdigest()
    if actual != expected_index_digest:
        return None, "index digest mismatch"
    try:
        timeline = TimelineRepair.from_dict(derived["timeline"])
        audio = AudioAnalysis.from_dict(derived["audio"])
        alignment = dict(derived["alignment"])
        ra_index = RandomAccessIndex.from_dict(index_raw)
    except (KeyError, TypeError, ValueError) as exc:
        return None, f"derived artifacts malformed: {exc}"
    if len(timeline.decisions) != len(units):
        return None, "timeline length does not match the scanned units"
    for entry in ra_index.entries:
        if entry.offset + entry.size > file_size:
            return None, f"index offset {entry.offset} lies past the input end"
    return (timeline, ra_index, audio, alignment), ""


def run_pipeline(
    video_path: str | Path,
    audio_path: str | Path,
    out_dir: str | Path,
    config: PipelineConfig,
    *,
    index_payload: Any | None = None,
) -> PipelineResult:
    """Run all stages for one fixture, checkpointing each committed boundary."""
    video_path, audio_path, out_dir = Path(video_path), Path(audio_path), Path(out_dir)
    if not video_path.exists():
        raise MediaInputError(f"video input not found: {video_path}", code="input_missing")
    if not audio_path.exists():
        raise MediaInputError(f"audio input not found: {audio_path}", code="input_missing")
    out_dir.mkdir(parents=True, exist_ok=True)
    checkpoint_path = out_dir / "checkpoint.json"
    index_path = out_dir / "index.json"
    manifest_path = out_dir / "manifest.json"
    derived_path = out_dir / "derived.json"

    inputs = fingerprint_inputs([video_path, audio_path])
    cfg = config.to_dict()
    checkpoint = Checkpoint.load(checkpoint_path, inputs=inputs, config=cfg)
    resumed = checkpoint is not None
    if checkpoint is None:
        checkpoint = Checkpoint.new(checkpoint_path, inputs=inputs, config=cfg)
        for stale in (index_path, derived_path, manifest_path):
            if stale.exists():
                stale.unlink()
        checkpoint.save()

    timings: dict[str, float] = {}
    budget = ByteBudget(config.memory_limit)
    interrupted_at = config.interrupt_after
    file_size = video_path.stat().st_size

    # ---- Stage 1: scan -----------------------------------------------------
    stage = scan_stream(video_path, config.codec,
                        keep_first=config.quality_limit)
    timings["scan"] = stage.scan_seconds
    units = stage.units
    for unit in units:
        unit.source_path = str(video_path)
    if not units:
        raise MediaCorruptError(
            "no access units could be recovered from the input",
            offset=0, code="no_units")
    tail_truncated, tail_reason, tail_unit = decode_tail_probe(units, config.codec)
    if tail_truncated:
        # Structural flags (cut mid-NAL, missing OBU bytes) already name an
        # offset; a strict-decode failure is attributed to the unit's own offset.
        if stage.report.truncated_tail_offset is None:
            bad = units[tail_unit] if tail_unit is not None and tail_unit < len(units) else units[-1]
            stage.report.truncated_tail_offset = bad.offset
            stage.report.truncated_tail_reason = tail_reason
            bad.complete = False
            bad.truncated = True
            from .errors import MediaIssue
            stage.report.issues.append(MediaIssue(
                "truncated_tail", tail_reason, bad.offset,
                detail={"unit_index": bad.index, "size": bad.size}))
    if stage.peak_read_buffer_bytes > budget.limit:
        raise MediaInputError(
            f"read buffer {stage.peak_read_buffer_bytes} exceeded memory limit",
            code="memory_limit")
    scan_commit = checkpoint.get("scan")
    if scan_commit is None:
        checkpoint.commit(
            "scan", unit_start=0, unit_end=len(units),
            byte_start=0, byte_end=file_size, records=len(units),
            payload={"codec": config.codec, "units": len(units)})
        checkpoint.last_status = "scan_committed"
        checkpoint.save()
        if interrupted_at and interrupted_at <= 1:
            raise Interrupted(
                "interrupted after scan stage; re-run to resume",
                code="interrupted_scan")

    # ---- Stage 2: timeline repair -----------------------------------------
    timeline = repair_timeline(units, fps=config.fps)
    tb = timeline.time_base
    pts_ticks = [d.fixed for d in timeline.decisions]
    pts_seconds = [tb.seconds(t) for t in pts_ticks]
    frame_duration = 1.0 / config.fps
    durations = [frame_duration] * len(units)

    # ---- Stage 3: index ----------------------------------------------------
    ra_index = random_access_index(
        units, time_base=tb, supplied=index_payload, pts_of=pts_ticks,
        durations=durations, file_size=file_size)
    observed, observe_problems = _observe_keyframes(ra_index, units, config.codec)
    ra_index.seeks = verify_seeks(
        ra_index.entries, durations,
        targets_ms=[e.pts_seconds * 1000.0 for e in ra_index.entries],
        observed_ms=observed)
    if observe_problems:
        from .errors import MediaIssue

        ra_index.issues.extend(
            MediaIssue("keyframe_observation_failed", message, 0,
                       detail={"observation": True})
            for message in observe_problems)

    # ---- Stage 4: audio ----------------------------------------------------
    audio_started = time.perf_counter()
    audio, audio_queue_stats = analyze_wav_queued(
        audio_path, block_frames=config.audio_block_frames,
        queue_depth=QUEUE_MAX_AUDIO_BLOCKS)
    timings["audio"] = time.perf_counter() - audio_started
    alignment = align_audio(audio, [float(x) for x in pts_seconds], tolerance_ms=40.0)

    index_payload_out = ra_index.to_dict()
    derived_payload = {
        "timeline": [d.to_dict(tb) for d in timeline.decisions
                     if d.reason != "ok" or d.uncertain],
        "segments": None,  # filled below
        "audio": audio.to_dict(),
        "alignment": alignment,
    }
    derive_commit = checkpoint.get("derive")
    if derive_commit is None:
        write_json_atomic(index_path, index_payload_out)
        write_json_atomic(derived_path, derived_payload)
        checkpoint.commit(
            "derive", unit_start=0, unit_end=len(units),
            byte_start=0, byte_end=file_size, records=len(units),
            payload={
                "index_digest": hashlib.sha256(
                    json.dumps(index_payload_out, sort_keys=True).encode()).hexdigest(),
                "audio_frames": audio.frames,
                "align_ms": round(alignment["max_alignment_ms"], 3),
            })
        checkpoint.last_status = "derive_committed"
        checkpoint.save()
        if interrupted_at and interrupted_at <= 2:
            raise Interrupted(
                "interrupted after derive stage; re-run to resume",
                code="interrupted_derive")

    # ---- Stage 5: quality and manifest ------------------------------------
    quality_started = time.perf_counter()
    quality_limit = min(config.quality_limit, len(units))
    quality = score_quality(
        units, config.codec, _reference_lookup(config), limit=quality_limit,
        cache_bytes=FRAME_CACHE_BYTES).to_dict()
    timings["quality"] = time.perf_counter() - quality_started

    # Payloads are no longer needed: the scanner metadata, timeline records and
    # measured statistics are all that the manifest derives from.  Dropping them
    # keeps resident memory flat for long fixtures (values stay re-readable
    # through AccessUnit.payload() if a future stage needs them).
    released = stage.payload_bytes_released + sum(
        unit.release_payload() for unit in units)
    segments = build_segments(
        units, pts_seconds, target_bps=config.target_bps,
        frame_duration_s=frame_duration, decisions=timeline.decisions)
    derived_payload["segments"] = [s.to_dict() for s in segments]
    write_json_atomic(index_path, index_payload_out)
    write_json_atomic(derived_path, derived_payload)

    actual_bps = file_size * 8.0 / config.duration_s
    deviation = (actual_bps - config.target_bps) / config.target_bps * 100.0
    decisions = [
        {"unit_index": i, **d.to_dict(tb)}
        for i, d in enumerate(timeline.decisions)
        if d.reason != "ok" or d.uncertain
    ]
    issues = [i.to_dict() for i in stage.report.issues]
    issues += [i.to_dict() for i in timeline.issues]
    issues += [i.to_dict() for i in ra_index.issues]

    run_material = hashlib.sha256(
        json.dumps([cfg, [f.digest for f in inputs], len(units)], sort_keys=True).encode()
    ).hexdigest()
    run_id = f"{config.codec}-{run_material[:12]}"
    checkpoint.commit(
        "manifest", unit_start=0, unit_end=len(units),
        byte_start=0, byte_end=file_size, records=len(segments),
        payload={"run_id": run_id, "segments": len(segments)})
    checkpoint.last_status = "manifest_committed"
    checkpoint.save()

    resource = budget.stats()
    resource.update({
        "memory_limit_bytes": config.memory_limit,
        "read_queue_max_items": QUEUE_MAX_UNITS * 4,
        "read_queue_high_water": stage.queue_stats.get("high_water", 0),
        "read_queue_blocked_puts": stage.queue_stats.get("blocked_puts", 0),
        "scan_peak_read_buffer_bytes": stage.peak_read_buffer_bytes,
        "frame_cache_bytes": FRAME_CACHE_BYTES,
        "audio_queue_max_blocks": QUEUE_MAX_AUDIO_BLOCKS,
        "audio_queue_high_water": audio_queue_stats.get("high_water", 0),
        "audio_queue_blocked_puts": audio_queue_stats.get("blocked_puts", 0),
        "payload_bytes_released": released,
        "python": platform.python_version(),
    })
    manifest = Manifest(
        run_id=run_id, codec=config.codec,
        time_base={"num": tb.num, "den": tb.den},
        target_bps=config.target_bps, actual_bps=actual_bps,
        deviation_pct=deviation,
        video={
            "path": str(video_path),
            "bytes": file_size,
            "units": len(units),
            "keyframes": sum(1 for u in units if u.keyframe),
            "fps": config.fps, "width": config.width, "height": config.height,
            "sha256": _sha256_file(video_path),
            "truncated_tail_offset": stage.report.truncated_tail_offset,
            "truncated_tail_reason": stage.report.truncated_tail_reason,
            "tail_verified": not tail_truncated,
        },
        audio=audio.to_dict(),
        segments=segments, index=index_payload_out,
        timeline=timeline.summary(),
        quality={"video": quality, "audio": {
            "rms": audio.rms, "peak": audio.peak,
            "silent_fraction": audio.silent_fraction}},
        alignment=alignment, inputs=[f.to_dict() for f in inputs],
        checkpoint={
            "path": str(checkpoint_path),
            "input_digest": checkpoint.input_digest,
            "config_digest": checkpoint.config_digest,
            "stages": sorted(checkpoint.stages()),
            "runs": checkpoint.runs,
            "resumed": resumed,
        },
        decisions=decisions, issues=issues, resource=resource,
        duration_s=config.duration_s)
    manifest.save(str(manifest_path))

    return PipelineResult(
        manifest=manifest, index=ra_index, checkpoint=checkpoint,
        resumed=resumed, run_id=run_id, scan=stage.report, audio=audio,
        timings=timings, resource=resource)


def _reference_lookup(config: PipelineConfig):
    from .fixtures import FixtureSpec, make_frame

    spec = FixtureSpec(
        codec=config.codec, duration_s=config.duration_s,
        bitrate=config.target_bps, seed=config.seed,
        width=config.width, height=config.height,
        fps=config.fps, gop=config.gop)

    # A one-entry memo keeps only the reference currently being compared with
    # the decoded picture, so scoring stays bounded for long fixtures.
    memo: dict[int, object] = {}

    def lookup(unit_index: int):
        if unit_index not in memo:
            memo.clear()
            memo[unit_index] = make_frame(unit_index, spec)[0]
        return memo[unit_index]

    return lookup
