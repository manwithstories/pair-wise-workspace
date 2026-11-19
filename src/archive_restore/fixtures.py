"""Fixture generator: clean, damaged and half-written-index samples.

All sample data is synthesized locally (sine-wave PCM payloads); no
external files are referenced.  The generator returns the byte streams
plus a metadata record of exactly where each defect was introduced, so
tests can assert byte-offset-accurate error reporting.
"""

from __future__ import annotations

import json
import os
import struct

import numpy as np

from .model import MEDIA_CLOCK_RATE, RTCP_SR
from .probe import PCM_SAMPLE_RATE

SSRC = 0xDEADBEEF
PAYLOAD_TYPE = 96
SAMPLES_PER_PACKET = 160  # 20 ms at 8 kHz
TS_STEP = MEDIA_CLOCK_RATE * SAMPLES_PER_PACKET // PCM_SAMPLE_RATE  # 1800
SR_EVERY = 500
NTP_UNIX_EPOCH_OFFSET = 2_208_988_800  # 1900 -> 1970


def make_rtp_packet(seq: int, ts: int, payload: bytes, ssrc: int = SSRC,
                    pt: int = PAYLOAD_TYPE, marker: bool = False) -> bytes:
    b0 = 0x80  # V=2, P=0, X=0, CC=0
    b1 = (0x80 if marker else 0) | (pt & 0x7F)
    return struct.pack(">BBHII", b0, b1, seq & 0xFFFF, ts & 0xFFFFFFFF, ssrc) + payload


def make_rtcp_sr(ssrc: int, ntp_seconds: float, rtp_ts: int) -> bytes:
    ntp = ntp_seconds + NTP_UNIX_EPOCH_OFFSET
    sec = int(ntp)
    frac = int((ntp - sec) * (1 << 32))
    body = struct.pack(">IIIIII", ssrc, sec, frac, rtp_ts & 0xFFFFFFFF, 0, 0)
    header = struct.pack(">BBH", 0x80, RTCP_SR, (28 // 4) - 1)
    return header + body  # 4 + 24 = 28 bytes


def frame(packet: bytes) -> bytes:
    return struct.pack(">H", len(packet)) + packet


def _payload_stream(n_packets: int, seed: int) -> bytes:
    rng = np.random.default_rng(seed)
    t = np.arange(n_packets * SAMPLES_PER_PACKET, dtype=np.float64)
    wave = 0.3 * np.sin(2 * np.pi * 440.0 * t / PCM_SAMPLE_RATE)
    wave += 0.05 * rng.standard_normal(t.shape[0])
    return (np.clip(wave, -1, 1) * 32767).astype("<i2").tobytes()


def build_clean(n_packets: int = 10_000, seed: int = 12797,
                seq_start: int = 65_000, ts_start: int = 0xFFFFFFF0):
    """Build a clean framed stream.  Returns (records, meta)."""
    pcm = _payload_stream(n_packets, seed)
    records: list[bytes] = []
    kinds: list[str] = []
    for i in range(n_packets):
        seq = seq_start + i
        ts = ts_start + i * TS_STEP
        if i % SR_EVERY == 0:
            ntp = 1_700_000_000.0 + i * SAMPLES_PER_PACKET / PCM_SAMPLE_RATE
            records.append(frame(make_rtcp_sr(SSRC, ntp, ts)))
            kinds.append("sr")
        chunk = pcm[i * SAMPLES_PER_PACKET * 2 : (i + 1) * SAMPLES_PER_PACKET * 2]
        records.append(frame(make_rtp_packet(seq, ts, chunk, marker=(i % 100 == 0))))
        kinds.append("rtp")
    meta = {"n_packets": n_packets, "seed": seed, "seq_start": seq_start,
            "ts_start": ts_start, "kinds": kinds, "damages": {}}
    return records, meta


def apply_damage(records: list[bytes], meta: dict) -> tuple[list[bytes], dict]:
    """Introduce reorder, loss, duplication, bad length, bad version and
    a truncated tail.  Returns (records, damages) with byte offsets
    computed after assembly by the caller."""
    recs = list(records)
    damages: dict[str, dict] = {}
    n = len(recs)

    # 1) reorder: swap adjacent records around every 97th RTP record.
    swapped = []
    i = 5
    while i + 1 < n - 3:
        recs[i], recs[i + 1] = recs[i + 1], recs[i]
        swapped.append(i)
        i += 97
    damages["reordered_rows"] = {"rows": swapped[:5], "count": len(swapped)}

    # 2) loss: drop ~1% of records.
    drop_rows = sorted(range(11, n - 2, 101), reverse=True)
    for r in drop_rows:
        del recs[r]
    damages["dropped_rows"] = {"count": len(drop_rows)}

    # 3) duplicate one record.
    dup_at = len(recs) // 3
    recs.insert(dup_at, recs[dup_at])
    damages["duplicate_row"] = {"row": dup_at}

    # 4) bad length field mid-stream.
    bad_len_row = len(recs) // 2
    rec = bytearray(recs[bad_len_row])
    struct.pack_into(">H", rec, 0, 8)  # below MIN_PACKET_LEN
    recs[bad_len_row] = bytes(rec)
    damages["bad_length"] = {"row": bad_len_row}

    # 5) bad RTP version byte inside a packet.
    bad_ver_row = (3 * len(recs)) // 4
    rec = bytearray(recs[bad_ver_row])
    rec[2] = 0x40  # version 1
    recs[bad_ver_row] = bytes(rec)
    damages["bad_version"] = {"row": bad_ver_row}

    # 6) truncated tail: cut the final record mid-packet.
    tail = recs[-1]
    recs[-1] = tail[: len(tail) - 10]
    damages["truncated_tail"] = {"row": len(recs) - 1}
    return recs, damages


def assemble(records: list[bytes]) -> tuple[bytes, list[int]]:
    """Concatenate records; returns (stream, record_start_offsets)."""
    offsets = []
    pos = 0
    out = bytearray()
    for rec in records:
        offsets.append(pos)
        out += rec
        pos += len(rec)
    return bytes(out), offsets


def write_fixtures(out_dir: str, n_packets: int = 10_000,
                   seed: int = 12797) -> dict:
    """Generate all sample files into ``out_dir``; returns a manifest."""
    os.makedirs(out_dir, exist_ok=True)
    records, meta = build_clean(n_packets, seed)
    clean_bytes, _ = assemble(records)
    clean_path = os.path.join(out_dir, "clean.rtp")
    with open(clean_path, "wb") as fh:
        fh.write(clean_bytes)

    damaged_records, damages = apply_damage(records, meta)
    damaged_bytes, offsets = assemble(damaged_records)
    for name, info in damages.items():
        row = info.get("row")
        if row is not None:
            info["offset"] = offsets[row]
    damaged_path = os.path.join(out_dir, "damaged.rtp")
    with open(damaged_path, "wb") as fh:
        fh.write(damaged_bytes)

    # Half-written index: valid JSON prefix with ~40% of entries, cut
    # mid-line, as if a previous indexer crashed.
    from .index import index_document, write_index_atomic  # noqa: PLC0415
    from .rtp import parse_stream  # noqa: PLC0415
    from .repair import repair_packets  # noqa: PLC0415

    parsed = parse_stream(clean_bytes)
    rep = repair_packets(parsed.packets)
    entries = []
    for idx in rep.order:
        pkt = parsed.packets[idx]
        entries.append({
            "ssrc": pkt.ssrc, "ext_seq": rep.ext_seq[idx],
            "timestamp": rep.ext_ts[idx],
            "time_seconds": (rep.ext_ts[idx] - rep.ext_ts[rep.order[0]])
            / MEDIA_CLOCK_RATE,
            "offset": pkt.offset, "length": pkt.length,
        })
    doc = index_document(entries[: max(1, len(entries) * 2 // 5)], "0" * 64)
    tmp_index = os.path.join(out_dir, ".index-full.tmp")
    write_index_atomic(tmp_index, doc)
    with open(tmp_index, "rb") as fh:
        full = fh.read()
    index_path = os.path.join(out_dir, "index.json")
    with open(index_path, "wb") as fh:
        fh.write(full[: len(full) * 3 // 4])  # truncate mid-entry
    os.unlink(tmp_index)

    meta["damages"] = damages
    meta["files"] = {"clean": clean_path, "damaged": damaged_path,
                     "index": index_path}
    meta_path = os.path.join(out_dir, "meta.json")
    with open(meta_path, "w", encoding="utf-8") as fh:
        json.dump(meta, fh, indent=2)
    return meta
