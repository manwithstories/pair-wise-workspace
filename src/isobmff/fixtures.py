"""Synthetic fMP4 fixture generation and corruption injection.

All bytes are derived from random.Random(seed); the same seed always
produces byte-identical output. No external media files are used.
"""
from __future__ import annotations

import random
import struct
from dataclasses import dataclass

TIMESCALE = 90000
FRAME_DURATION = 3750  # one frame at 24fps on a 90000 timescale
CORRUPT_KINDS = ("offset", "duration", "stsc", "truncate")


@dataclass
class FixtureParams:
    samples: int = 240
    fragments: int = 6
    seed: int = 7
    corrupt: str | None = None


def _box(type_: bytes, payload: bytes) -> bytes:
    return struct.pack(">I4s", 8 + len(payload), type_) + payload


def _fullbox(type_: bytes, version: int, flags: int, payload: bytes) -> bytes:
    return _box(type_, bytes([version]) + flags.to_bytes(3, "big") + payload)


def build_sidx(version: int, references: list[tuple[int, int, int]],
               timescale: int = TIMESCALE, first_offset: int = 0) -> bytes:
    """Build a sidx box; references are (type, size, duration) triples."""
    payload = struct.pack(">II", 1, timescale)
    if version == 1:
        payload += struct.pack(">QQ", 0, first_offset)
    else:
        payload += struct.pack(">II", 0, first_offset)
    payload += struct.pack(">HH", 0, len(references))
    for ref_type, size, duration in references:
        sap_word = (1 << 31) | (1 << 28)
        payload += struct.pack(">III", (ref_type << 31) | size, duration,
                               sap_word)
    return _fullbox(b"sidx", version, 0, payload)


def _build_moov(sizes: list[int], per_fragment: int) -> bytes:
    count = len(sizes)
    stts = _fullbox(b"stts", 0, 0,
                    struct.pack(">III", 1, count, FRAME_DURATION))
    ctts_offsets = [((i // 8) % 3 - 1) * 512 for i in range(count)]
    runs: list[tuple[int, int]] = []
    for offset in ctts_offsets:
        if runs and runs[-1][1] == offset:
            runs[-1] = (runs[-1][0] + 1, offset)
        else:
            runs.append((1, offset))
    ctts_payload = struct.pack(">I", len(runs))
    for run_count, offset in runs:
        ctts_payload += struct.pack(">Ii", run_count, offset)
    ctts = _fullbox(b"ctts", 1, 0, ctts_payload)
    stsc = _fullbox(b"stsc", 0, 0,
                    struct.pack(">IIII", 1, 1, per_fragment, 1))
    stsz_payload = struct.pack(">II", 0, count)
    for size in sizes:
        stsz_payload += struct.pack(">I", size)
    stsz = _fullbox(b"stsz", 0, 0, stsz_payload)
    chunks = (count + per_fragment - 1) // per_fragment
    stco = _fullbox(b"stco", 0, 0,
                    struct.pack(">I", chunks) + b"\0" * (4 * chunks))
    stsd = _fullbox(b"stsd", 0, 0,
                    struct.pack(">I", 1) + _box(b"avc1", b"\0" * 78))
    stbl = _box(b"stbl", stsd + stts + ctts + stsc + stsz + stco)
    vmhd = _fullbox(b"vmhd", 0, 1, b"\0" * 8)
    url = _fullbox(b"url ", 0, 1, b"")
    dref = _fullbox(b"dref", 0, 0, struct.pack(">I", 1) + url)
    minf = _box(b"minf", vmhd + _box(b"dinf", dref) + stbl)
    mdhd = _fullbox(b"mdhd", 0, 0, struct.pack(
        ">IIIIHH", 0, 0, TIMESCALE, count * FRAME_DURATION, 0x55C4, 0))
    hdlr = _fullbox(b"hdlr", 0, 0,
                    struct.pack(">I4sIII", 0, b"vide", 0, 0, 0)
                    + b"VideoHandler\0")
    mdia = _box(b"mdia", mdhd + hdlr + minf)
    tkhd = _fullbox(b"tkhd", 0, 3, struct.pack(
        ">IIIII", 0, 0, 1, 0, count * FRAME_DURATION) + b"\0" * 60)
    trak = _box(b"trak", tkhd + mdia)
    mvhd = _fullbox(b"mvhd", 0, 0, struct.pack(
        ">IIII", 0, 0, TIMESCALE, count * FRAME_DURATION) + b"\0" * 84)
    trex = _fullbox(b"trex", 0, 0, struct.pack(">IIIII", 1, 1, 0, 0, 0))
    return _box(b"moov", mvhd + _box(b"mvex", trex) + trak)


def _build_fragment(seq: int, base_decode_time: int,
                    sizes: list[int], payload: bytes) -> tuple[bytes, int]:
    """Build one moof+mdat pair; returns (bytes, moof_size)."""
    mfhd = _fullbox(b"mfhd", 0, 0, struct.pack(">I", seq))
    tfhd = _fullbox(b"tfhd", 0, 0x020000, struct.pack(">I", 1))
    tfdt = _fullbox(b"tfdt", 1, 0, struct.pack(">Q", base_decode_time))
    count = len(sizes)
    trun_entries = b"".join(
        struct.pack(">II", FRAME_DURATION, size) for size in sizes)

    def make_trun(data_offset: int) -> bytes:
        head = struct.pack(">Ii", count, data_offset)
        return _fullbox(b"trun", 0, 0x000001 | 0x000100 | 0x000200,
                        head + trun_entries)

    moof = _box(b"moof", mfhd + _box(b"traf", tfhd + tfdt + make_trun(0)))
    data_offset = len(moof) + 8  # default-base-is-moof -> mdat payload
    moof = _box(b"moof", mfhd + _box(b"traf", tfhd + tfdt
                                     + make_trun(data_offset)))
    mdat = _box(b"mdat", payload)
    return moof + mdat, len(moof)


def generate(params: FixtureParams) -> bytes:
    """Generate a complete fragmented MP4 as bytes."""
    if params.fragments < 1 or params.samples % params.fragments:
        raise ValueError("samples must be a positive multiple of fragments")
    rng = random.Random(params.seed)
    sizes = [rng.randint(800, 4000) for _ in range(params.samples)]
    per = params.samples // params.fragments
    moov = _build_moov(sizes, per)
    fragments: list[bytes] = []
    moof_sizes: list[int] = []
    for frag in range(params.fragments):
        chunk = sizes[frag * per:(frag + 1) * per]
        payload = rng.randbytes(sum(chunk))
        data, moof_size = _build_fragment(
            frag + 1, frag * per * FRAME_DURATION, chunk, payload)
        fragments.append(data)
        moof_sizes.append(moof_size)
    refs = [(0, len(data), per * FRAME_DURATION) for data in fragments]
    sidx = build_sidx(1, refs)
    ftyp = _box(b"ftyp", b"isom" + struct.pack(">I", 0) + b"isomiso6")
    head_len = len(ftyp) + len(moov) + len(sidx)
    stco_offsets = []
    pos = head_len
    for data, moof_size in zip(fragments, moof_sizes):
        stco_offsets.append(pos + moof_size + 8)
        pos += len(data)
    moov = bytearray(moov)
    idx = moov.find(b"stco")
    if idx < 0:
        raise ValueError("internal error: stco not found in moov")
    entry_pos = idx + 12
    for chunk_offset in stco_offsets:
        moov[entry_pos:entry_pos + 4] = struct.pack(">I", chunk_offset)
        entry_pos += 4
    blob = bytearray(bytes(ftyp) + bytes(moov) + sidx + b"".join(fragments))
    return bytes(_inject_corruption(blob, params, moof_sizes[0]))


def _inject_corruption(blob: bytearray, params: FixtureParams,
                       first_moof_size: int) -> bytearray:
    kind = params.corrupt
    if kind is None:
        return blob
    if kind == "truncate":
        return blob[: int(len(blob) * 0.6)]
    if kind == "stsc":
        idx = blob.find(b"stsc")
        struct.pack_into(">I", blob, idx + 12, params.fragments + 2)
        return blob
    sidx_idx = blob.find(b"sidx")
    box_start = sidx_idx - 4
    if kind == "offset":
        # first_offset lands inside the first mdat payload
        struct.pack_into(">Q", blob, box_start + 28, first_moof_size + 16)
        return blob
    if kind == "duration":
        # last subsegment one frame short vs the stbl timeline
        entries_start = box_start + 40
        last_duration = entries_start + (params.fragments - 1) * 12 + 4
        struct.pack_into(">I", blob, last_duration,
                         params.samples // params.fragments * FRAME_DURATION
                         - FRAME_DURATION)
        return blob
    raise ValueError(f"unknown corrupt kind: {kind}")


def write_fixture(path, params: FixtureParams) -> None:
    with open(path, "wb") as fh:
        fh.write(generate(params))
