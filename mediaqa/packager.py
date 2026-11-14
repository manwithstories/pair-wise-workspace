"""Delivery package assembly: stream-copy MP4 tracks, normalize WAV, roundtrip."""

import hashlib
import json
import os
import struct

from . import extract
from .consistency import WHITELIST

IDENTITY_MATRIX = [0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000]


class PackRejected(Exception):
    rule_id = "PACK_INCOMPATIBLE"


class RoundtripMismatch(Exception):
    rule_id = "ROUNDTRIP_MISMATCH"


# ---------------------------------------------------------------- MP4 writer

def _box(typ, payload):
    if isinstance(typ, str):
        typ = typ.encode("latin1")
    return struct.pack(">I4s", 8 + len(payload), typ) + payload


def _fullbox(typ, version, flags, payload=b""):
    return _box(typ, bytes((version,)) + flags.to_bytes(3, "big") + payload)


def _mvhd(timescale, duration, next_track_id):
    body = struct.pack(">IIII", 0, 0, timescale, duration)
    body += struct.pack(">IHI", 0x10000, 0x0100, 0) + b"\0" * 6
    body += struct.pack(">9i", *IDENTITY_MATRIX) + b"\0" * 24
    return _fullbox("mvhd", 0, 0, body + struct.pack(">I", next_track_id))


def _tkhd(track_id, duration, width, height, matrix, volume):
    body = struct.pack(">II", 0, 0) + struct.pack(">I", track_id)
    body += b"\0" * 4 + struct.pack(">I", duration) + b"\0" * 8
    body += struct.pack(">hhhh", 0, 0, volume, 0)
    body += struct.pack(">9i", *matrix)
    body += struct.pack(">II", width << 16, height << 16)
    return _fullbox("tkhd", 0, 3, body)


def _mdhd(timescale, duration, language):
    code = 0x55C4
    if language and len(language) == 3 and language != "und":
        code = 0
        for shift, ch in zip((10, 5, 0), language):
            code |= (ord(ch) - 0x60) << shift
    return _fullbox("mdhd", 0, 0, struct.pack(">IIIIHH", 0, 0, timescale,
                                              duration, code, 0))


def _hdlr(handler, name):
    return _fullbox("hdlr", 0, 0, b"\0" * 4 + handler.encode("latin1")
                    + b"\0" * 12 + name.encode() + b"\0")


def _stsd(codec, kind, channels=0, bits=0, rate=0):
    body = b"\0" * 6 + struct.pack(">H", 1)
    if kind == "audio":
        body += struct.pack(">HHI", 0, 0, 0)
        body += struct.pack(">HHHHI", channels, bits, 0, 0, rate << 16)
    entry = struct.pack(">I4s", 8 + len(body), codec.encode("latin1")) + body
    return _fullbox("stsd", 0, 0, struct.pack(">I", 1) + entry)


def _stts(count, ticks):
    if count < 1:
        entries = []
    elif count == 1:
        entries = [(1, ticks)]
    else:
        delta, rem = divmod(ticks, count)
        entries = [(count, delta)] if rem == 0 else [(count - 1, delta),
                                                     (1, delta + rem)]
    body = struct.pack(">I", len(entries))
    return _fullbox("stts", 0, 0, body + b"".join(
        struct.pack(">II", c, d) for c, d in entries))


def _stsz(sizes):
    if sizes and len(set(sizes)) == 1:
        body = struct.pack(">II", sizes[0], len(sizes))
    else:
        body = struct.pack(">II", 0, len(sizes))
        body += b"".join(struct.pack(">I", s) for s in sizes)
    return _fullbox("stsz", 0, 0, body)


def _stco(offsets, co64=False):
    fmt, typ = (">Q", "co64") if co64 else (">I", "stco")
    body = struct.pack(">I", len(offsets))
    return _fullbox(typ, 0, 0, body + b"".join(
        struct.pack(fmt, o) for o in offsets))


def _elst(entries):
    body = struct.pack(">I", len(entries))
    for seg, media_time in entries:
        body += struct.pack(">Iihh", seg, media_time, 1, 0)
    return _fullbox("elst", 0, 0, body)


def _minf(kind, stbl):
    if kind == "video":
        header = _fullbox("vmhd", 0, 1, struct.pack(">H3H", 0, 0, 0, 0))
    elif kind == "audio":
        header = _fullbox("smhd", 0, 0, struct.pack(">hH", 0, 0))
    else:
        header = _fullbox("nmhd", 0, 0)
    dref = _fullbox("dref", 0, 0, struct.pack(">I", 1) + _fullbox("url ", 0, 1))
    return _box("minf", header + _box("dinf", dref) + stbl)


_HANDLERS = {"video": "vide", "audio": "soun", "subtitle": "text"}


def _trak(tb, mv_timescale):
    stbl = _box("stbl", _stsd(tb["codec"], tb["kind"], tb.get("channels", 0),
                tb.get("bits", 0), tb.get("sample_rate", 0))
                + _stts(tb["sample_count"],
                tb["ticks"]) + _fullbox("stsc", 0, 0, struct.pack(
                    ">IIII", 1, 1, max(tb["sample_count"], 1), 1))
                + _stsz(tb["sizes"]) + _stco(tb["offsets"], tb.get("co64")))
    handler = _HANDLERS.get(tb["kind"], "meta")
    mdia = _box("mdia", _mdhd(tb["timescale"], tb["ticks"], tb.get("language"))
                + _hdlr(handler, tb["kind"]) + _minf(tb["kind"], stbl))
    mv_dur = tb["ticks"] * mv_timescale // max(tb["timescale"], 1)
    body = _tkhd(tb["track_id"], mv_dur, tb.get("width", 0), tb.get("height", 0),
                 tb.get("matrix") or IDENTITY_MATRIX,
                 0x0100 if tb["kind"] == "audio" else 0)
    if tb.get("elst"):
        body += _box("edts", _elst(tb["elst"]))
    return _box("trak", body + mdia), mv_dur


def write_mp4(path, tracks, brand="isom", mv_timescale=1000, moov_first=False,
              co64=False, mdat_largesize=False):
    """Write an MP4; each track dict carries codec/timescale/ticks/sizes/data."""
    brands = dict.fromkeys([brand, "isom", "mp42"])
    ftyp = _box("ftyp", brand.encode("latin1") + struct.pack(">I", 0x200)
                + b"".join(b.encode("latin1") for b in brands))
    moov = b""
    for _ in range(2):
        pos = len(ftyp) + (len(moov) if moov_first else 0)
        pos += 16 if mdat_largesize else 8
        for tb in tracks:
            tb["offsets"] = [pos]
            tb["co64"] = co64
            pos += len(tb["data"])
        traks = b""
        mv_dur = 0
        for tb in tracks:
            box_bytes, dur = _trak(tb, mv_timescale)
            traks += box_bytes
            mv_dur = max(mv_dur, dur)
        moov = _box("moov", _mvhd(mv_timescale, mv_dur, len(tracks) + 1) + traks)
    payload = b"".join(tb["data"] for tb in tracks)
    if mdat_largesize:
        mdat = struct.pack(">I4sQ", 1, b"mdat", 16 + len(payload)) + payload
    else:
        mdat = _box(b"mdat", payload)
    body = ftyp + (moov if moov_first else b"") + mdat
    body += b"" if moov_first else moov
    with open(path, "wb") as fh:
        fh.write(body)
    return len(body)


# ---------------------------------------------------------------- WAV writer

def wav_bytes(sample_rate, channels, bits, frames_data, extensible=False,
              fact_frames=None, extra_chunks=()):
    block = channels * max(bits, 1) // 8
    fmt = struct.pack("<HHIIHH", 1, channels, sample_rate,
                      sample_rate * block, block, bits)
    if extensible:
        guid = bytes.fromhex("0100000000001000800000aa00389b71")
        fmt = struct.pack("<HHIIHHH", 0xFFFE, channels, sample_rate,
                          sample_rate * block, block, bits, 22)
        fmt += struct.pack("<HI", bits, 0) + guid
    chunks = [(b"fmt ", fmt)]
    if fact_frames is not None:
        chunks.append((b"fact", struct.pack("<I", fact_frames)))
    chunks += list(extra_chunks)
    chunks.append((b"data", frames_data))
    payload = b""
    for cid, data in chunks:
        payload += cid + struct.pack("<I", len(data)) + data
        payload += b"\0" * (len(data) & 1)
    return b"RIFF" + struct.pack("<I", 4 + len(payload)) + b"WAVE" + payload


def write_wav(path, sample_rate, channels, bits, frames_data, **kw):
    with open(path, "wb") as fh:
        fh.write(wav_bytes(sample_rate, channels, bits, frames_data, **kw))


# ------------------------------------------------------------ orchestration

def _whitelist_ok(track):
    return (track.sample_rate == WHITELIST["sample_rate"]
            and track.channels == WHITELIST["channels"]
            and track.bits_per_sample == WHITELIST["bits_per_sample"]
            and track.codec == WHITELIST["codec"])


def _read_samples(track):
    sizes = track.extra.get("stsz") or []
    offsets = track.extra.get("stco") or []
    if not sizes or not offsets:
        return b""
    with open(track.path, "rb") as fh:
        if len(offsets) == 1:
            fh.seek(offsets[0])
            return fh.read(sum(sizes))
        per_chunk = len(sizes) // len(offsets)
        out = b""
        for i, off in enumerate(offsets):
            fh.seek(off)
            out += fh.read(sum(sizes[i * per_chunk:(i + 1) * per_chunk]))
        return out


def _sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as fh:
        for block in iter(lambda: fh.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


def build_package(spec, spec_dir, out_dir):
    """Assemble a delivery package from a spec; returns roundtrip metrics."""
    target_us = spec.get("target_duration_us")
    selected = []
    for src in spec.get("sources", []):
        path = os.path.normpath(os.path.join(spec_dir, src["path"]))
        result = extract.extract_file(path)
        if not result.ok:
            raise PackRejected("source unreadable: %s" % result.error)
        indexes = src.get("tracks", list(range(len(result.tracks))))
        for i in indexes:
            selected.append(result.tracks[i])
    if not selected:
        raise PackRejected("spec selects no tracks")
    if target_us is None:
        target_us = max(t.duration_us for t in selected)
    for t in selected:
        if t.kind == "audio" and not _whitelist_ok(t):
            raise PackRejected(
                "audio track %d of %s outside whitelist (%s)"
                % (t.track_index, t.path, t.summary()))

    os.makedirs(out_dir, exist_ok=True)
    written = []
    mp4_tracks = [t for t in selected if t.container in ("mp4", "mov", "m4a")]
    if mp4_tracks:
        builds = []
        for i, t in enumerate(mp4_tracks, 1):
            builds.append({
                "track_id": i, "kind": t.kind, "codec": t.codec,
                "timescale": t.timescale or 1000,
                "ticks": target_us * (t.timescale or 1000) // 1_000_000,
                "sample_count": t.sample_count, "sizes": t.extra["stsz"],
                "data": _read_samples(t), "language": t.language,
                "width": t.width, "height": t.height,
                "sample_rate": t.sample_rate, "channels": t.channels,
                "bits": t.bits_per_sample,
                "matrix": t.extra.get("matrix"),
            })
        out_mp4 = os.path.join(out_dir, "package.mp4")
        write_mp4(out_mp4, builds, brand="isom")
        written.append(out_mp4)
    for t in selected:
        if t.container != "wav":
            continue
        rate, block = t.sample_rate, t.extra["block_align"]
        frames = target_us * rate // 1_000_000
        with open(t.path, "rb") as fh:
            fh.seek(t.extra["data_offset"])
            data = fh.read(min(t.extra["data_size"], frames * block))
        data += b"\0" * (frames * block - len(data))
        out_wav = os.path.join(out_dir, os.path.basename(t.path))
        write_wav(out_wav, rate, t.channels, t.bits_per_sample, data)
        written.append(out_wav)

    manifest = {"package_id": spec.get("package_id", "package"),
                    "target_duration_us": target_us,
                    "files": [{"path": os.path.basename(p), "sha256": _sha256(p)}
                              for p in written]}
    with open(os.path.join(out_dir, "manifest.json"), "w") as fh:
        json.dump(manifest, fh, indent=2)

    back = []
    for p in written:
        result = extract.extract_file(p)
        if not result.ok:
            raise RoundtripMismatch("re-extract failed: %s" % result.error)
        back.extend(result.tracks)
    if len(back) != len(selected):
        raise RoundtripMismatch("track count %d != %d"
                                % (len(back), len(selected)))
    for t in back:
        tolerance = max(t.sample_duration_us, 1)
        if abs(t.duration_us - target_us) > tolerance:
            raise RoundtripMismatch(
                "track %d duration %dus off target by >1 sample"
                % (t.track_index, t.duration_us))
    durations = [t.duration_us for t in back]
    return {"roundtrip": "ok", "tracks": len(back),
            "duration_delta_us": max(durations) - min(durations),
            "target_duration_us": target_us,
            "files": [os.path.basename(p) for p in written],
            "sha256": _sha256(os.path.join(out_dir, "manifest.json"))}
