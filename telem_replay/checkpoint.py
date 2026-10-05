"""SQLite-backed checkpointing for crash-resumable replay.

Everything a resume needs lives in one row per session plus a small ``meta``
table, written inside a single transaction so the database is never observed in
a half-updated state.  WAL journalling lets the reader (``stats``) run while a
replay is in progress.

Durability tradeoff
-------------------
``synchronous=NORMAL`` is the default: in WAL mode it still guarantees the
checkpoint survives a *process* crash (the OS holds the write), it only risks
the last few transactions on a host power loss.  ``FULL`` is available via
``--sync full`` for operators who replay unattended overnight.
"""

from __future__ import annotations

import json
import os
import sqlite3
import time
from typing import Any, Dict, Tuple

SCHEMA_VERSION = 1

_SCHEMA = """
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- One row per session: the highest sequence number whose state has been
-- durably written to the output, plus its rebuilt state-machine snapshot.
CREATE TABLE IF NOT EXISTS session_state (
    session_id   TEXT PRIMARY KEY,
    last_seq     INTEGER NOT NULL,
    byte_offset  INTEGER NOT NULL,
    snapshot     BLOB,
    updated_at   REAL NOT NULL
);

-- Per-session running totals, so 'stats' never has to re-read the input.
CREATE TABLE IF NOT EXISTS counters (
    key   TEXT PRIMARY KEY,
    value INTEGER NOT NULL
);
"""


class Checkpoint:
    """Atomic, batched checkpoint writer / reader."""

    __slots__ = ("path", "_conn", "_dirty", "_frames_since", "_last_write",
                 "frame_interval", "time_interval", "write_count",
                 "write_seconds", "resumed")

    def __init__(self, path: str, *, sync: str = "normal",
                 frame_interval: int = 2000,
                 time_interval: float = 30.0,
                 read_only: bool = False) -> None:
        self.path = path
        self.frame_interval = frame_interval
        self.time_interval = time_interval
        self.write_count = 0
        self.write_seconds = 0.0
        self.resumed = 0
        self._dirty = False
        self._frames_since = 0
        self._last_write = time.monotonic()

        if read_only:
            if not os.path.exists(path):
                raise FileNotFoundError(path)
            self._conn = sqlite3.connect(f"file:{path}?mode=ro", uri=True)
        else:
            self._conn = sqlite3.connect(path, isolation_level=None)
            self._conn.execute("PRAGMA journal_mode=WAL")
            # NORMAL avoids an fsync per transaction; FULL is selectable.
            self._conn.execute(
                "PRAGMA synchronous=" + ("FULL" if sync == "full" else "NORMAL")
            )
            self._conn.execute("PRAGMA temp_store=MEMORY")
            # Bound WAL growth so a long run cannot fill the disk.
            self._conn.execute("PRAGMA journal_size_limit=67108864")
            self._conn.execute("PRAGMA cache_size=-4096")  # ~4 MB page cache
            self._conn.executescript(_SCHEMA)
            self._set_meta("schema_version", str(SCHEMA_VERSION))
            self._set_meta("input_path", "")

    # -- low level ---------------------------------------------------------

    def _set_meta(self, key: str, value: str) -> None:
        self._conn.execute(
            "INSERT INTO meta(key, value) VALUES(?, ?) "
            "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
            (key, value),
        )

    def _set_counter(self, key: str, value: int) -> None:
        self._conn.execute(
            "INSERT INTO counters(key, value) VALUES(?, ?) "
            "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
            (key, int(value)),
        )

    def _bump(self, key: str, delta: int) -> None:
        self._conn.execute(
            "INSERT INTO counters(key, value) VALUES(?, ?) "
            "ON CONFLICT(key) DO UPDATE SET value=value+?",
            (key, int(delta)),
        )

    # -- write path --------------------------------------------------------

    def note_progress(self, n: int = 1) -> None:
        """Record that *n* more frames were committed since the last commit.

        Marks the checkpoint dirty and advances the frame counter, so the
        next :meth:`maybe_flush` can decide whether a commit is due.
        """
        self._dirty = True
        self._frames_since += n

    def maybe_flush(self, force: bool = False) -> bool:
        """Flush if the frame or time threshold has been crossed."""
        now = time.monotonic()
        due = force or self._frames_since >= self.frame_interval or (
            self._dirty and (now - self._last_write) >= self.time_interval
        )
        if not due or (not self._dirty and not force):
            return False
        self._last_write = time.monotonic()
        return True

    def commit(self, sessions: Dict[str, Tuple[int, int, Any]], counters: Dict[str, int],
               global_offset: int, output_offset: int = 0, dlq_offset: int = 0,
               input_done: bool = False, high_water: int = 0) -> None:
        """Atomically persist session cursors, snapshots, counters and output length.

        *sessions* maps session_id -> (last_seq, byte_offset, snapshot_obj).
        *counters* holds the run totals to merge.  *high_water* is the highest
        input offset read so far, which lets a later run tell a re-read line
        apart from forward progress.  *output_offset* / *dlq_offset* are the
        byte lengths the output files had **at this exact moment**, which
        is what makes the sink consistent with the checkpoint: a crash between
        two commits leaves records in the output that the checkpoint does not
        know about, so a resume must truncate back to the recorded length
        before appending.  All of it lands in one transaction: a crash either
        loses the whole batch or none of it.
        """
        t0 = time.monotonic()
        conn = self._conn
        try:
            conn.execute("BEGIN IMMEDIATE")
            now = time.time()
            conn.executemany(
                "INSERT INTO session_state"
                "(session_id, last_seq, byte_offset, snapshot, updated_at) "
                "VALUES(?, ?, ?, ?, ?) "
                "ON CONFLICT(session_id) DO UPDATE SET "
                "last_seq=excluded.last_seq, byte_offset=excluded.byte_offset, "
                "snapshot=excluded.snapshot, updated_at=excluded.updated_at",
                [
                    (
                        sid,
                        int(last_seq),
                        int(off),
                        sqlite3.Binary(
                            json.dumps(snap, separators=(",", ":")).encode("utf-8")
                        )
                        if snap is not None
                        else None,
                        now,
                    )
                    for sid, (last_seq, off, snap) in sessions.items()
                ],
            )
            # Counters and metadata go in as two batched statements.  Issuing
            # one INSERT per key instead costs an order of magnitude more in
            # statement overhead, which is what pushed commit cost -- and so
            # total runtime -- past the 5% budget at a 2000-frame interval.
            conn.executemany(
                "INSERT INTO counters(key, value) VALUES(?, ?) "
                "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
                [(k, int(v)) for k, v in counters.items()],
            )
            conn.executemany(
                "INSERT INTO meta(key, value) VALUES(?, ?) "
                "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
                [
                    ("global_offset", str(int(global_offset))),
                    ("output_offset", str(int(output_offset))),
                    ("dlq_offset", str(int(dlq_offset))),
                    # Definite end-of-input marker, so a batched replay knows it
                    # is finished without inferring it from an offset compare.
                    ("input_done", "1" if input_done else "0"),
                    ("high_water", str(int(high_water))),
                    ("updated_at", str(now)),
                ],
            )
            conn.execute("COMMIT")
        except Exception:
            conn.execute("ROLLBACK")
            raise
        finally:
            self.write_seconds += time.monotonic() - t0
            self.write_count += 1
            self._dirty = False
            self._frames_since = 0
            self._last_write = time.monotonic()

    # -- read path ---------------------------------------------------------

    def high_water(self) -> int:
        """Highest input offset any run has read up to.

        A resumed run rewinds to the safe offset and re-reads up to here, so
        comparing against this mark is how re-read lines are distinguished from
        genuine forward progress.
        """
        row = self._conn.execute(
            "SELECT value FROM meta WHERE key='high_water'"
        ).fetchone()
        return int(row[0]) if row else 0

    def is_done(self) -> bool:
        """True once a run has consumed the input through to the end."""
        row = self._conn.execute(
            "SELECT value FROM meta WHERE key='input_done'"
        ).fetchone()
        return bool(row) and row[0] == "1"

    def sink_offsets(self) -> Tuple[int, int]:
        """Recorded (output, dlq) byte lengths as of the last commit."""
        rows = dict(self._conn.execute(
            "SELECT key, value FROM meta WHERE key IN ('output_offset','dlq_offset')"
        ))
        return int(rows.get("output_offset", 0)), int(rows.get("dlq_offset", 0))

    def load(self) -> Tuple[Dict[str, Tuple[int, int, Any]], int, Dict[str, int]]:
        """Return (sessions, global_offset, counters) from the last checkpoint."""
        sessions: Dict[str, Tuple[int, int, Any]] = {}
        for sid, last_seq, off, blob in self._conn.execute(
            "SELECT session_id, last_seq, byte_offset, snapshot FROM session_state"
        ):
            snap = json.loads(blob.decode("utf-8")) if blob else None
            sessions[sid] = (last_seq, off, snap)
        row = self._conn.execute(
            "SELECT value FROM meta WHERE key='global_offset'"
        ).fetchone()
        offset = int(row[0]) if row else 0
        counters = {
            k: v
            for k, v in self._conn.execute("SELECT key, value FROM counters")
        }
        return sessions, offset, counters

    def record_input(self, path: str) -> None:
        try:
            size = os.path.getsize(path)
        except OSError:
            size = -1
        self._set_meta("input_path", os.path.abspath(path))
        self._set_meta("input_size", str(size))
        self._set_meta("input_mtime", str(int(os.path.getmtime(path))))

    # -- reporting ---------------------------------------------------------

    def close(self) -> None:
        try:
            self._conn.execute("PRAGMA wal_checkpoint(TRUNCATE)")
        except sqlite3.Error:
            pass
        self._conn.close()

    def __enter__(self) -> "Checkpoint":
        return self

    def __exit__(self, *exc: Any) -> None:
        self.close()
