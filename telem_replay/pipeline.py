"""Streaming orchestrator: one pass, constant memory, crash-resumable.

The whole input is consumed with a single ``for line in file`` loop.  Nothing is
accumulated across lines except (a) in-flight fragments, which the reassembly
window bounds, and (b) state-machine views, which are capped by
``max_sessions``.  Peak RSS therefore does not grow with the size of the log.

Error containment
-----------------
Three independent layers, each with its own ``try/except``:

1. **parse** - malformed JSON / bad base64 / truncated frame  -> DLQ
2. **verify** - CRC mismatch                                   -> DLQ
3. **apply** - TLV or measurement errors during state folding  -> DLQ

A fault in one line never propagates out of the loop; each is counted and the
run continues.
"""

from __future__ import annotations

import base64
import io
import json
import os
import time
from typing import Any, Dict, Iterator, Optional, Tuple

from .checkpoint import Checkpoint
from .protocol import ProtocolError, parse_frame
from .reassembler import ReassemblyManager
from .state_machine import StateMachine

REQUIRED_FIELDS = ("session_id", "payload")


class Stats:
    """Mutable run counters.  Kept flat so checkpoints serialize trivially."""

    __slots__ = ("lines_read", "frames_committed", "corrupt", "duplicate",
                 "evicted", "warnings", "resumed", "bytes_read", "started",
                 "ended", "checkpoint_writes", "checkpoint_seconds",
                 "input_done", "reread", "lines_consumed", "noop")

    def __init__(self) -> None:
        self.lines_read = 0
        self.frames_committed = 0
        self.corrupt = 0
        self.duplicate = 0
        self.evicted = 0
        self.warnings = 0
        self.resumed = 0
        self.bytes_read = 0
        self.started = time.time()
        self.ended = 0.0
        self.checkpoint_writes = 0
        self.checkpoint_seconds = 0.0
        self.input_done = False
        #: Physical lines re-read because a resume rewound to a safe offset.
        #: ``lines_read`` counts every line touched, so this is what makes the
        #: accounting add up: lines_read - reread == distinct input lines.
        self.reread = 0
        #: Lines that advanced the reassembler (a complete frame, or a fragment
        #: held in the window).  Frames may span many lines, so this -- not
        #: frames_committed -- is the line-level counterpart.
        self.lines_consumed = 0
        #: True when a resume found the job already complete and unchanged.
        self.noop = False

    #: Keys that are meaningful to persist; ``input_done`` is tracked separately
    #: in the meta table and must not be carried in the cumulative counters.
    PERSISTED = ("lines_read", "frames_committed", "corrupt", "duplicate",
                 "evicted", "warnings", "resumed", "bytes_read", "reread",
                 "lines_consumed")

    def to_dict(self) -> Dict[str, int]:
        return {k: getattr(self, k) for k in self.PERSISTED}


class ReplayPipeline:
    """Drives parse -> reassemble -> state machine -> output for one run."""

    def __init__(self, input_path: str, *, state_path: str, output_path: str,
                 dlq_path: Optional[str], window: int = 256,
                 max_sessions: int = 64, resume: bool = False,
                 checkpoint_frames: int = 2000,
                 checkpoint_seconds: float = 30.0,
                 sync: str = "normal", progress_every: int = 0,
                 max_lines: int = 0) -> None:
        self.input_path = input_path
        self.output_path = output_path
        self.dlq_path = dlq_path
        self.window = window
        self.max_sessions = max_sessions
        self.resume = resume
        self.stats = Stats()
        self._progress_every = progress_every
        # Optional line budget for batched replay; 0 means "to end of input".
        self.max_lines = max_lines

        self.ckpt = Checkpoint(
            state_path,
            sync=sync,
            frame_interval=checkpoint_frames,
            time_interval=checkpoint_seconds,
        )
        self.reasm = ReassemblyManager(window_size=window, max_sessions=max_sessions)
        self.machine = StateMachine(max_sessions=max_sessions)

        self._out: Optional[io.BufferedWriter] = None
        self._dlq: Optional[io.TextIOWrapper] = None
        self._last_seq: Dict[str, int] = {}
        self.resume_offset = 0
        self._next_offset = 0
        self._resume_sessions = 0
        self._resume_output = 0
        self._resume_dlq = 0
        # Set by _iter_lines when the input is consumed to its true end.
        self._eof = False
        # True when a resume found the job already complete and unchanged.
        self._no_op = False
        # Highest input offset consumed so far, across all runs of this job.
        # A resumed run rewinds and re-reads everything below it, which is how
        # re-reads are told apart from forward progress.
        self._high_water = 0

    # -- output helpers ----------------------------------------------------

    def _open_outputs(self, append: bool) -> None:
        """Open the sinks, truncating to the checkpointed length when resuming.

        Between the last commit and a crash the pipeline may have appended more
        records than the checkpoint knows about.  Appending onto those would
        duplicate every such record, so the sinks are first cut back to exactly
        the lengths recorded in the same transaction as the session state.  That
        makes the output file a pure function of the last checkpoint.
        """
        if append:
            self._truncate(self.output_path, self._resume_output)
        self._out = open(self.output_path, "ab" if append else "wb", buffering=1 << 20)

        if self.dlq_path:
            # The DLQ is a diagnostic aid, not a correctness artifact, so it is
            # appended to across resumes rather than truncated: the corrupt-line
            # audit trail from earlier runs is worth keeping, and a missing file
            # is simply recreated rather than treated as an error -- losing the
            # audit trail must never be able to block a resume.
            self._dlq = open(self.dlq_path, "a" if append else "w",
                             encoding="utf-8", buffering=1 << 16)

    @staticmethod
    def _truncate(path: str, size: int) -> None:
        """Cut *path* back to *size* bytes, creating it if absent."""
        if not os.path.exists(path):
            if size:
                raise FileNotFoundError(f"{path} missing but checkpoint expects {size} bytes")
            open(path, "ab").close()
            return
        actual = os.path.getsize(path)
        if actual == size:
            return
        if actual < size:
            # The output was rotated or deleted underneath us; refuse to
            # silently append a divergent suffix.
            raise IOError(
                f"{path} is {actual} bytes but the checkpoint recorded {size}; "
                f"refusing to resume onto a different output"
            )
        with open(path, "r+b") as fh:
            fh.truncate(size)
            fh.flush()
            os.fsync(fh.fileno())

    def _close_outputs(self) -> None:
        if self._out is not None:
            self._out.flush()
            self._out.close()
            self._out = None
        if self._dlq is not None:
            self._dlq.flush()
            self._dlq.close()
            self._dlq = None

    def _emit(self, record: Dict[str, Any]) -> None:
        assert self._out is not None
        self._out.write(
            (json.dumps(record, separators=(",", ":"), ensure_ascii=False) + "\n")
            .encode("utf-8")
        )

    def _dead_letter(self, reason: str, raw: bytes, **extra: Any) -> None:
        self.stats.corrupt += 1
        if self._dlq is None:
            return
        rec: Dict[str, Any] = {"reason": reason, "offset": self.stats.bytes_read}
        rec.update(extra)
        # Keep a bounded preview: a corrupt line may be megabytes long and the
        # DLQ must not become a second copy of the input.
        rec["raw_preview"] = raw[:512].decode("utf-8", "replace")
        rec["raw_len"] = len(raw)
        self._dlq.write(json.dumps(rec, separators=(",", ":"), ensure_ascii=False) + "\n")

    # -- checkpoint plumbing ----------------------------------------------

    def _safe_offsets(self) -> Dict[str, int]:
        """Per-session resume points.

        For each session this is the start of the oldest line whose data is
        still only in memory.  Committing the live read cursor instead would
        skip fragments that were read but not yet committed into a frame, losing
        those frames after a crash.

        The per-session granularity is essential.  With sessions interleaved,
        a single global minimum taken over *all* in-flight fragments is unsafe:
        if session A holds an old fragment while session B is momentarily idle,
        the global minimum lands after B's unread lines and B's frames are lost
        entirely.  A session with nothing in flight is fully described by its
        ``last_seq``, so it records the current read position.
        """
        offsets: Dict[str, int] = {}
        for sid, sess in self.reasm.session_offsets().items():
            offsets[sid] = sess if sess is not None else self._next_offset
        return offsets

    def _safe_offset(self) -> int:
        """Global resume point: the oldest per-session safe offset."""
        offsets = self._safe_offsets()
        return min(offsets.values()) if offsets else self._next_offset

    def _snapshot_sessions(self) -> Dict[str, Tuple[int, int, Any]]:
        """Build the checkpoint payload from live state plus per-session offsets."""
        offsets = self._safe_offsets()
        rows: Dict[str, Tuple[int, int, Any]] = {}
        for sid, st in self.machine.states().items():
            rows[sid] = (st.last_seq, offsets.get(sid, self._next_offset), st.snapshot())
        for sid, last in self._last_seq.items():
            if sid not in rows:
                rows[sid] = (last, offsets.get(sid, self._next_offset), None)
        return rows

    @staticmethod
    def _sink_length(handle: Any, path: str) -> int:
        """Byte length of a sink, correct whether or not it is still open.

        Using the handle's ``tell()`` is wrong when the handle has already been
        closed (an interrupted run commits after closing its outputs): that
        reports 0, and a resume would then truncate the output to nothing while
        the session cursors still claim those frames were committed -- silently
        destroying the early part of the timeline.  Measuring the file on disk
        is always the truth.
        """
        if handle is not None:
            handle.flush()
        try:
            return os.path.getsize(path)
        except OSError:
            return 0

    def _commit(self, force: bool = False, input_done: bool = False) -> None:
        out_len = self._sink_length(self._out, self.output_path)
        dlq_len = (self._sink_length(self._dlq, self.dlq_path)
                   if self.dlq_path else 0)
        self.ckpt.commit(
            self._snapshot_sessions(),
            self.stats.to_dict(),
            self._safe_offset(),
            out_len,
            dlq_len,
            input_done,
            self._high_water,
        )
        self.stats.checkpoint_writes = self.ckpt.write_count
        self.stats.checkpoint_seconds = self.ckpt.write_seconds

    def _maybe_commit(self) -> None:
        """Flush on the frame or wall-clock threshold, whichever comes first."""
        if self.ckpt.maybe_flush():
            self._commit()

    # -- resume ------------------------------------------------------------

    def _load_checkpoint(self) -> None:
        sessions, offset, counters = self.ckpt.load()
        for sid, (last_seq, _off, snap) in sessions.items():
            self._last_seq[sid] = last_seq
            self.reasm.restore(sid, last_seq)
            if snap:
                self.machine.restore(sid, snap)
        st = self.stats
        # Counters are cumulative job totals, so a restart restores them all and
        # then bumps only what this restart adds to.
        for key in st.PERSISTED:
            if key in counters:
                setattr(st, key, int(counters[key]))
        st.resumed += 1
        self._resume_output, self._resume_dlq = self.ckpt.sink_offsets()
        st.input_done = self.ckpt.is_done()
        self.resume_offset = offset
        self._resume_sessions = len(sessions)
        # Sessions restored from the checkpoint hold no in-flight fragments, so
        # until the first line is read their safe offset must be the resume
        # point itself.  Leaving this at 0 would commit a global offset of 0 and
        # force the next resume to re-read the file from the beginning.
        self._next_offset = offset
        self._high_water = self.ckpt.high_water()

    # -- main loop ---------------------------------------------------------

    def _input_grew(self) -> bool:
        """True when the input is longer than when the job last finished."""
        try:
            size = os.path.getsize(self.input_path)
        except OSError:
            return False
        row = self.ckpt._conn.execute(
            "SELECT value FROM meta WHERE key='input_size'"
        ).fetchone()
        return bool(row) and size > int(row[0])

    def _iter_lines(self) -> Iterator[Tuple[int, bytes]]:
        """Yield ``(start_offset, line)`` from the resume point.

        Uses a binary file object and tracks offsets by accumulating
        ``len(line)``; a re-scan after a crash therefore lands exactly on a line
        boundary.
        """
        start = self.resume_offset
        with open(self.input_path, "rb") as fh:
            if start:
                fh.seek(start)
            offset = start
            for raw in fh:
                line_off = offset
                offset += len(raw)
                yield line_off, raw
        # The generator ran to exhaustion, so the whole tail is accounted for.
        self._eof = True

    def is_complete(self) -> bool:
        """True once the input has been consumed through to the end.

        Lets a batched driver (or the CLI) tell "finished" from "stopped early"
        without inspecting private checkpoint internals.
        """
        return self.stats.input_done

    def run(self) -> Stats:
        self._no_op = False
        if self.resume:
            self._load_checkpoint()
            if self.stats.input_done and self._input_grew():
                # The previous run finished this input and the file has since
                # grown, so start over deliberately rather than resuming into a
                # finalised output.  (Resuming an unchanged, already complete
                # input must instead be a no-op -- see below.)
                self.resume_offset = 0
                self._resume_output = 0
                self._resume_dlq = 0
                self.stats.input_done = False
                self._last_seq.clear()
                for st in self.machine.states().values():
                    st.points.clear()
                    st.breakers.clear()
                    st.frames_seen = 0
                    st.last_seq = -1
                self.machine._states.clear()
                self.machine._order.clear()
                self.stats = Stats()
                self._high_water = 0
            elif self.stats.input_done:
                # Nothing new to read: keep the finished output intact and make
                # the run a no-op.  Still count the restart, so the operator can
                # see from 'stats' that a resume was attempted.
                self._no_op = True
        append = self.resume_offset > 0
        self.stats.noop = self._no_op
        if self._no_op:
            # Persist the restart counter even though no work was done, so
            # 'stats' reflects that this job was resumed.
            self._commit(force=True, input_done=True)
        self._open_outputs(append=append or self._no_op)
        try:
            if not self._no_op:
                self._run_loop()
        finally:
            self.stats.ended = time.time()
            self._close_outputs()
        return self.stats

    def _run_loop(self) -> None:
        st = self.stats
        self._batch_start = st.lines_read
        b64decode = base64.b64decode
        jsonloads = json.loads
        parse = parse_frame

        # The line budget is per batch, so count it separately from the
        # cumulative ``lines_read`` that is restored from the checkpoint.
        budget = self.max_lines
        for line_off, raw in self._iter_lines():
            if budget and st.lines_read - self._batch_start >= budget:
                break
            # Advance the "next unconsumed line" cursor only once this line is
            # actually being processed.  Doing it in the generator would skip a
            # line whenever the budget check breaks out of the loop.
            self._next_offset = line_off + len(raw)
            if line_off < self._high_water:
                st.reread += 1  # revisited after a rewind; not new input
            elif line_off > self._high_water:
                self._high_water = line_off
            st.lines_read += 1
            st.bytes_read = line_off

            if not raw.strip():
                continue

            # --- layer 1: envelope parse ---------------------------------
            try:
                env = jsonloads(raw)
                if not isinstance(env, dict):
                    raise ValueError("envelope is not a JSON object")
                for field in REQUIRED_FIELDS:
                    if field not in env:
                        raise ValueError(f"missing field {field!r}")
                session_id = env["session_id"]
                if not isinstance(session_id, str) or not session_id:
                    raise ValueError("session_id must be a non-empty string")
                blob = env["payload"]
                if isinstance(blob, str):
                    data = b64decode(blob, validate=True)
                elif isinstance(blob, (bytes, bytearray)):
                    data = bytes(blob)
                else:
                    raise ValueError("payload must be a base64 string")
            except Exception as exc:
                # Catches the expected JSON/base64/type errors and anything
                # unforeseen alike: one bad line must never end the run.
                self._dead_letter(f"envelope:{type(exc).__name__}", raw)
                continue

            # Duplicate suppression across a resume boundary: the line that
            # completes a frame may be replayed, so drop anything at or below
            # the committed sequence number for that session.
            last = self._last_seq.get(session_id, -1)

            # --- layer 2: frame parse + reassembly -----------------------
            try:
                frame = parse(data)
            except ProtocolError as exc:
                self._dead_letter(f"frame:{exc}", raw, session_id=session_id)
                continue

            if frame.seq <= last:
                st.duplicate += 1
                continue

            try:
                released = self.reasm.feed(session_id, frame, line_off)
                st.lines_consumed += 1
            except Exception as exc:
                detail = (str(exc) if isinstance(exc, ProtocolError)
                          else type(exc).__name__)
                self._dead_letter(f"reassembly:{detail}", raw, session_id=session_id)
                continue

            # --- layer 3: state machine + output ------------------------
            for seq, body in released:
                try:
                    rec = self.machine.apply(session_id, seq, body)
                except Exception as exc:
                    self._dead_letter(
                        f"state:{type(exc).__name__}:{exc}", raw,
                        session_id=session_id, seq=seq,
                    )
                    continue
                self._last_seq[session_id] = seq
                st.frames_committed += 1
                if rec is not None:
                    self._emit(rec)
                self.ckpt.note_progress()
                self._maybe_commit()

            if st.lines_read % 50000 == 0 and self._progress_every:
                self._progress()

        # End of input: release anything parked behind a permanent gap.
        for session_id, seq, body in self.reasm.flush():
            try:
                rec = self.machine.apply(session_id, seq, body)
            except Exception as exc:
                self._dead_letter(f"state:{type(exc).__name__}:{exc}", b"",
                                  session_id=session_id, seq=seq)
                continue
            self._last_seq[session_id] = seq
            st.frames_committed += 1
            if rec is not None:
                self._emit(rec)
            self.ckpt.note_progress()

        st.evicted = self.reasm.stats()["evicted"]
        st.warnings = len(self.reasm.warnings)
        # Only a genuine end-of-input counts as complete; stopping early (line
        # budget, or a crash) must leave the job resumable.
        st.input_done = self._eof
        self._commit(force=True, input_done=self._eof)

    def _progress(self) -> None:
        el = max(time.time() - self.stats.started, 1e-9)
        rate = self.stats.frames_committed / el
        print(
            f"[replay] lines={self.stats.lines_read} frames={self.stats.frames_committed} "
            f"corrupt={self.stats.corrupt} {rate:,.0f} frames/s",
            flush=True,
        )
