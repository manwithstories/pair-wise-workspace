"""SQLite random-access index: transactional chunk writer.

The writer records metadata, tracks, committed chunks, seek samples and
skipped damage intervals.  A chunk's rows and the commit cursor land in one
transaction, so a crash never exposes a half-written chunk.

The fresh builder writes to a temporary file and ``os.replace``s it into
place, so an interrupted build never leaves a usable-looking index.
``open_existing`` reopens a partially committed index in place for resume,
where the rows already written must survive a second interruption.

The on-disk contract (schema, row types, version) lives in ``index_types``.
"""

from __future__ import annotations

import json
import os
import sqlite3
import tempfile
from fractions import Fraction
from pathlib import Path
from typing import Sequence

from .errors import IndexIncompatibleError, IndexWriteError
from .index_types import (
    INDEX_VERSION,
    SCHEMA,
    TOOL_VERSION,
    ChunkRecord,
    DamageRow,
    SampleRow,
    rows_digest,
)
from .index_read import IndexReader  # re-exported for convenience
from .timeline import TrackSpec, dump_fraction



class IndexWriter:
    """Build a fresh index at a temporary path and move it into place."""

    def __init__(self, target: Path):
        self.target = Path(target)
        self.target.parent.mkdir(parents=True, exist_ok=True)
        handle, temp_name = tempfile.mkstemp(prefix=f".{self.target.name}.", suffix=".tmp",
                                             dir=str(self.target.parent))
        os.close(handle)
        self.temp_path = Path(temp_name)
        try:
            self.connection = sqlite3.connect(str(self.temp_path))
            self.connection.executescript(SCHEMA)
            self.connection.commit()
        except Exception as exc:
            self.abort()
            raise IndexWriteError(f"cannot create index: {exc}") from exc
        self._commit_seq = 0
        self._closed = False

    # -- metadata ---------------------------------------------------------
    def set_meta(self, **values: object) -> None:
        payload = []
        for key, value in values.items():
            if isinstance(value, (dict, list)):
                encoded = json.dumps(value, sort_keys=True)
            elif isinstance(value, Fraction):
                encoded = json.dumps(dump_fraction(value))
            else:
                encoded = str(value)
            payload.append((key, encoded))
        try:
            with self.connection:
                self.connection.executemany(
                    "INSERT INTO meta(key, value) VALUES (?, ?) "
                    "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
                    payload,
                )
        except sqlite3.Error as exc:
            raise IndexWriteError(f"cannot write index metadata: {exc}") from exc

    def add_tracks(self, tracks: Sequence[TrackSpec], timescales: dict[int, int]) -> None:
        rows = []
        for spec in tracks:
            timescale = timescales.get(spec.stream_index)
            if not timescale:
                raise IndexWriteError(f"no output timescale for stream {spec.stream_index}")
            rows.append((
                spec.track_index,
                spec.stream_index,
                spec.kind,
                spec.codec,
                spec.time_base.numerator,
                spec.time_base.denominator,
                int(timescale),
                spec.sample_rate,
                spec.channels,
            ))
        try:
            with self.connection:
                self.connection.executemany(
                    "INSERT OR REPLACE INTO tracks(track_index, stream_index, kind, codec, "
                    "time_base_num, time_base_den, output_timescale, sample_rate, channels) "
                    "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
                    rows,
                )
        except sqlite3.Error as exc:
            raise IndexWriteError(f"cannot write tracks: {exc}") from exc

    # -- chunk commit -----------------------------------------------------
    def commit_chunk(self, record: ChunkRecord, samples: Sequence[SampleRow],
                     damage: Sequence[DamageRow] = ()) -> None:
        """Persist one chunk's rows and bump ``commit_seq`` in one transaction."""
        try:
            with self.connection:
                self.connection.execute(
                    "INSERT INTO chunks(chunk_index, first_global_packet, last_global_packet, "
                    "first_packet_seq, last_packet_seq, packet_count, file_offset, file_length, "
                    "rows_sha256, bytes_sha256, start_seconds) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
                    (
                        record.chunk_index, record.first_global_packet, record.last_global_packet,
                        record.first_packet_seq, record.last_packet_seq, record.packet_count,
                        record.file_offset, record.file_length, record.rows_sha256,
                        record.bytes_sha256, record.start_seconds,
                    ),
                )
                self.connection.executemany(
                    "INSERT INTO samples(track_index, kind, packet_seq, global_packet, keyframe, "
                    "dts_ticks, pts_ticks, time_seconds, byte_offset, byte_size, chunk_index) "
                    "VALUES (?,?,?,?,?,?,?,?,?,?,?)",
                    [
                        (s.track_index, s.kind, s.packet_seq, s.global_packet, int(s.keyframe),
                         s.dts_ticks, s.pts_ticks, str(s.time_seconds), s.byte_offset, s.byte_size,
                         s.chunk_index)
                        for s in samples
                    ],
                )
                self.connection.executemany(
                    "INSERT INTO damage(track_index, start_packet, end_packet, reason, dts, pts, notes) "
                    "VALUES (?,?,?,?,?,?,?)",
                    [
                        (d.track_index, d.start_packet, d.end_packet, d.reason, d.dts, d.pts,
                         json.dumps(d.notes, sort_keys=True) if d.notes else None)
                        for d in damage
                    ],
                )
                self._commit_seq = record.chunk_index
                self.connection.execute(
                    "INSERT INTO meta(key, value) VALUES ('commit_seq', ?) "
                    "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
                    (str(self._commit_seq),),
                )
        except sqlite3.Error as exc:
            raise IndexWriteError(f"cannot commit chunk {record.chunk_index}: {exc}") from exc

    @classmethod
    def open_existing(cls, path: Path) -> "IndexWriter":
        """Reopen a partially committed index for appending (resume path).

        Unlike the fresh builder this writes in place: the rows already
        committed must survive a second interruption, and the checkpoint's
        committed byte boundary is the only thing that may roll back.
        """
        path = Path(path)
        if not path.exists():
            raise IndexIncompatibleError(f"cannot resume without an index: {path}",
                                         context={"path": str(path)})
        instance = cls.__new__(cls)
        instance.target = path
        instance.temp_path = path
        try:
            instance.connection = sqlite3.connect(str(path))
            instance.connection.executescript(SCHEMA)
            instance.connection.commit()
        except sqlite3.Error as exc:
            raise IndexIncompatibleError(f"cannot open index for resume: {exc}") from exc
        instance._commit_seq = 0
        instance._closed = False
        return instance

    def finish(self, **meta: object) -> None:
        if meta:
            self.set_meta(**meta)
        try:
            self.connection.execute("ANALYZE")
            self.connection.commit()
            self.connection.execute("PRAGMA wal_checkpoint(TRUNCATE)") if False else None
            self.connection.close()
        except sqlite3.Error as exc:
            raise IndexWriteError(f"cannot finalise index: {exc}") from exc
        self._closed = True
        if self.temp_path != self.target:
            self.target.parent.mkdir(parents=True, exist_ok=True)
            os.replace(self.temp_path, self.target)

    def abort(self) -> None:
        if self._closed:
            return
        self._closed = True
        try:
            self.connection.close()
        except Exception:
            pass
        if self.temp_path == self.target:
            return
        try:
            os.unlink(self.temp_path)
        except FileNotFoundError:
            pass

    def __enter__(self) -> "IndexWriter":
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        if exc_type is None:
            return
        self.abort()


# ---------------------------------------------------------------------------
# Reader
# ---------------------------------------------------------------------------
