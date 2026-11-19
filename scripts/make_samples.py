#!/usr/bin/env python3
"""Generate deterministic sample media + defective packet captures.

Everything is synthesized locally (no external files):

* synthetic H.264/H.265 Annex B and AV1 low-overhead OBU elementary streams
  whose NAL/OBU types make keyframes detectable by ``annexb.py``;
* mono WAV audio tracks with known capture offsets (soundfile/NumPy);
* packet captures with realistic, recoverable link defects: packet loss
  healed by retransmission, duplicates, bounded reordering, a truncated
  access unit and a 33-bit timestamp wrap;
* a deliberately damaged keyframe index (out-of-bounds, duplicate,
  misordered and mismatched entries) for quarantine/rebuild.

Usage: python scripts/make_samples.py --out DIR --seed 41
"""

from __future__ import annotations

import argparse
import json
import os
import sys

import numpy as np
import soundfile as sf

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))

from remuxsync.clock import TICKS_PER_SECOND, WRAP_MODULUS  # noqa: E402
from remuxsync.packets import (  # noqa: E402
    FLAG_AUDIO,
    FLAG_KEY,
    FLAG_RETX,
    FLAG_TRUNC,
    encode_packet,
)

FPS = 30
FRAME_TICKS = TICKS_PER_SECOND // FPS  # 3000
KEY_EVERY = 25
N_FRAMES = 120
PACKET_BYTES = 48


# --------------------------------------------------------------------------
# Synthetic elementary streams
# --------------------------------------------------------------------------

def _annexb_unit(nal_type: int, codec: str, payload: bytes = b"") -> bytes:
    if codec == "h264":
        header = bytes([nal_type & 0x1F])  # nal_ref_idc=0
    else:
        header = bytes([(nal_type & 0x3F) << 1])
    return b"\x00\x00\x00\x01" + header + payload


def make_annexb(codec: str, rng: np.random.Generator) -> tuple[bytes, list[int]]:
    """Return (stream, keyframe_frame_indices)."""
    parts: list[bytes] = []
    if codec == "h264":
        parts.append(_annexb_unit(7, codec, b"SPS"))   # SPS
        parts.append(_annexb_unit(8, codec, b"PPS"))   # PPS
        key_nal, p_nal = 5, 1
    else:
        parts.append(_annexb_unit(33, codec, b"VPS"))  # VPS
        parts.append(_annexb_unit(33, codec, b"SPS"))
        parts.append(_annexb_unit(34, codec, b"PPS"))
        key_nal, p_nal = 19, 1
    keyframes: list[int] = []
    for i in range(N_FRAMES):
        is_key = i % KEY_EVERY == 0
        if is_key:
            keyframes.append(i)
        nal = key_nal if is_key else p_nal
        parts.append(_annexb_unit(nal, codec, rng.bytes(PACKET_BYTES)))
    return b"".join(parts), keyframes


def _leb128(value: int) -> bytes:
    out = bytearray()
    while True:
        byte = value & 0x7F
        value >>= 7
        if value:
            out.append(byte | 0x80)
        else:
            out.append(byte)
            return bytes(out)


def _obu(obu_type: int, payload: bytes) -> bytes:
    header = bytes([(obu_type & 0x0F) << 3 | 0x02])  # has_size=1
    return header + _leb128(len(payload)) + payload


def make_av1(rng: np.random.Generator) -> tuple[bytes, list[int]]:
    parts = [_obu(1, b"\x00seq")]  # OBU_SEQUENCE_HEADER
    keyframes: list[int] = []
    for i in range(N_FRAMES):
        is_key = i % KEY_EVERY == 0
        if is_key:
            keyframes.append(i)
        # frame header first payload byte: show_existing=0; frame_type in bits 7-5
        frame_byte = 0x00 if is_key else 0x20  # KEY_FRAME vs INTER
        parts.append(_obu(6, bytes([frame_byte]) + rng.bytes(PACKET_BYTES)))
    return b"".join(parts), keyframes


# --------------------------------------------------------------------------
# Audio
# --------------------------------------------------------------------------

def _base_signal(sr: int, n: int, seed: int, freq: float) -> np.ndarray:
    rng = np.random.default_rng(seed)
    t = np.arange(n) / sr
    tone = 0.4 * np.sin(2 * np.pi * freq * t)
    clicks = np.zeros(n)
    for mark in np.arange(0.0, n / sr, 0.25):  # sync markers every 250 ms
        i = int(mark * sr)
        clicks[i:i + 8] += 0.8
    sig = tone + clicks + 0.01 * rng.standard_normal(n)
    return sig.astype(np.float32)


def make_audio_wav(path: str, sr: int, duration_s: float, seed: int,
                   freq: float, delay_s: float = 0.0) -> np.ndarray:
    """WAV with an optional capture delay (leading zero pad, not a crop)."""
    n = int(sr * duration_s)
    base = _base_signal(sr, n, seed, freq)
    pad = int(round(delay_s * sr))
    sig = np.concatenate((np.zeros(pad, dtype=np.float32), base))[:n]
    sf.write(path, sig, sr, subtype="PCM_16")
    return sig


# --------------------------------------------------------------------------
# Packet capture with defects
# --------------------------------------------------------------------------

def packetize_video(units: list[bytes], keyframes: list[int],
                    ts_list: list[int]) -> tuple[bytes, dict]:
    """One packet per access unit, with loss+retx, truncation+retx, dup, reorder."""
    key_set = set(keyframes)
    lost_frame = 7        # original delivery missing; healed by retransmission
    trunc_frame = 13      # first delivery truncated; full retransmission follows
    dup_frame = 31        # delivered twice
    reorder_frames = (40, 41)  # two arrivals swapped, well inside 120 ms
    indexed: list[tuple[int, tuple]] = []
    for i, payload in enumerate(units):
        ts = ts_list[i] & (WRAP_MODULUS - 1)
        flags = FLAG_KEY if i in key_set else 0
        if i == lost_frame:
            indexed.append((i, (i, ts, payload, flags | FLAG_RETX)))
        elif i == trunc_frame:
            half = payload[: len(payload) // 2]
            indexed.append((i, (i, ts, half, flags | FLAG_TRUNC)))
            indexed.append((i, (i, ts, payload, flags | FLAG_RETX)))
        elif i == dup_frame:
            indexed.append((i, (i, ts, payload, flags)))
            indexed.append((i, (i, ts, payload, flags | FLAG_RETX)))
        else:
            indexed.append((i, (i, ts, payload, flags)))
    a, b = reorder_frames
    indexed[a], indexed[b] = indexed[b], indexed[a]
    blob = b"".join(encode_packet(seq, ts, payload, flags)
                    for _pos, (seq, ts, payload, flags) in indexed)
    meta = {"lost_frame": lost_frame, "truncated_frame": trunc_frame,
            "dup_frame": dup_frame, "reordered_frames": list(reorder_frames)}
    return blob, meta


# --------------------------------------------------------------------------
# Capture timestamps: drift + clock step + VFR jitter + 33-bit wrap
# --------------------------------------------------------------------------

def make_timestamps(n: int, rng: np.random.Generator,
                    drift_ppm: float, wrap: bool) -> list[int]:
    base = FRAME_TICKS
    jitter = rng.integers(-90, 120, size=n)  # VFR: +/- ~1 tick noise
    step_frame = n // 2
    ts: list[int] = []
    cap = 0
    for i in range(n):
        delta = base + int(jitter[i])
        cap += delta + int(round(delta * drift_ppm / 1e6))
        if i == step_frame:
            cap += 40 * TICKS_PER_SECOND // 1000  # 40 ms capture-clock step
        ts.append(cap)
    if wrap:  # anchor near the 33-bit boundary so one wrap occurs mid-stream
        anchor = ts[n // 3]
        shift = (WRAP_MODULUS - 6 * base - anchor) % WRAP_MODULUS
        ts = [(t + shift) & (WRAP_MODULUS - 1) for t in ts]
    return ts


def packetize_audio(wav_bytes: bytes, rng: np.random.Generator,
                    chunk: int = 2048) -> bytes:
    parts: list[bytes] = []
    chunks = [wav_bytes[off:off + chunk]
              for off in range(0, len(wav_bytes), chunk)]
    trunc_seq = max(1, len(chunks) // 2)
    for seq, payload in enumerate(chunks):
        if seq == trunc_seq:
            half = payload[: len(payload) // 2]
            parts.append(encode_packet(seq, 0, half,
                                       FLAG_AUDIO | FLAG_TRUNC))
            parts.append(encode_packet(seq, 0, payload,
                                       FLAG_AUDIO | FLAG_RETX))
        else:
            parts.append(encode_packet(seq, 0, payload, FLAG_AUDIO))
    return b"".join(parts)


# --------------------------------------------------------------------------
# Damaged keyframe index
# --------------------------------------------------------------------------

def make_damaged_index(path: str, good_offsets: list[int], stream_size: int,
                       frames: list[int]) -> None:
    entries = [
        {"frame": frames[0], "offset": good_offsets[0]},          # good
        {"frame": frames[1], "offset": stream_size + 999},        # out of bounds
        {"frame": frames[0], "offset": good_offsets[0]},          # duplicate
        {"frame": frames[2] + 7, "offset": good_offsets[2] - 11}, # mismatch
        {"frame": frames[3], "offset": good_offsets[1]},          # misordered
        {"frame": frames[-1], "offset": good_offsets[-1]},        # good
    ]
    with open(path, "w", encoding="utf-8") as f:
        json.dump({"entries": entries}, f, indent=2)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--out", required=True)
    ap.add_argument("--seed", type=int, default=41)
    args = ap.parse_args()
    out = os.path.abspath(args.out)
    os.makedirs(out, exist_ok=True)
    rng = np.random.default_rng(args.seed)

    dur_s = N_FRAMES / FPS
    sr = 48_000
    manifest: dict = {"seed": args.seed, "video": [], "audio": []}

    codec_specs = [
        ("cam_studio", "h264", 120.0, True),
        ("cam_field", "h265", -80.0, False),
        ("cam_proxy", "av1", 35.0, True),
    ]
    for track, codec, ppm, do_wrap in codec_specs:
        if codec == "av1":
            stream, keyframes = make_av1(rng)
        else:
            stream, keyframes = make_annexb(codec, rng)
        es_path = os.path.join(out, f"{track}.{codec}.es")
        with open(es_path, "wb") as f:
            f.write(stream)
        # split access units so each becomes one packet payload; parameter
        # sets (SPS/PPS/VPS/sequence header) stay in the .es file only
        from remuxsync.annexb import scan
        scan_res = scan(es_path, codec)
        if codec == "av1":
            vcl_types = {6}
        elif codec == "h264":
            vcl_types = {1, 5}
        else:
            vcl_types = {1, 19, 20}
        vcl_units = [u for u in scan_res.units if u.unit_type in vcl_types]
        units = [stream[u.offset:u.offset + u.length] for u in vcl_units]
        key_unit_frames = [i for i, u in enumerate(vcl_units) if u.is_keyframe]
        assert len(units) == N_FRAMES
        ts_list = make_timestamps(len(units), rng, ppm, do_wrap)
        blob, pkt_meta = packetize_video(units, key_unit_frames, ts_list)
        pkt_path = os.path.join(out, f"{track}.{codec}.pkt")
        with open(pkt_path, "wb") as f:
            f.write(blob)
        key_offsets = [u.offset for u in vcl_units if u.is_keyframe]
        idx_path = os.path.join(out, f"{track}.{codec}.index.json")
        key_frames = key_unit_frames
        make_damaged_index(idx_path, key_offsets, len(stream), key_frames)
        manifest["video"].append({
            "track": track, "codec": codec,
            "packet_file": os.path.basename(pkt_path),
            "index_file": os.path.basename(idx_path),
            "drift_ppm": ppm, "wrapped": do_wrap,
            "keyframe_frames": key_frames, **pkt_meta,
        })

    # audio: boom (reference, offset 0) and lav (capture delay +60 ms, overrun)
    make_audio_wav(os.path.join(out, "audio_boom.wav"), sr, dur_s,
                   args.seed + 1, 440.0, delay_s=0.0)
    lav_dur = dur_s + 0.1
    make_audio_wav(os.path.join(out, "audio_lav.wav"), sr, lav_dur,
                   args.seed + 1, 440.0, delay_s=0.06)
    for name, offset_ms in (("audio_boom", 0), ("audio_lav", 60)):
        with open(os.path.join(out, f"{name}.wav"), "rb") as f:
            wav_bytes = f.read()
        with open(os.path.join(out, f"{name}.pkt"), "wb") as f:
            f.write(packetize_audio(wav_bytes, rng))
        manifest["audio"].append({
            "track": name,
            "packet_file": f"{name}.pkt",
            "expected_offset_ms": offset_ms,
            "reference": name == "audio_boom",
        })

    with open(os.path.join(out, "manifest.json"), "w", encoding="utf-8") as f:
        json.dump(manifest, f, indent=2)
    print(f"samples written to {out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
