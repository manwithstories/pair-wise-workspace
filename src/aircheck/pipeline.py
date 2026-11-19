"""Staged pipeline: demux -> map -> write, with backpressure, cancel and resume.

Three stages run concurrently and are connected by :class:`BoundedQueue`
instances.  A full queue blocks its producer, so a slow writer stalls demuxing
instead of letting packets accumulate in memory; every queue records its peak
depth so the acceptance report can show the ceiling held.

Why three stages
----------------
The mapping stage is where per-packet policy lives: it drops streams not present
in the output mapping, renumbers packets onto output stream slots, and records
the timeline facts used by the acceptance report.  Keeping it separate from the
writer means the queue between them measures the real hand-off cost.

Resume semantics
----------------
A container cannot be appended to in-place, so a resumed run writes a
*continuation part* rather than patching the original file: ``name.part{N}.ext``
holds exactly the segments after the last committed boundary of the interrupted
run.  Each part is a complete, verifiable container, and the parts together
cover the source exactly once - no packet is written twice.

Checkpoints are written atomically after every run.  A segment is committed only
after the writer has accepted it and :data:`COMMIT_LAG_SEGMENTS` further segments
have passed, which absorbs packets whose timestamps sit just across a boundary.
"""

from __future__ import annotations

import json
import os
import threading
import time
from dataclasses import dataclass, field
from pathlib import Path

from .demux import DemuxedPacket, Demuxer
from .hashing import (
    HashComparison,
    SegmentHash,
    SegmentHasher,
    SegmentPlan,
    compare_hashes,
    compare_timeline,
)
from .mapping import Mapping, TimelineReport, TimelinePacket, validate_timeline
from .probe import ProbeResult
from .queueing import BoundedQueue, QueueStats
from .remux import RemuxWriter
from .timebase import (
    AircheckError,
    CancelledError,
    ExitCode,
    InputError,
    QueueOverflowError,
)

#: Segments kept open behind the demuxer to absorb boundary jitter.  A packet
#: whose timestamp sits a frame past a boundary can still arrive (containers
#: interleave streams), so a segment is only declared complete once demux has
#: advanced this many segments beyond it.
@dataclass(frozen=True)
class SegmentBoundary:
    """In-band marker: every packet of ``segment`` has been enqueued.

    Markers travel through the same FIFO queues as packets, so by the time the
    writer pops one, every packet the demuxer will ever emit for that segment
    has already been written.  That makes the resume boundary exact instead of
    inferred from timestamps or cross-stage counters.
    """

    segment: int

#: Default per-queue byte ceiling: 64 MiB of packet payload.
DEFAULT_QUEUE_BYTES = 64 * 1024 * 1024


def _queue_item_size(item: "DemuxedPacket | MappedPacket | SegmentBoundary") -> int:
    if isinstance(item, SegmentBoundary):
        return 0
    if isinstance(item, MappedPacket):
        return item.packet.size
    return item.size


@dataclass
class MappedPacket:
    """A packet with its stream renumbered to an output slot."""

    packet: DemuxedPacket
    output_stream: int


@dataclass
class PipelineConfig:
    segment_ticks: int
    target_format: str = "mkv"
    queue_size: int = 4
    queue_bytes: int = DEFAULT_QUEUE_BYTES
    slow_consumer_ms: int = 0
    resume: bool = False
    validate_timestamps: bool = True
    cut_points: list[int] = field(default_factory=list)
    transition_ms: int = 0


@dataclass
class StageResult:
    source: Path
    output: Path | None
    ok: bool
    duration_seconds: float = 0.0
    packets_in: int = 0
    packets_out: int = 0
    packets_skipped: int = 0
    segments: int = 0
    segment_hashes: list[SegmentHash] = field(default_factory=list)
    segment_plan: SegmentPlan | None = None
    hash_comparison: HashComparison | None = None
    timeline: TimelineReport | None = None
    timeline_drift: list[dict[str, object]] = field(default_factory=list)
    queue_stats: dict[str, QueueStats] = field(default_factory=dict)
    last_committed_segment: int = 0
    resumed_from_segment: int | None = None
    part_index: int = 0
    cancelled: bool = False
    errors: list[dict[str, object]] = field(default_factory=list)
    notes: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, object]:
        return {
            "source": str(self.source),
            "output": str(self.output) if self.output else None,
            "ok": self.ok,
            "duration_seconds": round(self.duration_seconds, 4),
            "packets_in": self.packets_in,
            "packets_out": self.packets_out,
            "packets_skipped": self.packets_skipped,
            "segments": self.segments,
            "segment_hashes": [item.to_dict() for item in self.segment_hashes],
            "segment_plan": self.segment_plan.to_dict() if self.segment_plan else None,
            "hash_comparison": self.hash_comparison.to_dict() if self.hash_comparison else None,
            "timeline": self.timeline.to_dict() if self.timeline else None,
            "timeline_drift": list(self.timeline_drift),
            "queues": {name: stats.to_dict() for name, stats in self.queue_stats.items()},
            "last_committed_segment": self.last_committed_segment,
            "resumed_from_segment": self.resumed_from_segment,
            "part_index": self.part_index,
            "cancelled": self.cancelled,
            "errors": list(self.errors),
            "notes": list(self.notes),
        }


@dataclass
class Checkpoint:
    """Durable resume state for one asset's output family."""

    source: Path
    base_output: Path
    segment_ticks: int
    last_committed_segment: int
    part_index: int
    packets_written: int
    complete: bool
    updated_at: float

    def to_dict(self) -> dict[str, object]:
        return {
            "source": str(self.source),
            "base_output": str(self.base_output),
            "segment_ticks": self.segment_ticks,
            "last_committed_segment": self.last_committed_segment,
            "part_index": self.part_index,
            "packets_written": self.packets_written,
            "complete": self.complete,
            "updated_at": self.updated_at,
        }

    @classmethod
    def from_dict(cls, payload: dict[str, object]) -> "Checkpoint":
        return cls(
            source=Path(str(payload["source"])),
            base_output=Path(str(payload["base_output"])),
            segment_ticks=int(payload["segment_ticks"]),
            last_committed_segment=int(payload["last_committed_segment"]),
            part_index=int(payload.get("part_index", 0)),
            packets_written=int(payload.get("packets_written", 0)),
            complete=bool(payload.get("complete", False)),
            updated_at=float(payload.get("updated_at", 0.0)),
        )


def checkpoint_path(base_output: Path) -> Path:
    return base_output.with_suffix(base_output.suffix + ".resume.json")


def part_path(base_output: Path, part_index: int) -> Path:
    if part_index == 0:
        return base_output
    return base_output.with_name(
        f"{base_output.stem}.part{part_index}{base_output.suffix}"
    )


def write_checkpoint(checkpoint: Checkpoint) -> Path:
    path = checkpoint_path(checkpoint.base_output)
    path.parent.mkdir(parents=True, exist_ok=True)
    temp = path.with_suffix(path.suffix + f".{os.getpid()}.tmp")
    temp.write_text(json.dumps(checkpoint.to_dict(), indent=2), encoding="utf-8")
    os.replace(temp, path)
    return path


def read_checkpoint(base_output: Path) -> Checkpoint | None:
    path = checkpoint_path(base_output)
    if not path.exists():
        return None
    try:
        return Checkpoint.from_dict(json.loads(path.read_text(encoding="utf-8")))
    except (OSError, ValueError, KeyError) as exc:
        raise InputError(
            "resume checkpoint is unreadable",
            path=str(path),
            error=str(exc),
        ) from exc


class Pipeline:
    """Run one asset through demux -> map -> write.

    ``cancel`` is shared with callers so an operator can interrupt a run; the
    blocked stage wakes, the queues drain, and the checkpoint records the last
    committed boundary.
    """

    def __init__(
        self,
        probe_result: ProbeResult,
        mapping: Mapping,
        base_output: Path,
        config: PipelineConfig,
        *,
        cancel: threading.Event | None = None,
    ) -> None:
        self.probe = probe_result
        self.mapping = mapping
        self.base_output = Path(base_output)
        self.config = config
        self.cancel = cancel or threading.Event()

        # Both queues carry packets *and* segment-boundary markers, so they are
        # typed on the union; the size function accounts for each shape.
        self._packet_queue: BoundedQueue[DemuxedPacket | SegmentBoundary] = BoundedQueue(
            config.queue_size,
            name="demux->map",
            capacity_bytes=config.queue_bytes,
            item_size=_queue_item_size,
            cancel=self.cancel,
        )
        self._mapped_queue: BoundedQueue[MappedPacket | SegmentBoundary] = BoundedQueue(
            config.queue_size,
            name="map->write",
            capacity_bytes=config.queue_bytes,
            item_size=_queue_item_size,
            cancel=self.cancel,
        )

        self._error: BaseException | None = None
        self._error_lock = threading.Lock()
        self._committed = -1
        self._commit_lock = threading.Lock()
        self._max_written_segment = -1
        self._packets_out = 0
        self._notes: list[str] = []
        self._held_segments: list[int] = []
        self._held_packets = 0

        self.output: Path = self.base_output
        self.part_index = 0
        self.resumed_from: int | None = None

    # -- public ----------------------------------------------------------- #

    def run(self) -> StageResult:
        started = time.monotonic()
        resume_from = 0
        checkpoint: Checkpoint | None = None
        if self.config.resume:
            checkpoint = read_checkpoint(self.base_output)
            if checkpoint is not None:
                self.resumed_from = checkpoint.last_committed_segment + 1
                resume_from = checkpoint.last_committed_segment + 1
                self.part_index = checkpoint.part_index + 1
            else:
                self._notes.append("no checkpoint found; started from the beginning")
        self.output = part_path(self.base_output, self.part_index)

        result = StageResult(
            source=self.probe.path,
            output=None,
            ok=False,
            resumed_from_segment=self.resumed_from,
            part_index=self.part_index,
        )
        if self.output.exists():
            error = InputError(
                "refusing to overwrite an existing output file",
                path=str(self.output),
            )
            self._record_error(error)
            result.errors.append(error.to_dict())
            result.duration_seconds = time.monotonic() - started
            return result

        writer = RemuxWriter(
            source=self.probe,
            mapping=self.mapping,
            target_format=self.config.target_format,
            path=self.output,
        )
        try:
            writer.open()
        except AircheckError as exc:
            self._record_error(exc)
            result.errors.append(exc.to_dict())
            result.duration_seconds = time.monotonic() - started
            return result

        demuxer = Demuxer(
            probe=self.probe,
            segment_ticks=self.config.segment_ticks,
            resume_from_segment=resume_from,
            validate_timestamps=self.config.validate_timestamps,
        )
        threads = [
            threading.Thread(target=self._demux_stage, args=(demuxer,), name="aircheck-demux"),
            threading.Thread(target=self._map_stage, name="aircheck-map"),
            threading.Thread(target=self._write_stage, args=(writer,), name="aircheck-write"),
        ]
        for thread in threads:
            thread.daemon = True
            thread.start()
        try:
            for thread in threads:
                while thread.is_alive():
                    thread.join(timeout=0.05)
        except KeyboardInterrupt:
            self.cancel.set()
            self._close_queues()
            for thread in threads:
                thread.join()
        writer.close()
        self._close_queues()

        cancelled = self.cancel.is_set()
        result.duration_seconds = time.monotonic() - started
        result.output = self.output if self.output.exists() else None
        result.packets_in = demuxer.stats.packets + demuxer.stats.skipped_before
        result.packets_skipped = demuxer.stats.skipped_before
        result.packets_out = self._packets_out
        result.queue_stats = {
            "demux->map": self._packet_queue.stats,
            "map->write": self._mapped_queue.stats,
        }
        result.last_committed_segment = max(self._committed, resume_from - 1)
        result.notes = list(self._notes)
        if self._held_packets:
            result.notes.append(
                f"discarded {self._held_packets} packets from uncommitted "
                f"segments {self._held_segments} on stop; a resume re-emits them"
            )
        result.cancelled = cancelled and self._error is None
        result.segments = max(0, self._max_written_segment + 1)

        error = self._error
        if error is not None:
            result.errors.append(_error_dict(error))
            result.ok = False
        elif cancelled:
            if self._committed < resume_from - 1:
                # Nothing was durably complete in this run.  Remove the
                # partial part so a later resume does not mistake it for data.
                try:
                    self.output.unlink(missing_ok=True)
                    result.output = None
                except OSError as exc:
                    self._notes.append(f"partial part could not be removed: {exc}")
            result.errors.append(
                {
                    "category": "cancelled",
                    "message": "processing cancelled; checkpoint records the last committed segment",
                    "exit_code": int(ExitCode.UNRECOVERABLE),
                    "context": {"last_committed_segment": result.last_committed_segment},
                }
            )
            result.ok = False
        else:
            result.ok = True

        # Keep a durable resume point for any run that wrote something, even if
        # it later failed; a clean success marks the part complete.
        if self._packets_out or error is None or cancelled:
            self._persist_checkpoint(result, complete=not cancelled and error is None)
        return result

    # -- stages ----------------------------------------------------------- #

    def _demux_stage(self, demuxer: Demuxer) -> None:
        try:
            for item in demuxer.iter_items(cancel=self.cancel):
                if isinstance(item, int):
                    self._packet_queue.put(SegmentBoundary(item))
                else:
                    self._packet_queue.put(item)
        except CancelledError:
            pass
        except BaseException as exc:  # noqa: BLE001 - relayed via _record_error
            self._record_error(exc)
        finally:
            self._packet_queue.close()

    def _map_stage(self) -> None:
        output_for = {item.input_index: item.output_index for item in self.mapping.assignments}
        try:
            while True:
                try:
                    item = self._packet_queue.get()
                except QueueOverflowError:
                    return
                if isinstance(item, SegmentBoundary):
                    self._mapped_queue.put(item)
                    continue
                output_stream = output_for.get(item.stream_index)
                if output_stream is None:
                    # Unmapped stream: counted, not written, never silently
                    # mistaken for a write failure.
                    continue
                self._mapped_queue.put(MappedPacket(packet=item, output_stream=output_stream))
        except CancelledError:
            return
        except BaseException as exc:  # noqa: BLE001
            self._record_error(exc)
        finally:
            self._mapped_queue.close()

    def _write_stage(self, writer: RemuxWriter) -> None:
        """Write packets, committing only whole segments.

        Packets are held per segment until that segment's boundary marker
        arrives.  Only then are they written and the segment committed: the
        marker is the demuxer's promise that no further packet for that segment
        exists.  Committing earlier would let a cancellation checkpoint name a
        segment whose packets were still in flight, and a resume would then
        duplicate them.

        On cancellation or error the uncommitted hold is *discarded*.  The part
        file therefore ends exactly on the last committed boundary, and the
        checkpoint resumes immediately after it with no overlap.
        """
        pending: dict[int, list[MappedPacket]] = {}
        try:
            while True:
                try:
                    item = self._mapped_queue.get()
                except QueueOverflowError:
                    break
                if isinstance(item, SegmentBoundary):
                    self._flush_through(writer, pending, item.segment)
                    continue
                if self.config.slow_consumer_ms:
                    # Emulate a slow downstream device: the packet has already
                    # been counted against the queue ceiling before this cost.
                    time.sleep(self.config.slow_consumer_ms / 1000.0)
                pending.setdefault(item.packet.segment, []).append(item)
        except CancelledError:
            pass
        except BaseException as exc:  # noqa: BLE001
            self._record_error(exc)
        if pending:
            self._held_segments = sorted(pending)
            self._held_packets = sum(len(items) for items in pending.values())

    def _flush_through(
        self,
        writer: RemuxWriter,
        pending: dict[int, list[MappedPacket]],
        segment: int,
    ) -> None:
        for ready in sorted(name for name in pending if name <= segment):
            self._flush(writer, pending, ready, commit=True)
            if self._error is not None:
                return

    def _flush(
        self,
        writer: RemuxWriter,
        pending: dict[int, list[MappedPacket]],
        segment: int,
        *,
        commit: bool,
    ) -> None:
        for mapped in pending.pop(segment, []):
            try:
                writer.write(mapped.packet)
            except AircheckError as exc:
                self._record_error(exc)
                return
            self._packets_out += 1
        if commit:
            self._commit(segment)

    # -- bookkeeping ------------------------------------------------------ #

    def _record_error(self, exc: BaseException) -> None:
        with self._error_lock:
            if self._error is None:
                self._error = exc
        self.cancel.set()
        self._close_queues()

    def _close_queues(self) -> None:
        self._packet_queue.close()
        self._mapped_queue.close()

    def _commit(self, segment: int) -> None:
        """Record a segment as fully written once its boundary marker arrives.

        The demuxer already lagged the marker by
        :data:`aircheck.demux.BOUNDARY_LAG_SEGMENTS`, so a packet whose
        timestamp landed a frame across the boundary is guaranteed to have been
        queued ahead of it.
        """
        with self._commit_lock:
            if segment > self._max_written_segment:
                self._max_written_segment = segment
            if segment > self._committed:
                self._committed = segment

    def _persist_checkpoint(self, result: StageResult, *, complete: bool) -> None:
        checkpoint = Checkpoint(
            source=self.probe.path,
            base_output=self.base_output,
            segment_ticks=self.config.segment_ticks,
            last_committed_segment=result.last_committed_segment,
            part_index=self.part_index,
            packets_written=self._packets_out,
            complete=complete,
            updated_at=time.time(),
        )
        try:
            write_checkpoint(checkpoint)
        except OSError as exc:
            result.notes.append(f"checkpoint could not be written: {exc}")


def _error_dict(error: BaseException) -> dict[str, object]:
    if isinstance(error, AircheckError):
        return error.to_dict()
    return {
        "category": "internal",
        "message": str(error),
        "exit_code": int(ExitCode.UNRECOVERABLE),
        "context": {"type": type(error).__name__},
    }


def hash_demuxed(
    packets: list[DemuxedPacket],
) -> tuple[list[SegmentHash], SegmentPlan]:
    hasher = SegmentHasher()
    for packet in packets:
        hasher.update(packet)
    return hasher.finish(), hasher.plan()


def hash_demuxed_with_plan(
    packets: list[DemuxedPacket], plan: SegmentPlan
) -> list[SegmentHash]:
    hasher = SegmentHasher(plan=plan)
    for packet in packets:
        hasher.update(packet)
    return hasher.finish()


def compare_roundtrip(
    source_packets: list[DemuxedPacket],
    output_packets: list[DemuxedPacket],
) -> tuple[HashComparison, list[dict[str, object]], SegmentPlan]:
    """Compare input/output packet streams for byte and timeline fidelity."""
    expected, plan = hash_demuxed(source_packets)
    actual = hash_demuxed_with_plan(output_packets, plan)
    comparison = compare_hashes(expected, actual)
    drift = [item.to_dict() for item in compare_timeline(source_packets, output_packets)]
    return comparison, drift, plan


def timeline_of(
    packets: list[DemuxedPacket],
    *,
    segment_ticks: int,
    duration_ticks: int,
    transition_ms: int = 0,
    cut_points: list[int] | None = None,
) -> TimelineReport:
    return validate_timeline(
        [
            TimelinePacket(
                stream_index=packet.stream_index,
                pts_ticks=packet.pts_ticks,
                dts_ticks=packet.dts_ticks,
                is_keyframe=packet.is_keyframe,
                segment=packet.segment,
            )
            for packet in packets
        ],
        segment_ticks=segment_ticks,
        duration_ticks=duration_ticks,
        transition_ms=transition_ms,
        cut_points=cut_points,
    )


__all__ = [
    "Checkpoint",
    "DEFAULT_QUEUE_BYTES",
    "MappedPacket",
    "Pipeline",
    "PipelineConfig",
    "StageResult",
    "checkpoint_path",
    "compare_roundtrip",
    "hash_demuxed",
    "hash_demuxed_with_plan",
    "part_path",
    "read_checkpoint",
    "timeline_of",
    "write_checkpoint",
]
