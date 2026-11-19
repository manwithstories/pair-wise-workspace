"""Random-access index over a recovered RTP stream.

The index is a JSON document with one entry per line inside the array
so that a half-written (truncated) index can be salvaged entry by
entry: valid entries are kept, missing ones rebuilt.  All writes are
atomic (temp file + os.replace).
"""

from __future__ import annotations

import json
import os
import struct
import tempfile
from dataclasses import asdict, dataclass

from .model import MEDIA_CLOCK_RATE, RtpPacket

INDEX_FORMAT = "archive-restore-index"
INDEX_VERSION = 1


@dataclass
class IndexEntry:
    ssrc: int
    ext_seq: int
    timestamp: int  # extended 64-bit 90 kHz timestamp
    time_seconds: float | None  # None inside uncertain intervals
    offset: int
    length: int


def build_entry(pkt: RtpPacket, ext_seq: int, ext_ts: int,
                time_seconds: float | None) -> IndexEntry:
    return IndexEntry(
        ssrc=pkt.ssrc,
        ext_seq=ext_seq,
        timestamp=ext_ts,
        time_seconds=time_seconds,
        offset=pkt.offset,
        length=pkt.length,
    )


def index_document(entries: list[IndexEntry], input_sha256: str) -> dict:
    return {
        "format": INDEX_FORMAT,
        "version": INDEX_VERSION,
        "clock_rate": MEDIA_CLOCK_RATE,
        "input_sha256": input_sha256,
        "entries": [asdict(e) for e in entries],
    }


def write_index_atomic(path: str, doc: dict) -> None:
    """Write the index atomically; entries are one-per-line so a crash
    mid-write leaves a prefix that stays salvageable."""
    directory = os.path.dirname(os.path.abspath(path))
    os.makedirs(directory, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=directory, prefix=".index-", suffix=".tmp")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            fh.write("{\n")
            head = {k: v for k, v in doc.items() if k != "entries"}
            for i, (k, v) in enumerate(head.items()):
                comma = "," if i < len(head) - 1 else ","
                fh.write(f'  {json.dumps(k)}: {json.dumps(v)}{comma}\n')
            fh.write('  "entries": [\n')
            for i, entry in enumerate(doc["entries"]):
                comma = "," if i < len(doc["entries"]) - 1 else ""
                fh.write(f"    {json.dumps(entry, sort_keys=True)}{comma}\n")
            fh.write("  ]\n}\n")
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def load_index_tolerant(path: str) -> tuple[list[dict], bool]:
    """Load an index, salvaging complete entries from a truncated file.

    Returns (entries, was_damaged).  Raises ValueError if no usable
    entry structure can be found at all.
    """
    with open(path, "r", encoding="utf-8", errors="replace") as fh:
        text = fh.read()
    try:
        doc = json.loads(text)
        entries = doc.get("entries", [])
        if not isinstance(entries, list):
            raise ValueError("index 'entries' is not a list")
        return [e for e in entries if isinstance(e, dict)], False
    except json.JSONDecodeError:
        pass
    # Salvage: every well-formed entry occupies exactly one line.
    entries: list[dict] = []
    required = {"ssrc", "ext_seq", "timestamp", "offset", "length"}
    for line in text.splitlines():
        line = line.strip().rstrip(",")
        if not line.startswith("{"):
            continue
        try:
            obj = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(obj, dict) and required <= set(obj):
            entries.append(obj)
    if not entries and '"entries"' not in text:
        raise ValueError("not an archive-restore index file")
    return entries, True


def validate_entries(
    entries: list[dict], data: bytes
) -> tuple[list[dict], list[tuple[int, str]]]:
    """Check each entry against the raw stream bytes.

    Returns (valid_entries, failures) where failures are (row, reason).
    """
    valid: list[dict] = []
    failed: list[tuple[int, str]] = []
    for row, e in enumerate(entries):
        try:
            offset = int(e["offset"])
            length = int(e["length"])
            ssrc = int(e["ssrc"])
            ext_seq = int(e["ext_seq"])
        except (KeyError, TypeError, ValueError):
            failed.append((row, "malformed_entry"))
            continue
        if offset < 0 or length < 12 or offset + length > len(data):
            failed.append((row, "offset_out_of_range"))
            continue
        pkt = data[offset : offset + length]
        if len(pkt) < 12 or pkt[0] >> 6 != 2:
            failed.append((row, "not_an_rtp_packet"))
            continue
        seq, _ts, pkt_ssrc = struct.unpack(">HII", pkt[2:12])
        if pkt_ssrc != ssrc:
            failed.append((row, "ssrc_mismatch"))
            continue
        if seq != ext_seq % (1 << 16):
            failed.append((row, "sequence_mismatch"))
            continue
        valid.append(e)
    return valid, failed


def check_monotonic_time(entries: list[dict]) -> list[int]:
    """Return rows whose known time decreases vs. the previous known."""
    bad: list[int] = []
    prev: float | None = None
    for row, e in enumerate(entries):
        t = e.get("time_seconds")
        if t is None:
            continue
        t = float(t)
        if prev is not None and t < prev:
            bad.append(row)
        prev = t
    return bad


def lookup(entries: list[dict], ssrc: int, t0: float, t1: float) -> list[dict]:
    """Random-access lookup: entries for ``ssrc`` within [t0, t1] seconds."""
    out = []
    for e in entries:
        if int(e["ssrc"]) != ssrc:
            continue
        t = e.get("time_seconds")
        if t is not None and t0 <= float(t) <= t1:
            out.append(e)
    return out
