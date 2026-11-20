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

    def _abort(self) -> None:
        if self.index_writer is not None:
            self.index_writer.abort()
            self.index_writer = None
        if self.checkpoint is not None:
            self.checkpoint.state = "aborted"
            try:
                save_checkpoint(self.config.checkpoint_path, self.checkpoint)
            except Exception:
                pass

    # -- core ------------------------------------------------------------
    def _validate_paths(self) -> None:
        """Refuse to overwrite the source or touch anything outside the cwd tree.

        Media archives are frequently symlinked and shared across nodes, so a
        repair must never write over its own input, and a path that resolves
        outside the user's working tree is treated as a mistake (typo or
        attempt at path escape) rather than silently honoured.
        """
        source = Path(self.config.source).resolve()
        output = Path(self.config.output).resolve()
        if source == output:
            raise StructuralError(
                "repaired output would overwrite the source file",
                context={"source": str(source), "output": str(output)},
            )
        if source.exists() and output.exists():
            if os.path.samefile(source, output):
                raise StructuralError(
                    "repaired output is a hard link to the source file",
                    context={"source": str(source), "output": str(output)},
                )

    def _run(self) -> None:
        cfg = self.config
        self._validate_paths()
        fingerprint = probe.file_fingerprint(cfg.source)
        probe_result = probe.probe(cfg.source)

        checkpoint: Checkpoint | None = None
        resume = cfg.resume
        if resume:
            checkpoint = load_checkpoint(cfg.checkpoint_path)
            index_meta = None
            if cfg.index_path.exists():
                try:
                    with IndexReader(cfg.index_path, require_complete=False) as reader:
                        index_meta = reader.meta()
                except IndexIncompatibleError:
                    index_meta = None
            validate_resume(checkpoint, fingerprint=fingerprint,
                            profile=cfg.profile, tool_version=TOOL_VERSION,
                            index_version=INDEX_VERSION, index_meta=index_meta)
            if index_meta is None:
                raise IndexIncompatibleError(
                    "resume requires a partially committed index",
                    context={"index": str(cfg.index_path)},
                )
            committed = output_matches(checkpoint, cfg.output)
            truncate_output(cfg.output, committed)
            self.resume_offset_bytes = committed
            self.summary.resume_start_packet = checkpoint.cursor.get("next_global_packet")
            if checkpoint.cursor.get("resume_seconds") is not None:
                self.summary.resume_seconds = float(load_fraction(checkpoint.cursor["resume_seconds"]))
        else:
            # A fresh run owns its index.  Reusing an index built for another
            # source/run would mix committed chunks from two repairs, so an
            # existing index is only replaced when it is ours to replace.
            existing_index = Path(cfg.index_path)
            if existing_index.exists():
                try:
                    with IndexReader(existing_index) as reader:
                        recorded = reader.info().source
                except IndexIncompatibleError:
                    recorded = None
                if recorded and recorded.get("digest") not in (
                        None, fingerprint["digest"]):
                    raise IndexIncompatibleError(
                        "index belongs to a different source; pass a fresh --index path "
                        "or delete it before rebuilding",
                        context={"index": str(existing_index),
                                 "indexed_source": recorded.get("path")},
                    )
                existing_index.unlink()
            output_path = Path(cfg.output)
            if output_path.exists():
                output_path.unlink()

        # Build the planner and discover output timescales.
        container = probe.open_container(cfg.source)
        try:
            streams = list(container.streams)
            planner = ChunkPlanner(probe_result, streams, cfg.profile,
                                   chunk_packets=cfg.chunk_packets,
                                   detect_damage=cfg.detect_damage,
                                   consumer_delay=cfg.consumer_delay,
                                   audio_index_stride=cfg.audio_index_stride)
            timescales = self._discover_timescales(probe_result, streams, cfg)
            planner.set_timescales(timescales)

            self.summary.packets_total = probe_result.packet_count

            if resume and checkpoint is not None:
                self._prepare_resume(planner, checkpoint, timescales)
            else:
                self._prepare_fresh(planner, probe_result, fingerprint, timescales)

            self._consume(container, planner)
        finally:
            container.close()

    # -- setup helpers ----------------------------------------------------
    def _discover_timescales(self, probe_result, streams, cfg):
        """Probe the muxer's output timescales with two real packets.

        Encoding must happen while the source container is still open: an
        ``av.Packet`` keeps a reference to its ``AVStream``, and using it
        after the container is closed is a use-after-free (observed as a
        segfault or a bogus ``stream.index``).
        """
        container = probe.open_container(cfg.source)
        try:
            sample: list[av.Packet] = []
            for info, packet in probe.iter_packets(container, probe_result.tracks):
                if packet is not None and len(sample) < 2:
                    packet.time_base = info.time_base
                    sample.append(packet)
                if len(sample) >= 2:
                    break
            encoder = ChunkEncoder(streams)
            return discover_timescales(encoder, sample)
        finally:
            container.close()

    def _prepare_fresh(self, planner: ChunkPlanner, probe_result, fingerprint, timescales):
        writer = IndexWriter(self.config.index_path)
        writer.set_meta(
            index_version=INDEX_VERSION,
            tool_version=TOOL_VERSION,
            state="running",
            source=json.dumps(fingerprint, sort_keys=True),
            profile=json.dumps(self.config.profile.as_dict(), sort_keys=True),
            timescales=json.dumps(timescales, sort_keys=True),
        )
        writer.add_tracks(probe_result.tracks, timescales)
        self.index_writer = writer
        self.checkpoint = Checkpoint(
            tool_version=TOOL_VERSION,
            source=fingerprint,
            profile=self.config.profile.as_dict(),
            output={"path": str(self.config.output), "committed_bytes": 0},
            cursor={"chunk_index": 0, "next_global_packet": 0, "completed": False},
            clocks={},
            state="running",
        )

    def _prepare_resume(self, planner: ChunkPlanner, checkpoint: Checkpoint, timescales):
        writer = IndexWriter.open_existing(self.config.index_path)
        self.index_writer = writer
        chunk_index = int(checkpoint.cursor.get("chunk_index", 0))
        packet_seq = {int(k): int(v) for k, v in checkpoint.cursor.get("packet_seq", {}).items()}
        origin_raw = checkpoint.cursor.get("origin")
        origin = load_fraction(origin_raw) if origin_raw is not None else None
        committed_samples = 0
        committed_damage: list[DamageRow] = []
        try:
            with IndexReader(self.config.index_path, require_complete=False) as reader:
                committed_samples = reader.info().sample_count
                for row in reader.damage():
                    committed_damage.append(DamageRow(
                        track_index=int(row["track_index"]),
                        start_packet=int(row["start_packet"]),
                        end_packet=int(row["end_packet"]),
                        reason=row["reason"],
                        dts=row["dts"],
                        pts=row["pts"],
                        notes=json.loads(row["notes"]) if row["notes"] else None,
                    ))
        except IndexIncompatibleError:
            committed_samples = 0
        planner.set_resume_state(
            chunk_index=chunk_index,
            packet_seq=packet_seq,
            origin=origin,
            samples_written=0,
            samples_offset=committed_samples,
            first_error_packet=checkpoint.cursor.get("first_error_packet"),
            clocks=checkpoint.clocks,
            # Damage already in the index must stay in the operator summary;
            # the resumed run only records what it newly finds.
            damage=committed_damage,
        )
        self.summary.first_error_packet = checkpoint.cursor.get("first_error_packet")
        self._prior_dropped = int(checkpoint.cursor.get("packets_dropped", 0))
        self.checkpoint = checkpoint
        checkpoint.state = "running"

    # -- consumer ---------------------------------------------------------
    def _consume(self, container, planner: ChunkPlanner) -> None:
        cfg = self.config
        q: "queue.Queue" = queue.Queue(maxsize=max(1, cfg.queue_size))
        cancel = threading.Event()
        producer = _Producer(container, planner.probe.tracks, q, cancel, self._note_depth)
        producer.start()

        output = open(cfg.output, "r+b" if cfg.resume else "wb")
        if cfg.resume:
            output.seek(0, os.SEEK_END)
        else:
            output.seek(0)
        committed_bytes = self.resume_offset_bytes
        # The init segment (``ftyp``+``moov``) is written exactly once, with
        # chunk 0.  On resume it is only absent when no chunk was committed,
        # i.e. the committed byte boundary is still zero.
        first_chunk = committed_bytes == 0
        consumed = 0
        interrupted = False
        acked = 0

        try:
            while True:
                try:
                    item = q.get(timeout=0.1)
                except queue.Empty:
                    if producer.error is not None or not producer.is_alive():
                        break
                    if cancel.is_set():
                        break
                    continue
                if item is _Producer._STOP:
                    break
                info, packet = item
                if packet is None:
                    # Demuxer-level corruption: report and continue.
                    planner.ingest(info, None)
                    consumed += 1
                    continue
                acked = info.global_index
                if cfg.interrupt_after is not None and acked >= cfg.interrupt_after:
                    cancel.set()
                    interrupted = True
                    break
                if self._should_skip(planner, info):
                    consumed += 1
                    continue
                chunks = planner.ingest(info, packet)
                consumed += 1
                for chunk in chunks:
                    committed_bytes = self._commit_chunk(chunk, output, committed_bytes,
                                                         first_chunk, planner)
                    first_chunk = False
            if producer.error is not None:
                raise StructuralError(f"demux failed: {producer.error}") from producer.error
            if not interrupted:
                for chunk in planner.finish():
                    committed_bytes = self._commit_chunk(chunk, output, committed_bytes,
                                                         first_chunk, planner)
                    first_chunk = False
        finally:
            cancel.set()
            try:
                while True:
                    q.get_nowait()
            except queue.Empty:
                pass
            producer.join(timeout=2.0)
            output.flush()
            os.fsync(output.fileno())
            output.close()

        self.summary.packets_dropped = planner.packets_dropped + self._prior_dropped
        self.summary.damage = [self._damage_dict(row) for row in planner.all_damage()]
        if planner.first_error_packet is not None:
            self.summary.first_error_packet = planner.first_error_packet

        if interrupted:
            self._persist_interruption(planner, committed_bytes, acked)
            self.summary.status = "interrupted"
            self.summary.exit_code = EXIT_OK
            if self.index_writer is not None:
                self.index_writer.finish(state="partial")
                self.index_writer = None
            return

        self._finalise(planner, committed_bytes)

