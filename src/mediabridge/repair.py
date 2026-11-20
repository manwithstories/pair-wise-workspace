"""Repair pipeline: bounded-queue demux, timeline repair, fragment commit.

Producer / consumer
-------------------
A producer thread demuxes packets and puts them on a bounded queue
(``queue_size``).  When the consumer is slower - it validates every packet
and muxes output fragments - the producer blocks in ``put``; that is the
backpressure knob operators tune.  Cancellation (``--interrupt-after``, a
consumer exception, or Ctrl-C) unblocks the producer via an event and records
the last acknowledged packet, so no thread keeps the container open.

Commit ordering
---------------
For every chunk: write the fragment bytes to the output, ``fsync``, commit
the index rows in one SQLite transaction, then atomically replace the
checkpoint.  A crash at any point leaves at most uncommitted output bytes,
which resume truncates away before appending.  This is what makes a resumed
run byte-identical to a full run.

Resume
------
``--resume`` validates source fingerprint, repair profile, tool and index
version, opens the existing index for appending, truncates the output to the
checkpointed committed length, restores clock state, and re-demuxes from the
beginning while skipping packets already committed.  Chunk boundaries depend
only on the packet stream, so the first chunk produced after resume is
exactly the chunk the full run would have produced.
"""

from __future__ import annotations

import hashlib
import json
import os
import queue
import threading
import time
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

import av

from . import probe
from .checkpoint import (
    Checkpoint,
    load_checkpoint,
    output_matches,
    save_checkpoint,
    truncate_output,
    validate_resume,
)
from .errors import (
    EXIT_INTERNAL,
    EXIT_OK,
    EXIT_UNRECOVERABLE,
    IndexIncompatibleError,
    IndexWriteError,
    MediaBridgeError,
    StructuralError,
    TimestampToleranceError,
    TruncatedInputError,
)
from .index import IndexWriter
from .index_read import IndexReader
from .index_types import (
    INDEX_VERSION,
    TOOL_VERSION,
    ChunkRecord,
    DamageRow,
    SampleRow,
    rows_digest,
)
from .mp4write import (
    ChunkEncoder,
    discover_timescales,
    iter_boxes,
    patch_durations,
    read_init_segment,
)
from .pipeline import ChunkPlanner, PlannedChunk
from .timeline import (
    TimelineProfile,
    dump_fraction,
    load_fraction,
    seconds,
    to_ticks,
)

#: Minimum packets a chunk must hold before a keyframe may close it.
MIN_CHUNK_PACKETS = 1


@dataclass
class RepairConfig:
    source: Path
    output: Path
    index_path: Path
    checkpoint_path: Path
    queue_size: int = 8
    chunk_packets: int = 128
    profile: TimelineProfile = field(default_factory=TimelineProfile)
    resume: bool = False
    interrupt_after: int | None = None
    consumer_delay: float = 0.0
    audio_index_stride: int = 16
    detect_damage: bool = True


@dataclass
class RepairSummary:
    command: str = "repair"
    status: str = "ok"
    exit_code: int = EXIT_OK
    source: str = ""
    output: str = ""
    index: str = ""
    checkpoint: str = ""
    packets_total: int = 0
    packets_written: int = 0
    packets_dropped: int = 0
    chunks_committed: int = 0
    index_rows: int = 0
    first_error_packet: int | None = None
    resume_start_packet: int | None = None
    resume_seconds: float | None = None
    peak_queue_depth: int = 0
    queue_size: int = 0
    output_sha256: str = ""
    elapsed_seconds: float = 0.0
    damaged_packets: int = 0
    damage: list[dict] = field(default_factory=list)
    timestamp_repairs: list[dict] = field(default_factory=list)
    warnings: list[str] = field(default_factory=list)
    error: dict | None = None

    def as_dict(self) -> dict:
        return {
            "command": self.command,
            "status": self.status,
            "exit_code": self.exit_code,
            "source": self.source,
            "output": self.output,
            "index": self.index,
            "checkpoint": self.checkpoint,
            "packets_total": self.packets_total,
            "packets_written": self.packets_written,
            "packets_dropped": self.packets_dropped,
            "chunks_committed": self.chunks_committed,
            "index_rows": self.index_rows,
            "first_error_packet": self.first_error_packet,
            "resume_start_packet": self.resume_start_packet,
            "resume_seconds": self.resume_seconds,
            "peak_queue_depth": self.peak_queue_depth,
            "queue_size": self.queue_size,
            "output_sha256": self.output_sha256,
            "elapsed_seconds": round(self.elapsed_seconds, 3),
            "damage": self.damage,
            "timestamp_repairs": self.timestamp_repairs,
            "warnings": self.warnings,
            "error": self.error,
        }


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for block in iter(lambda: handle.read(probe.HASH_CHUNK), b""):
            digest.update(block)
    return digest.hexdigest()


class _Producer(threading.Thread):
    """Demuxes on a background thread into a bounded queue."""

    _STOP = object()

    def __init__(self, container: "av.container.InputContainer",
                 tracks, queue: "queue.Queue", cancel: threading.Event,
                 on_depth):
        super().__init__(name="mediabridge-demux", daemon=True)
        self.container = container
        self.tracks = tracks
        self.queue = queue
        self.cancel = cancel
        self.on_depth = on_depth
        self.error: BaseException | None = None

    def run(self) -> None:
        try:
            for info, packet in probe.iter_packets(self.container, self.tracks):
                if self.cancel.is_set():
                    break
                while True:
                    if self.cancel.is_set():
                        return
                    try:
                        self.queue.put((info, packet), timeout=0.05)
                        break
                    except queue.Full:
                        continue
                self.on_depth(self.queue.qsize())
        except BaseException as exc:  # surface to consumer, never swallow
            self.error = exc
        finally:
            try:
                self.queue.put(self._STOP, timeout=1.0)
            except queue.Full:
                pass


class Repairer:
    """Run one repair (fresh or resumed) and return a machine-readable summary."""

    def __init__(self, config: RepairConfig):
        self.config = config
        self.summary = RepairSummary(
            source=str(config.source),
            output=str(config.output),
            index=str(config.index_path),
            checkpoint=str(config.checkpoint_path),
            queue_size=config.queue_size,
        )
        self.checkpoint: Checkpoint | None = None
        self.index_writer: IndexWriter | None = None
        self.resume_offset_bytes = 0
        self._peaks = 0
        self._lock = threading.Lock()
        self._resume_primed = False
        self._last_committed_global = 0
        self._last_committed_bytes = 0
        self._committed_clock_state: dict[int, dict] | None = None
        self._committed_packet_seq: dict[int, int] | None = None
        self._prior_dropped = 0

    # -- queue instrumentation -------------------------------------------
    def _note_depth(self, depth: int) -> None:
        with self._lock:
            if depth > self._peaks:
                self._peaks = depth

    # -- entry point ------------------------------------------------------
    def run(self) -> RepairSummary:
        started = time.monotonic()
        try:
            self._run()
        except MediaBridgeError as exc:
            self.summary.status = "failed"
            self.summary.exit_code = exc.exit_code
            self.summary.error = exc.as_dict()
            self._abort()
        except Exception as exc:  # unexpected - still machine readable
            self.summary.status = "failed"
            self.summary.exit_code = EXIT_INTERNAL
            self.summary.error = {
                "code": "internal_error",
                "message": f"{type(exc).__name__}: {exc}",
                "exit_code": EXIT_INTERNAL,
            }
            self._abort()
        self.summary.elapsed_seconds = time.monotonic() - started
        self.summary.peak_queue_depth = self._peaks
        return self.summary

