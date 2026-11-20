"""Index schema, row types and version constants.

Kept separate from the SQLite writer/reader so the on-disk contract - table
layout, row fields, digests and version - can be reviewed and imported on
its own.  ``index`` builds its writer and reader on top of these
definitions.
"""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from fractions import Fraction

INDEX_VERSION = 1
TOOL_VERSION = "0.1.0"

SCHEMA = """
CREATE TABLE IF NOT EXISTS meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tracks (
    track_index INTEGER PRIMARY KEY,
    stream_index INTEGER NOT NULL,
    kind TEXT NOT NULL,
    codec TEXT NOT NULL,
    time_base_num INTEGER NOT NULL,
    time_base_den INTEGER NOT NULL,
    output_timescale INTEGER NOT NULL,
    sample_rate INTEGER,
    channels INTEGER
);
CREATE TABLE IF NOT EXISTS chunks (
    chunk_index INTEGER PRIMARY KEY,
    first_global_packet INTEGER NOT NULL,
    last_global_packet INTEGER NOT NULL,
    first_packet_seq INTEGER NOT NULL,
    last_packet_seq INTEGER NOT NULL,
    packet_count INTEGER NOT NULL,
    file_offset INTEGER NOT NULL,
    file_length INTEGER NOT NULL,
    rows_sha256 TEXT NOT NULL,
    bytes_sha256 TEXT NOT NULL,
    start_seconds TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS samples (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    track_index INTEGER NOT NULL,
    kind TEXT NOT NULL,
    packet_seq INTEGER NOT NULL,
    global_packet INTEGER NOT NULL,
    keyframe INTEGER NOT NULL,
    dts_ticks INTEGER NOT NULL,
    pts_ticks INTEGER NOT NULL,
    time_seconds TEXT NOT NULL,
    byte_offset INTEGER NOT NULL,
    byte_size INTEGER NOT NULL,
    chunk_index INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS samples_by_time ON samples(track_index, time_seconds);
CREATE INDEX IF NOT EXISTS samples_by_offset ON samples(byte_offset);
CREATE TABLE IF NOT EXISTS damage (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    track_index INTEGER NOT NULL,
    start_packet INTEGER NOT NULL,
    end_packet INTEGER NOT NULL,
    reason TEXT NOT NULL,
    dts INTEGER,
    pts INTEGER,
    notes TEXT
);
"""


@dataclass
class ChunkRecord:
    chunk_index: int
    first_global_packet: int
    last_global_packet: int
    first_packet_seq: int
    last_packet_seq: int
    packet_count: int
    file_offset: int
    file_length: int
    rows_sha256: str
    bytes_sha256: str
    start_seconds: str

    def as_dict(self) -> dict:
        return {
            "chunk_index": self.chunk_index,
            "first_global_packet": self.first_global_packet,
            "last_global_packet": self.last_global_packet,
            "first_packet_seq": self.first_packet_seq,
            "last_packet_seq": self.last_packet_seq,
            "packet_count": self.packet_count,
            "file_offset": self.file_offset,
            "file_length": self.file_length,
            "rows_sha256": self.rows_sha256,
            "bytes_sha256": self.bytes_sha256,
            "start_seconds": self.start_seconds,
        }


@dataclass
class SampleRow:
    track_index: int
    kind: str
    packet_seq: int
    global_packet: int
    keyframe: bool
    dts_ticks: int
    pts_ticks: int
    time_seconds: Fraction
    byte_offset: int
    byte_size: int
    chunk_index: int

    def as_dict(self) -> dict:
        return {
            "track_index": self.track_index,
            "kind": self.kind,
            "packet_seq": self.packet_seq,
            "global_packet": self.global_packet,
            "keyframe": self.keyframe,
            "dts_ticks": self.dts_ticks,
            "pts_ticks": self.pts_ticks,
            "time_seconds": str(self.time_seconds),
            "byte_offset": self.byte_offset,
            "byte_size": self.byte_size,
            "chunk_index": self.chunk_index,
        }


@dataclass
class DamageRow:
    track_index: int
    start_packet: int
    end_packet: int
    reason: str
    dts: int | None = None
    pts: int | None = None
    notes: dict | None = None


def rows_digest(rows) -> str:
    """Stable digest of a chunk's sample rows, used for resume equivalence."""
    digest = hashlib.sha256()
    for row in rows:
        digest.update(json.dumps(row, sort_keys=True, separators=(",", ":")).encode())
        digest.update(b"\n")
    return digest.hexdigest()
