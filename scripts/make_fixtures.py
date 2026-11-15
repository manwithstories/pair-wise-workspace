#!/usr/bin/env python3
"""Generate deterministic byte-level media fixtures without external assets."""

from __future__ import annotations

import argparse
import random
import struct
from pathlib import Path


class BitWriter:
    def __init__(self) -> None:
        self.bits: list[int] = []

    def write(self, value: int, count: int) -> None:
        for shift in range(count - 1, -1, -1):
            self.bits.append((value >> shift) & 1)

    def ue(self, value: int) -> None:
        code = value + 1
        width = code.bit_length()
        self.write(0, width - 1)
        self.write(code, width)

    def se(self, value: int) -> None:
        self.ue(value * 2 - 1 if value > 0 else -value * 2)

    def finish(self) -> bytes:
        self.bits.append(1)
        while len(self.bits) % 8:
            self.bits.append(0)
        out = bytearray()
        for index in range(0, len(self.bits), 8):
            byte = 0
            for bit in self.bits[index:index + 8]:
                byte = (byte << 1) | bit
            out.append(byte)
        return bytes(out)


def escape_rbsp(data: bytes) -> bytes:
    out = bytearray()
    zeros = 0
    for byte in data:
        if zeros >= 2 and byte <= 3:
            out.append(3)
            zeros = 0
        out.append(byte)
        zeros = zeros + 1 if byte == 0 else 0
    return bytes(out)


def make_sps(width: int = 320, height: int = 180) -> bytes:
    bits = BitWriter()
    bits.write(66, 8)
    bits.write(0, 8)
    bits.write(31, 8)
    bits.ue(0)
    bits.ue(0)
    bits.ue(0)
    bits.ue(0)
    bits.ue(1)
    bits.write(0, 1)
    bits.ue((width + 15) // 16 - 1)
    bits.ue((height + 15) // 16 - 1)
    bits.write(1, 1)
    bits.write(1, 1)
    crop = (height + 15) // 16 * 16 - height
    if crop:
        bits.write(1, 1)
        bits.ue(0)
        bits.ue(0)
        bits.ue(crop // 2)
        bits.ue(0)
    else:
        bits.write(0, 1)
    bits.write(0, 1)
    return b"\x67" + escape_rbsp(bits.finish())


def make_pps() -> bytes:
    bits = BitWriter()
    bits.ue(0)
    bits.ue(0)
    bits.write(0, 1)
    bits.write(0, 1)
    bits.ue(0)
    bits.ue(0)
    bits.ue(0)
    bits.write(0, 1)
    bits.write(0, 2)
    bits.se(0)
    bits.se(0)
    bits.se(0)
    bits.write(1, 1)
    bits.write(0, 1)
    bits.write(0, 1)
    return b"\x68" + escape_rbsp(bits.finish())


def box(kind: bytes, payload: bytes, large: bool = False) -> bytes:
    if large:
        return struct.pack(">I4sQ", 1, kind, 16 + len(payload)) + payload
    return struct.pack(">I4s", 8 + len(payload), kind) + payload


def fullbox(kind: bytes, version: int, flags: int, payload: bytes) -> bytes:
    return box(kind, bytes([version]) + flags.to_bytes(3, "big") + payload)


def descriptor(tag: int, payload: bytes) -> bytes:
    length = len(payload)
    encoded = bytearray([length & 0x7F])
    length >>= 7
    while length:
        encoded.insert(0, 0x80 | (length & 0x7F))
        length >>= 7
    return bytes([tag]) + bytes(encoded) + payload


def make_esds() -> bytes:
    asc = struct.pack(">H", (2 << 11) | (3 << 7) | (2 << 3))
    dsi = descriptor(0x05, asc)
    decoder = descriptor(
        0x04, b"\x40\x15\x00\x00\x02\x00\x00\x02\x00\x00\x00\x00\x00" + dsi
    )
    es = descriptor(0x03, b"\x00\x01\x00" + decoder)
    return fullbox(b"esds", 0, 0, es)


def make_avcc(sps_count: int = 1, missing_sps: bool = False) -> bytes:
    sps = make_sps()
    pps = make_pps()
    entries = b""
    if not missing_sps:
        for _ in range(sps_count):
            entries += struct.pack(">H", len(sps)) + sps
    entries += b"\x01" + struct.pack(">H", len(pps)) + pps
    count_byte = 0xE0 | (0 if missing_sps else sps_count)
    return bytes([1, 66, 0, 31, 0xFF, count_byte]) + entries


def video_entry(avcc: bytes) -> bytes:
    payload = (
        b"\x00" * 6 + struct.pack(">H", 1)
        + struct.pack(">HHIII", 0, 0, 0, 0, 0)
        + struct.pack(">HH", 320, 180)
        + struct.pack(">III", 0x00480000, 0x00480000, 0)
        + struct.pack(">H", 1) + b"\x00" * 32 + struct.pack(">HH", 24, 0xFFFF)
        + box(b"avcC", avcc)
    )
    return box(b"avc1", payload)


def audio_entry() -> bytes:
    payload = (
        b"\x00" * 6 + struct.pack(">H", 1)
        + struct.pack(">HHIHHHH", 0, 0, 0, 2, 16, 0, 0)
        + struct.pack(">I", 48000 << 16) + make_esds()
    )
    return box(b"mp4a", payload)


def make_mvhd(movie_ts: int, duration: int) -> bytes:
    payload = struct.pack(">IIII", 0, 0, movie_ts, duration)
    payload += struct.pack(">I", 0x00010000) + struct.pack(">H", 0x0100) + b"\x00" * 10
    payload += struct.pack(">9I", 0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000)
    payload += b"\x00" * 24 + struct.pack(">I", 3)
    return fullbox(b"mvhd", 0, 0, payload)


def make_tkhd(track_id: int, duration: int, width: int = 0, height: int = 0) -> bytes:
    payload = struct.pack(">IIIII", 0, 0, track_id, 0, duration)
    payload += b"\x00" * 8 + struct.pack(">HHHH", 0, 0, 0 if width else 0x0100, 0)
    payload += struct.pack(">9I", 0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000)
    payload += struct.pack(">II", width << 16, height << 16)
    return fullbox(b"tkhd", 0, 7, payload)


def make_mdhd(timescale: int, duration: int) -> bytes:
    payload = struct.pack(">IIII", 0, 0, timescale, duration)
    payload += struct.pack(">HH", 0x55C4, 0)
    return fullbox(b"mdhd", 0, 0, payload)


def make_hdlr(kind: str) -> bytes:
    handler = {"video": b"vide", "audio": b"soun"}[kind]
    name = b"VideoHandler\x00" if kind == "video" else b"SoundHandler\x00"
    return fullbox(b"hdlr", 0, 0, b"\x00" * 4 + handler + b"\x00" * 12 + name)


def make_stbl(kind: str, spec: dict, chunk_offset: int) -> bytes:
    entry = video_entry(make_avcc(spec.get("sps_count", 1), spec.get("missing_sps", False))) \
        if kind == "video" else audio_entry()
    stsd = fullbox(b"stsd", 0, 0, struct.pack(">I", 1) + entry)
    stts_count = spec.get("stts_override", spec["samples"])
    stts = fullbox(b"stts", 0, 0, struct.pack(">III", 1, stts_count, spec["delta"]))
    stsc = fullbox(b"stsc", 0, 0, struct.pack(">IIII", 1, 1, max(spec["samples"], 1), 1))
    stsz = fullbox(b"stsz", 0, 0, struct.pack(">II", spec["sample_size"], spec["samples"]))
    if spec.get("co64"):
        stco = fullbox(b"co64", 0, 0, struct.pack(">IQ", 1, chunk_offset))
    else:
        stco = fullbox(b"stco", 0, 0, struct.pack(">II", 1, chunk_offset))
    parts = [stsd, stts, stsc, stsz, stco]
    if spec.get("ctts") is not None:
        parts.append(fullbox(b"ctts", 1, 0,
                             struct.pack(">IIi", 1, spec["samples"], spec["ctts"])))
    if kind == "video" and spec.get("stss") is not None:
        sync = spec["stss"]
        parts.append(fullbox(b"stss", 0, 0,
                             struct.pack(">I", len(sync)) + b"".join(
                                 struct.pack(">I", number) for number in sync)))
    return box(b"stbl", b"".join(parts))


def make_trak(spec: dict, movie_ts: int, chunk_offset: int) -> bytes:
    kind = spec["kind"]
    timescale = 0 if spec.get("zero_timescale") else spec["timescale"]
    duration = spec["samples"] * spec["delta"]
    mdhd = make_mdhd(timescale, duration)
    hdlr = make_hdlr(kind)
    if kind == "video":
        minf_header = fullbox(b"vmhd", 0, 1, b"\x00" * 8)
    else:
        minf_header = fullbox(b"smhd", 0, 0, b"\x00" * 4)
    dinf = box(b"dinf", fullbox(b"dref", 0, 0, b"\x00" * 4))
    stbl = make_stbl(kind, spec, chunk_offset)
    minf = box(b"minf", minf_header + dinf + stbl)
    mdia = box(b"mdia", mdhd + hdlr + minf)
    edits = spec.get("edits")
    edts = b""
    if edits:
        body = struct.pack(">I", len(edits))
        for segment, media_time in edits:
            body += struct.pack(">IiHH", segment, media_time, 1, 0)
        edts = box(b"edts", fullbox(b"elst", 0, 0, body))
    duration_movie = spec.get("movie_duration", duration)
    return box(b"trak", make_tkhd(spec["id"], duration_movie,
                                  spec.get("width", 0), spec.get("height", 0)) + edts + mdia)


def make_ftyp(ext: str) -> bytes:
    major = b"qt  " if ext == ".mov" else b"mp42"
    return box(b"ftyp", major + b"\x00\x00\x00\x00" + major + b"isomiso2mp41")


def base_specs(audio_samples: int = 500, video_samples: int = 300) -> list[dict]:
    video_sync = list(range(1, video_samples + 1, 30))
    return [
        {"id": 1, "kind": "video", "timescale": 90000, "delta": 3000,
         "samples": video_samples, "sample_size": 128, "width": 320, "height": 180,
         "stss": video_sync},
        {"id": 2, "kind": "audio", "timescale": 48000, "delta": 960,
         "samples": audio_samples, "sample_size": 96},
    ]


def build_mp4(specs: list[dict], ext: str = ".mp4", prefix_free: bool = False,
              mdat_zero: bool = False, moof: bool = False) -> bytes:
    ftyp = make_ftyp(ext)
    prefix = box(b"free", b"") if prefix_free else b""
    dummy = b"".join(make_trak(spec, 1000, 0) for spec in specs)
    moov_size = len(box(b"moov", make_mvhd(1000, 10000) + dummy))
    header_size = 8 if not any(spec.get("large_mdat") for spec in specs) else 16
    payload_start = len(prefix) + len(ftyp) + moov_size + header_size
    offsets: list[int] = []
    cursor = payload_start
    for spec in specs:
        offsets.append(cursor)
        cursor += spec["samples"] * spec["sample_size"]
    for index, spec in enumerate(specs):
        if spec.get("bad_offset"):
            offsets[index] += spec["bad_offset"]
        if spec.get("overlap") and index > 0:
            offsets[index] = offsets[0]
    traks = b"".join(make_trak(spec, 1000, offsets[index])
                     for index, spec in enumerate(specs))
    moov = box(b"moov", make_mvhd(1000, 10000) + traks)
    payload = b"".join(
        (bytes(range(128)) * (spec["samples"] * spec["sample_size"] // 128 + 1))[
            :spec["samples"] * spec["sample_size"]]
        for spec in specs
    )
    if mdat_zero:
        mdat = struct.pack(">I4s", 0, b"mdat") + payload
    elif any(spec.get("large_mdat") for spec in specs):
        mdat = box(b"mdat", payload, large=True)
    else:
        mdat = box(b"mdat", payload)
    output = prefix + ftyp + moov + mdat
    if moof:
        output += box(b"moof", fullbox(b"mfhd", 0, 0, struct.pack(">I", 1)))
    return output


def riff_chunk(kind: bytes, payload: bytes) -> bytes:
    return kind + struct.pack("<I", len(payload)) + payload + (b"\x00" if len(payload) & 1 else b"")


def make_wav(data_bytes: bytes, channels: int = 2, rate: int = 48000,
             bits: int = 16) -> bytes:
    block_align = channels * bits // 8
    fmt = struct.pack("<HHIIHH", 1, channels, rate, rate * block_align, block_align, bits)
    payload = b"WAVE" + riff_chunk(b"fmt ", fmt) + riff_chunk(b"data", data_bytes)
    return b"RIFF" + struct.pack("<I", len(payload)) + payload


def avi_strh(kind: bytes, handler: bytes, scale: int, rate: int, length: int,
             sample_size: int = 0, width: int = 0, height: int = 0) -> bytes:
    payload = kind + handler + struct.pack("<IHHIIIIIIII", 0, 0, 0, 0,
                                            scale, rate, 0, length, 0, 0, sample_size)
    payload += struct.pack("<hhhh", 0, 0, width, height)
    return riff_chunk(b"strh", payload)


def make_avi() -> bytes:
    avih = struct.pack("<14I", 33333, 0, 0, 0x10, 300, 0, 2, 0,
                       320, 180, 0, 0, 0, 0)
    video_strh = avi_strh(b"vids", b"H264", 1, 30, 300, 0, 320, 180)
    bitmap = struct.pack("<IiiHH4sIiiII", 40, 320, 180, 1, 24, b"H264",
                         320 * 180 * 3, 0, 0, 0, 0)
    audio_strh = avi_strh(b"auds", b"\x00\x00\x00\x00", 1, 48000, 480000, 4)
    wavefmt = struct.pack("<HHIIHH", 1, 2, 48000, 192000, 4, 16)
    hdrl = riff_chunk(b"LIST", b"hdrl" + riff_chunk(b"avih", avih)
                       + riff_chunk(b"LIST", b"strl" + video_strh
                                    + riff_chunk(b"strf", bitmap))
                       + riff_chunk(b"LIST", b"strl" + audio_strh
                                    + riff_chunk(b"strf", wavefmt)))
    movi = riff_chunk(b"LIST", b"movi")
    payload = b"AVI " + hdrl + movi
    return b"RIFF" + struct.pack("<I", len(payload)) + payload


def scenario_bytes(name: str, seed: int) -> bytes:
    rng = random.Random(seed)
    specs = base_specs()
    if name == "clean_h264_aac.mp4":
        return build_mp4(specs, prefix_free=True)
    if name == "clean_h264_aac.mov":
        return build_mp4(specs, ".mov")
    if name == "drift_audio.mp4":
        return build_mp4(base_specs(audio_samples=600))
    if name == "short_audio.mp4":
        return build_mp4(base_specs(audio_samples=400))
    if name == "co64_clean.mp4":
        specs[1]["co64"] = True
        specs[1]["large_mdat"] = True
        return build_mp4(specs)
    if name == "elst_trim.mp4":
        for spec in specs:
            spec["edits"] = [(8000, 0)]
            spec["movie_duration"] = 8000
        return build_mp4(specs)
    if name == "empty_edit.mp4":
        specs[0]["edits"] = [(500, -1), (9500, 0)]
        specs[1]["edits"] = [(10000, 0)]
        return build_mp4(specs)
    if name == "negative_ctts.mp4":
        specs[0]["ctts"] = -1
        return build_mp4(specs)
    if name == "stsz_stts_mismatch.mp4":
        specs[0]["stts_override"] = specs[0]["samples"] - 1
        return build_mp4(specs)
    if name == "bad_stco.mp4":
        specs[1]["bad_offset"] = 1_000_000
        return build_mp4(specs)
    if name == "chunk_overlap.mp4":
        specs[1]["overlap"] = True
        return build_mp4(specs)
    if name == "missing_sps.mp4":
        specs[0]["missing_sps"] = True
        return build_mp4(specs)
    if name == "moof_fragmented.mp4":
        return build_mp4(specs, moof=True)
    if name == "truncated_mdat.mp4":
        return build_mp4(specs)[:-2048]
    if name == "tail_garbage.mp4":
        return build_mp4(specs) + b"JUNK"
    if name == "zero_timescale.mp4":
        specs[0]["zero_timescale"] = True
        return build_mp4(specs)
    if name == "no_sync.mp4":
        specs[0]["stss"] = []
        return build_mp4(specs)
    if name == "multi_sps.mp4":
        specs[0]["sps_count"] = 2
        return build_mp4(specs)
    if name == "zero_size_mdat.mp4":
        return build_mp4(specs, mdat_zero=True)
    if name == "clean_audio.wav":
        return make_wav(bytes(rng.randrange(256) for _ in range(96000)))
    if name == "odd_data.wav":
        return make_wav(bytes(rng.randrange(256) for _ in range(1001)))
    if name == "clean_pcm.wav":
        return make_wav(bytes(rng.randrange(256) for _ in range(48000)), 1, 48000, 16)
    if name == "clean_video.avi":
        return make_avi()
    if name == "unsupported.bin":
        return b"NOTAMEDIA" + bytes(range(32))
    return build_mp4(specs)


def fixture_names() -> list[str]:
    return [
        "clean_h264_aac.mp4", "clean_h264_aac.mov", "drift_audio.mp4",
        "short_audio.mp4", "co64_clean.mp4", "elst_trim.mp4", "empty_edit.mp4",
        "negative_ctts.mp4", "stsz_stts_mismatch.mp4", "bad_stco.mp4",
        "chunk_overlap.mp4", "missing_sps.mp4", "moof_fragmented.mp4",
        "truncated_mdat.mp4", "tail_garbage.mp4", "zero_timescale.mp4",
        "no_sync.mp4", "multi_sps.mp4", "zero_size_mdat.mp4", "clean_audio.wav",
        "odd_data.wav", "clean_pcm.wav", "clean_video.avi", "unsupported.bin",
    ]


def generate(out_dir: Path, count: int, seed: int) -> list[Path]:
    out_dir.mkdir(parents=True, exist_ok=True)
    names = fixture_names()
    written: list[Path] = []
    for index in range(count):
        if index < len(names):
            name = names[index]
        else:
            rng = random.Random(seed + index)
            audio = rng.randint(60, 180)
            video = rng.randint(45, 90)
            name = f"bulk_{index:03d}.mp4"
            data = build_mp4(base_specs(audio, video))
            path = out_dir / name
            path.write_bytes(data)
            written.append(path)
            continue
        data = scenario_bytes(name, seed + index)
        path = out_dir / name
        path.write_bytes(data)
        written.append(path)
    return written


def main() -> int:
    parser = argparse.ArgumentParser(description="Generate mediaqc byte fixtures")
    parser.add_argument("--out", required=True)
    parser.add_argument("--count", type=int, default=24)
    parser.add_argument("--seed", type=int, default=7)
    args = parser.parse_args()
    if args.count < 0:
        parser.error("--count must be non-negative")
    paths = generate(Path(args.out), args.count, args.seed)
    print(f"generated={len(paths)} out={args.out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
