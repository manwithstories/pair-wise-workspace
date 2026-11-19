"""Self-generated sample fixtures: no external files are referenced.

Builds deterministic frame-record files covering the normal case plus the
field failure modes: truncated tail, checksum mismatch, unknown record
version, out-of-bounds fields, timestamp wraparound at the device-clock
modulus, sequence reset (card swap) and a companion WAV track.
"""
from __future__ import annotations

import json
import struct
import zlib
from pathlib import Path

import numpy as np

from .model import PIX_FMT_IDS, TIMESTAMP_WRAP_NS, frame_nbytes

_V1 = struct.Struct("<4sBBHIQBBHHII")
_V2 = struct.Struct("<4sBBHIQBBHHIIqI")

WIDTH, HEIGHT = 64, 48
STEP_NS = 40_000_000  # 25 fps


def _payload(pixfmt: str, seed: int) -> bytes:
    rng = np.random.default_rng(seed)
    return rng.integers(0, 256, frame_nbytes(pixfmt, WIDTH, HEIGHT), dtype=np.uint8).tobytes()


def _meta(device_id: int, seq: int) -> bytes:
    return json.dumps({"device": device_id, "seq": seq, "note": "fixture"}).encode()


def record_v1(device: int, seq: int, ts: int, pixfmt: str, payload: bytes,
              keyframe: bool, meta: bytes | None = None) -> bytes:
    meta = _meta(device, seq) if meta is None else meta
    flags = 1 if keyframe else 0
    crc = zlib.crc32(meta + payload) & 0xFFFFFFFF
    return _V1.pack(b"FPR1", 1, flags, device, seq, ts, PIX_FMT_IDS[pixfmt], 0,
                    WIDTH, HEIGHT, len(meta), len(payload), crc) + meta + payload


def record_v2(device: int, seq: int, ts: int, pixfmt: str, payload: bytes,
              keyframe: bool, exposure_ns: int = 20_000_000, iso: int = 800) -> bytes:
    meta = _meta(device, seq)
    flags = 1 if keyframe else 0
    crc = zlib.crc32(meta + payload) & 0xFFFFFFFF
    return _V2.pack(b"FPR2", 2, flags, device, seq, ts, PIX_FMT_IDS[pixfmt], 0,
                    WIDTH, HEIGHT, len(meta), len(payload), crc,
                    exposure_ns, iso) + meta + payload


def write_good(path: Path) -> None:
    """Two interleaved devices; device 2 crosses the clock wraparound and
    performs a card swap (sequence reset) mid-stream."""
    parts: list[bytes] = []
    seed = 1000
    # device 1: 24 frames, v1, rgb24, keyframe every 6
    d1 = [record_v1(1, s, 5_000_000_000 + s * STEP_NS, "rgb24",
                    _payload("rgb24", seed + s), s % 6 == 0) for s in range(24)]
    # device 2: 20 frames, v2, gray, starts 3 steps before the wrap modulus
    d2: list[bytes] = []
    base = TIMESTAMP_WRAP_NS - 3 * STEP_NS
    for s in range(20):
        seq = s if s < 12 else s - 12  # card swap resets the sequence at 12
        ts = (base + s * STEP_NS) % TIMESTAMP_WRAP_NS
        d2.append(record_v2(2, seq, ts, "gray", _payload("gray", seed + 100 + s),
                            s % 5 == 0))
    # interleave in acquisition order (two d1, one d2)
    i = j = 0
    while i < len(d1) or j < len(d2):
        for _ in range(2):
            if i < len(d1):
                parts.append(d1[i]); i += 1
        if j < len(d2):
            parts.append(d2[j]); j += 1
    path.write_bytes(b"".join(parts))


def write_corrupt(path: Path) -> None:
    """Valid device-1 records interleaved with four corrupt spots and a
    truncated tail; every corrupt spot is followed by recoverable records."""
    parts: list[bytes] = []
    seed = 5000

    def good(seq: int) -> bytes:
        return record_v1(1, seq, 1_000_000_000 + seq * STEP_NS, "rgb24",
                         _payload("rgb24", seed + seq), seq % 4 == 0)

    parts += [good(0), good(1)]
    # 1) unknown version record
    junk_meta = b"\x00" * 8
    junk_payload = b"\x00" * frame_nbytes("rgb24", WIDTH, HEIGHT)
    parts.append(_V1.pack(b"FPR9", 9, 0, 1, 99, 0, 1, 0, WIDTH, HEIGHT,
                          len(junk_meta), len(junk_payload), 0) + junk_meta + junk_payload)
    parts += [good(2), good(3)]
    # 2) checksum mismatch
    bad = bytearray(good(4))
    bad[32] ^= 0xFF  # crc32 field lives at header bytes 32..35
    parts.append(bytes(bad))
    parts += [good(5), good(6)]
    # 3) out-of-bounds field (payload length beyond the single-frame limit)
    parts.append(_V1.pack(b"FPR1", 1, 0, 1, 7, 7 * STEP_NS, 1, 0, WIDTH, HEIGHT,
                          0, 128 * 1024 * 1024, 0))
    parts += [good(8), good(9)]
    # 4) garbage between records (bad magic)
    parts.append(b"\xde\xad\xbe\xef" * 6)
    parts += [good(10), good(11)]
    # 5) truncated tail: half a header
    parts.append(good(12)[:17])
    path.write_bytes(b"".join(parts))


def write_audio(path: Path, seconds: float = 0.5, rate: int = 48_000) -> None:
    import soundfile as sf

    t = np.arange(int(seconds * rate), dtype=np.float32) / rate
    tone = np.stack([np.sin(2 * np.pi * 440 * t), np.sin(2 * np.pi * 554 * t)], axis=1)
    sf.write(str(path), tone, rate, subtype="PCM_16")


def generate_all(directory: str | Path) -> dict[str, Path]:
    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=True)
    paths = {
        "good": directory / "good.fpk",
        "corrupt": directory / "corrupt.fpk",
        "audio": directory / "companion.wav",
    }
    write_good(paths["good"])
    write_corrupt(paths["corrupt"])
    write_audio(paths["audio"])
    return paths
