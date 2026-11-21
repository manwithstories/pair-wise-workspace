"""Bounded-queue pipeline: scan -> repair -> journal, then mux/index/verify.

A producer thread streams packets out of the container into a bounded
queue; the consumer repairs timestamps and appends them to the on-disk
journal, committing a checkpoint every COMMIT_PACKETS packets.  Backpressure
time and peak occupancy are measured, cancellation propagates through a
shared token, and a global budget bounds in-flight packets across files.
"""

from __future__ import annotations

import queue
import threading
import time
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

import av

from .checkpoint import (
    Checkpoint,
    CheckpointStore,
    compute_fingerprint,
    sha256_file,
    truncate_to,
    verify_fingerprint,
)
from .errors import ErrorCode, MediaError, Severity, WriteFailure
from .index import build_index, verify_index, write_index
from .journal import JournalRecord, JournalWriter, read_journal
from .probe import ProbeResult, check_truncation, scan
from .repair import TrackRepairer, TrackRepairStats, check_av_skew
from .timebase import rescale, to_seconds

COMMIT_PACKETS = 128
_SENTINEL = object()


class _Cancelled(Exception):
    pass


class CancelToken:
    def __init__(self) -> None:
        self._event = threading.Event()

    def cancel(self) -> None:
        self._event.set()

    @property
    def cancelled(self) -> bool:
        return self._event.is_set()


class MemoryBudget:
    """Global bound on in-flight packets across all concurrent pipelines."""

    def __init__(self, packets: int = 256):
        self._sem = threading.Semaphore(packets)

    def acquire(self) -> None:
        self._sem.acquire()

    def release(self) -> None:
        self._sem.release()


class BackpressureQueue:
    """Bounded queue that measures producer blocking and peak occupancy."""

    def __init__(self, maxsize: int, budget: MemoryBudget):
        self._q: queue.Queue = queue.Queue(maxsize)
        self._budget = budget
        self.blocked_seconds = 0.0
        self.peak = 0

    def put(self, item: object, cancel: CancelToken) -> None:
        self._budget.acquire()
        start = time.monotonic()
        try:
            while True:
                try:
                    self._q.put(item, timeout=0.05)
                    break
                except queue.Full:
                    if cancel.cancelled:
                        raise _Cancelled()
        finally:
            self.blocked_seconds += time.monotonic() - start
        self.peak = max(self.peak, self._q.qsize())

    def get(self, cancel: CancelToken) -> object:
        while True:
            try:
                item = self._q.get(timeout=0.05)
                self._budget.release()
                return item
            except queue.Empty:
                if cancel.cancelled:
                    raise _Cancelled()


@dataclass
class FileResult:
    path: Path
    ok: bool = False
    unrepairable: bool = False
    cancelled: bool = False
    errors: list[MediaError] = field(default_factory=list)
    track_stats: list[TrackRepairStats] = field(default_factory=list)
    packets_scanned: int = 0
    repaired_timestamps: int = 0
    index_entries: int = 0
    backpressure_seconds: float = 0.0
    peak_queue_packets: int = 0
    resumed_from_packet: int = 0
    resume_seconds: float = 0.0
    output_sha256: str | None = None
    output_path: Path | None = None
    index_path: Path | None = None


def mux_journal(journal_path: Path, input_path: Path, output_path: Path) -> None:
    """Mux journalled packets into the final MP4 in one deterministic pass."""
    src = av.open(str(input_path))
    out = av.open(str(output_path), "w")
    stream_map: dict[int, tuple] = {}
    try:
        for s in src.streams:
            if s.type in ("video", "audio"):
                stream_map[s.index] = (s, out.add_stream(template=s))
        for item in read_journal(journal_path):
            if isinstance(item, dict):
                continue
            in_s, out_s = stream_map[item.stream_index]
            tb = Fraction(out_s.time_base)
            pkt = av.Packet(item.payload)
            pkt.stream = out_s
            pkt.time_base = tb
            pkt.pts = rescale(item.pts, Fraction(1), tb) if item.pts is not None else None
            pkt.dts = rescale(item.dts, Fraction(1), tb) if item.dts is not None else None
            pkt.duration = rescale(item.duration, Fraction(1), tb)
            pkt.is_keyframe = item.is_keyframe
            out.mux(pkt)
    except (OSError, av.error.FFmpegError) as exc:
        raise WriteFailure(f"muxing {output_path} failed: {exc}") from exc
    finally:
        out.close()
        src.close()


def process_file(
    input_path: Path,
    output_path: Path,
    store: CheckpointStore,
    queue_size: int,
    budget: MemoryBudget,
    cancel: CancelToken,
    key: str | None = None,
) -> FileResult:
    """Run the full pipeline for one input file, resuming if possible."""
    key = key or input_path.stem
    result = FileResult(path=input_path, output_path=output_path)
    started = time.monotonic()
    fingerprint = compute_fingerprint(input_path)
    journal_path = output_path.with_suffix(".journal")
    checkpoint = store.load(key)
    skip_packets = 0
    if checkpoint is not None:
        verify_fingerprint(input_path, checkpoint.fingerprint)  # raises -> exit 3
        if checkpoint.stage == "done" and Path(checkpoint.output_path).exists():
            result.ok = True
            result.output_sha256 = checkpoint.output_sha256
            result.resumed_from_packet = checkpoint.committed_packets
            result.resume_seconds = time.monotonic() - started
            result.index_path = output_path.with_suffix(".midx")
            if result.index_path.exists():
                result.index_entries = len(build_index(output_path).entries)
            return result
        skip_packets = checkpoint.committed_packets
        truncate_to(journal_path, checkpoint.committed_bytes)
        result.resumed_from_packet = skip_packets
    else:
        journal_path.unlink(missing_ok=True)
        checkpoint = Checkpoint(
            input_path=str(input_path),
            output_path=str(output_path),
            temp_output_path=str(journal_path),
            fingerprint=fingerprint,
        )
    result.resume_seconds = time.monotonic() - started
    bq: BackpressureQueue = BackpressureQueue(queue_size, budget)
    probe_result = ProbeResult(input_path)
    last_end: list[Fraction | None] = [None]

    def produce() -> None:
        try:
            for event in scan(input_path, probe_result):
                if cancel.cancelled:
                    raise _Cancelled()
                end = to_seconds((event.pts or 0) + event.duration, event.track.time_base)
                if end is not None and (last_end[0] is None or end > last_end[0]):
                    last_end[0] = end
                bq.put(event, cancel)
        except _Cancelled:
            pass
        finally:
            while True:
                try:
                    bq.put(_SENTINEL, cancel)
                    break
                except _Cancelled:
                    # Consumer is gone; nothing more to signal.
                    break

    producer = threading.Thread(target=produce, daemon=True)
    producer.start()
    repairers: dict[int, TrackRepairer] = {}
    last_pts: dict[str, Fraction] = {}
    committed = skip_packets
    try:
        with JournalWriter(journal_path, header={"input": str(input_path)} if skip_packets == 0 else None) as journal:
            while True:
                item = bq.get(cancel)
                if item is _SENTINEL:
                    break
                event = item
                if event.global_index < skip_packets:
                    continue
                track = event.track
                repairer = repairers.get(track.index)
                if repairer is None:
                    repairer = repairers[track.index] = TrackRepairer(
                        track.kind, track.time_base, track.rate
                    )
                repaired, errors = repairer.process(event)
                result.errors.extend(errors)
                if repaired.pts is not None:
                    other = "audio" if track.kind.value == "video" else "video"
                    skew = check_av_skew(
                        repaired.pts if track.kind.value == "video" else last_pts.get("video"),
                        repaired.pts if track.kind.value == "audio" else last_pts.get("audio"),
                        event.packet_index,
                        event.byte_offset,
                    )
                    if skew is not None and len(result.errors) < 500:
                        result.errors.append(skew)
                    last_pts[track.kind.value] = repaired.pts
                committed_bytes = journal.append(
                    JournalRecord(
                        stream_index=track.index,
                        kind=track.kind.value,
                        pts=repaired.pts,
                        dts=repaired.dts,
                        duration=repaired.duration,
                        is_keyframe=event.is_keyframe,
                        payload=bytes(event.raw) if event.raw is not None else b"",
                    )
                )
                committed += 1
                if committed % COMMIT_PACKETS == 0:
                    checkpoint.committed_packets = committed
                    checkpoint.committed_bytes = committed_bytes
                    checkpoint.stage = "mux"
                    store.save(key, checkpoint)
    except _Cancelled:
        result.cancelled = True
        producer.join(timeout=2)
        return result
    producer.join()
    result.packets_scanned = probe_result.packets_scanned
    result.errors.extend(probe_result.errors)
    check_truncation(probe_result, last_end[0])
    if probe_result.errors and probe_result.errors[-1] not in result.errors:
        result.errors.append(probe_result.errors[-1])
    for repairer in repairers.values():
        repairer.finish()
        result.track_stats.append(repairer.stats)
        result.repaired_timestamps += repairer.stats.repaired_timestamps
    result.backpressure_seconds = bq.blocked_seconds
    result.peak_queue_packets = bq.peak
    result.unrepairable = any(
        e.severity in (Severity.UNREPAIRABLE, Severity.FATAL) for e in result.errors
    )
    if result.unrepairable:
        return result
    # Mux, index, verify, publish.
    output_path.parent.mkdir(parents=True, exist_ok=True)
    mux_journal(journal_path, input_path, output_path)
    media_index = build_index(output_path)
    result.index_entries = len(media_index.entries)
    index_path = output_path.with_suffix(".midx")
    write_index(media_index, index_path)
    result.index_path = index_path
    verify_errors = verify_index(output_path, media_index)
    if verify_errors:
        result.errors.extend(verify_errors)
        result.unrepairable = True
        return result
    result.output_sha256 = sha256_file(output_path)
    checkpoint.stage = "done"
    checkpoint.committed_packets = committed
    checkpoint.committed_bytes = journal_path.stat().st_size
    checkpoint.output_sha256 = result.output_sha256
    store.save(key, checkpoint)
    journal_path.unlink(missing_ok=True)
    result.ok = True
    return result
