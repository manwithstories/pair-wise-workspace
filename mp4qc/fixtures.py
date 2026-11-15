"""Deterministic sample-media generators; no external files needed."""
from __future__ import annotations

import os
import random
import struct

from .sidx import Sidx, SidxReference, build_sidx

__all__ = ["write_fixtures"]


def _box(type4, payload):
    return struct.pack(">I4s", 8 + len(payload), type4.encode("ascii")) + payload


def _fullbox(type4, version, flags, payload):
    head = struct.pack(">B", version) + flags.to_bytes(3, "big")
    return _box(type4, head + payload)


def _stts(durations):
    entries = []
    for delta in durations:
        if entries and entries[-1][1] == delta:
            entries[-1][0] += 1
        else:
            entries.append([1, delta])
    payload = struct.pack(">I", len(entries))
    for count, delta in entries:
        payload += struct.pack(">II", count, delta)
    return _fullbox("stts", 0, 0, payload)


def _ctts(offsets, version=0):
    entries = []
    for off in offsets:
        if entries and entries[-1][1] == off:
            entries[-1][0] += 1
        else:
            entries.append([1, off])
    fmt = ">i" if version == 1 else ">I"
    payload = struct.pack(">I", len(entries))
    for count, off in entries:
        payload += struct.pack(">I", count) + struct.pack(fmt, off)
    return _fullbox("ctts", version, 0, payload)


def _stsc(spc_runs):
    payload = struct.pack(">I", len(spc_runs))
    for first, spc in spc_runs:
        payload += struct.pack(">III", first, spc, 1)
    return _fullbox("stsc", 0, 0, payload)


def _stsz(sizes):
    payload = struct.pack(">II", 0, len(sizes))
    payload += b"".join(struct.pack(">I", s) for s in sizes)
    return _fullbox("stsz", 0, 0, payload)


def _stz2(sizes, field_size):
    if field_size == 4:
        packed = bytearray()
        for i in range(0, len(sizes), 2):
            hi = sizes[i]
            lo = sizes[i + 1] if i + 1 < len(sizes) else 0
            packed.append((hi << 4) | lo)
        data = bytes(packed)
    elif field_size == 8:
        data = bytes(sizes)
    elif field_size == 16:
        data = b"".join(struct.pack(">H", s) for s in sizes)
    else:
        raise ValueError("bad stz2 field_size %r" % field_size)
    payload = b"\x00\x00\x00" + struct.pack(">B", field_size)
    payload += struct.pack(">I", len(sizes)) + data
    return _fullbox("stz2", 0, 0, payload)


def _stco(offsets, co64=False):
    if co64:
        payload = struct.pack(">I", len(offsets))
        payload += b"".join(struct.pack(">Q", o) for o in offsets)
        return _fullbox("co64", 0, 0, payload)
    payload = struct.pack(">I", len(offsets))
    payload += b"".join(struct.pack(">I", o) for o in offsets)
    return _fullbox("stco", 0, 0, payload)


def _stss(sync):
    payload = struct.pack(">I", len(sync))
    payload += b"".join(struct.pack(">I", s) for s in sync)
    return _fullbox("stss", 0, 0, payload)


def _stsd(codec):
    entry = struct.pack(">I4s", 86, codec.encode("ascii"))
    entry += b"\x00" * 6 + struct.pack(">H", 1) + b"\x00" * 70
    return _fullbox("stsd", 0, 0, struct.pack(">I", 1) + entry)


def _stbl(track, chunk_offsets):
    out = _stsd(track["codec"])
    out += _stts(track["durations"])
    if "ctts" in track:
        out += _ctts(track["ctts"], track.get("ctts_version", 0))
    out += _stsc(track["spc_runs"])
    if track.get("stz2"):
        out += _stz2(track["sizes"], track["stz2"])
    else:
        out += _stsz(track["sizes"])
    out += _stco(chunk_offsets, track.get("co64", False))
    if track.get("sync"):
        out += _stss(track["sync"])
    return _box("stbl", out)


def _mvhd(timescale, duration):
    payload = struct.pack(">IIII", 0, 0, timescale, duration)
    payload += struct.pack(">I", 0x00010000) + struct.pack(">H", 0x0100)
    payload += b"\x00" * 10 + b"\x00" * 36 + b"\x00" * 24
    return _fullbox("mvhd", 0, 0, payload + struct.pack(">I", 3))


def _tkhd(track_id, duration):
    payload = struct.pack(">IIIII", 0, 0, track_id, 0, duration)
    payload += b"\x00" * 8 + struct.pack(">hhhh", 0, 0, 0, 0)
    payload += b"\x00" * 36 + struct.pack(">II", 0, 0)
    return _fullbox("tkhd", 0, 7, payload)


def _mdhd(timescale, duration):
    payload = struct.pack(">IIII", 0, 0, timescale, duration)
    return _fullbox("mdhd", 0, 0, payload + struct.pack(">HH", 0x55C4, 0))


def _hdlr(handler):
    payload = struct.pack(">I4s", 0, handler.encode("ascii"))
    return _fullbox("hdlr", 0, 0, payload + b"\x00" * 12 + b"mp4qc\x00")


def _minf(track, chunk_offsets):
    if track.get("handler", "vide") == "vide":
        header = _fullbox("vmhd", 0, 1, struct.pack(">HHHH", 0, 0, 0, 0))
    else:
        header = _fullbox("smhd", 0, 0, struct.pack(">Hh", 0, 0))
    url = _fullbox("url ", 0, 1, b"")
    dinf = _box("dinf", _fullbox("dref", 0, 0, struct.pack(">I", 1) + url))
    return _box("minf", header + dinf + _stbl(track, chunk_offsets))


def _trak(track, chunk_offsets):
    duration = sum(track["durations"])
    mdia = _box("mdia", _mdhd(track.get("timescale", 90000), duration)
                + _hdlr(track.get("handler", "vide"))
                + _minf(track, chunk_offsets))
    return _box("trak", _tkhd(track["track_id"], duration) + mdia)


def _expand_spc(spc_runs, sample_count):
    per_chunk = []
    total = 0
    run = 0
    while total < sample_count:
        chunk_no = len(per_chunk) + 1
        while run + 1 < len(spc_runs) and spc_runs[run + 1][0] <= chunk_no:
            run += 1
        spc = spc_runs[run][1]
        per_chunk.append(spc)
        total += spc
    if total != sample_count:
        raise ValueError("spc runs sum %d != sample count %d"
                         % (total, sample_count))
    return per_chunk


def _build_regular_mp4(tracks, seed):
    ftyp = _box("ftyp", b"isom" + struct.pack(">I", 0x200) + b"isomiso6dash")
    rng = random.Random(seed)
    track_data = [b"".join(rng.randbytes(s) for s in t["sizes"])
                  for t in tracks]

    def build_moov(offsets):
        out = _mvhd(90000, max(sum(t["durations"]) for t in tracks))
        for track, offs in zip(tracks, offsets):
            out += _trak(track, offs)
        return _box("moov", out)

    offsets = [[0] * len(_expand_spc(t["spc_runs"], len(t["sizes"])))
               for t in tracks]
    moov = build_moov(offsets)
    pos = len(ftyp) + len(moov) + 8  # mdat header
    for ti, track in enumerate(tracks):
        per_chunk = _expand_spc(track["spc_runs"], len(track["sizes"]))
        off = pos
        idx = 0
        for ci, spc in enumerate(per_chunk):
            offsets[ti][ci] = off
            off += sum(track["sizes"][idx:idx + spc])
            idx += spc
        pos += len(track_data[ti])
    moov = build_moov(offsets)
    return ftyp + moov + _box("mdat", b"".join(track_data))


def _valid_dash(seed):
    rng = random.Random(seed)
    sizes = [rng.randint(180, 480) for _ in range(1480)]
    durations = [1024] * 1480
    for i in range(1168, 1480):  # 312 stts runs in total
        durations[i] = 1024 if (i - 1168) % 2 == 0 else 1536
    track = {
        "track_id": 1, "codec": "avc1", "sizes": sizes,
        "durations": durations,
        "ctts": [512 if i % 3 == 1 else 0 for i in range(1480)],
        "spc_runs": [(1, 37), (11, 36), (21, 38), (31, 37), (36, 37)],
        "sync": list(range(1, 1481, 37)),
    }
    return _build_regular_mp4([track], seed)


def _multitrack(seed):
    rng = random.Random(seed + 1)
    video = {
        "track_id": 1, "codec": "avc1",
        "sizes": [rng.randint(150, 400) for _ in range(600)],
        "durations": [1024] * 600, "spc_runs": [(1, 25)],
        "sync": list(range(1, 601, 25)),
    }
    audio = {
        "track_id": 2, "codec": "encv", "handler": "soun",
        "sizes": [rng.randint(40, 120) for _ in range(480)],
        "durations": [512] * 480, "spc_runs": [(1, 20)],
    }
    return _build_regular_mp4([video, audio], seed + 1)


def _co64(seed):
    rng = random.Random(seed + 2)
    track = {
        "track_id": 1, "codec": "avc1", "co64": True,
        "sizes": [rng.randint(200, 500) for _ in range(240)],
        "durations": [1024] * 240, "spc_runs": [(1, 30)],
    }
    return _build_regular_mp4([track], seed + 2)


def _stz2_file(seed):
    rng = random.Random(seed + 3)
    ctts = [0] * 100
    ctts[3] = -64  # signed composition offset (ctts version 1)
    ctts[7] = -64
    track = {
        "track_id": 1, "codec": "avc1", "stz2": 8,
        "sizes": [rng.randint(3, 200) for _ in range(100)],
        "durations": [1024] * 100, "spc_runs": [(1, 10)],
        "ctts": ctts, "ctts_version": 1,
    }
    return _build_regular_mp4([track], seed + 3)


def _build_moof(seq, track_id, base_time, sizes, default_duration):
    mfhd = _fullbox("mfhd", 0, 0, struct.pack(">I", seq))
    tfhd = _fullbox("tfhd", 0, 0x020008,
                    struct.pack(">II", track_id, default_duration))
    tfdt = _fullbox("tfdt", 1, 0, struct.pack(">Q", base_time))
    trun_size = 8 + 4 + 4 + 4 + 4 * len(sizes)
    moof_size = 8 + len(mfhd) + 8 + len(tfhd) + len(tfdt) + trun_size
    data_offset = moof_size + 8  # past this moof + mdat header
    trun = _fullbox("trun", 0, 0x201,
                    struct.pack(">Ii", len(sizes), data_offset)
                    + b"".join(struct.pack(">I", s) for s in sizes))
    traf = _box("traf", tfhd + tfdt + trun)
    return _box("moof", mfhd + traf)


def _fragmented_moov():
    stbl = _box("stbl", _stsd("avc1") + _stts([]) + _stsc([])
                + _stsz([]) + _stco([]))
    url = _fullbox("url ", 0, 1, b"")
    dinf = _box("dinf", _fullbox("dref", 0, 0, struct.pack(">I", 1) + url))
    minf = _box("minf", _fullbox("vmhd", 0, 1, struct.pack(">HHHH", 0, 0, 0, 0))
                + dinf + stbl)
    mdia = _box("mdia", _mdhd(90000, 0) + _hdlr("vide") + minf)
    trak = _box("trak", _tkhd(1, 0) + mdia)
    trex = _fullbox("trex", 0, 0, struct.pack(">IIIII", 1, 1, 1024, 0, 0))
    return _box("moov", _mvhd(90000, 0) + trak + _box("mvex", trex))


def _drift_sidx(seed):
    rng = random.Random(seed + 4)
    ftyp = _box("ftyp", b"isom" + struct.pack(">I", 0x200) + b"isomiso6dashmp41")
    moov = _fragmented_moov()
    segments = []
    base_time = 0
    for seq in range(1, 38):  # 37 subsegments
        sizes = [rng.randint(200, 600) for _ in range(40)]
        moof = _build_moof(seq, 1, base_time, sizes, 1024)
        payload = rng.randbytes(sum(sizes))
        segments.append((moof, payload))
        base_time += 40 * 1024
    true_sizes = [len(moof) + 8 + len(payload) for moof, payload in segments]
    # Encoder restart corrupted the index: drift grows towards the tail,
    # references 25..36 (0-based) miss their moof, max drift 4864 bytes.
    drifted = list(true_sizes)
    prev = 0
    for k in range(12):
        cum = round(4864 * (k + 1) / 12)
        drifted[24 + k] += cum - prev
        prev = cum
    free_box = _box("free", b"\x00" * 504)  # 512 bytes of stale padding
    refs = [SidxReference(0, size, 40960, True, 1, 0) for size in drifted]
    sidx = build_sidx(Sidx(0, 1, 90000, 0, len(free_box), refs))
    out = bytearray(ftyp + moov + sidx + free_box)
    for moof, payload in segments:
        out += moof + _box("mdat", payload)
    return bytes(out)


def write_fixtures(out_dir, seed=7):
    """Generate all six fixture files; returns the written paths."""
    os.makedirs(out_dir, exist_ok=True)
    valid = _valid_dash(seed)
    files = {
        "valid_dash.mp4": valid,
        "drift_sidx.mp4": _drift_sidx(seed),
        "truncated.mp4": valid[:int(len(valid) * 0.55)],
        "multitrack.mp4": _multitrack(seed),
        "co64.mp4": _co64(seed),
        "stz2.mp4": _stz2_file(seed),
    }
    paths = []
    for name, data in files.items():
        path = os.path.join(out_dir, name)
        with open(path, "wb") as fh:
            fh.write(data)
        paths.append(path)
    return paths

