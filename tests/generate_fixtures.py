"""Deterministic fixture generation for relaymedia.

All sample material is synthesised locally from a seed — no network, no
external files. The generator writes audio (varied sample rates, channel
layouts and one damaged AAC source with a known corruption offset), SRT and
VTT subtitles (good, overlap, bad-timecode, truncated), a timecode manifest
exercising fractional rates / negative offsets / day wrap / repeated stamps,
and MP4 colour fixtures (consistent, conflicting, missing metadata). A
``truth.json`` records every expected damage position and line so the
verifier can prove localisation accuracy.
"""

from __future__ import annotations

import json
from fractions import Fraction
from pathlib import Path

import av
import numpy as np

# Fixed editorial frame rate for the generated timecode manifest.
FRAME_RATE = "30000/1001"


def _rng(seed: int) -> np.random.Generator:
    return np.random.default_rng(seed)


def _write_pcm_wav(path: Path, pcm: np.ndarray, sample_rate: int, layout: str) -> None:
    """pcm is planar int16: shape (channels, samples)."""
    container = av.open(str(path), "w")
    stream = container.add_stream("pcm_s16le", rate=sample_rate)
    stream.layout = layout
    frame_size = 1024
    for start in range(0, pcm.shape[1], frame_size):
        frame = av.AudioFrame.from_ndarray(
            pcm[:, start:start + frame_size], format="s16p", layout=layout
        )
        frame.sample_rate = sample_rate
        for packet in stream.encode(frame):
            container.mux(packet)
    for packet in stream.encode():
        container.mux(packet)
    container.close()


def _tone(rng: np.random.Generator, samples: int, freq: float) -> np.ndarray:
    t = np.arange(samples) / 48000.0
    warble = 0.05 * np.sin(2 * np.pi * 3.0 * t)
    return (0.5 * np.sin(2 * np.pi * freq * t + warble)).astype(np.float32)


def _silence_gaps(pcm: np.ndarray, rng: np.random.Generator) -> np.ndarray:
    # Two deliberate silent gaps and one 24 ms pulse that must not merge them.
    pcm[:, 9600:14400] = 0.0
    pcm[:, 24000:28800] = 0.0
    pulse = _tone(rng, 1152, 880)
    pcm[:, 33600:34752] = pulse[None, :]
    return pcm


def build_audio(out: Path, seed: int, truth: dict) -> None:
    rng = _rng(seed)
    audio = out / "audio"
    audio.mkdir(parents=True, exist_ok=True)
    samples = 48000 * 3
    mono = _silence_gaps(_tone(rng, samples, 440)[None, :].copy(), rng)
    # Primary node: 48 kHz stereo with explicit silence gaps and a short pulse.
    stereo = np.stack([mono[0], np.roll(mono[0], 7)], axis=0)
    _write_pcm_wav(audio / "node-a.wav", (stereo * 32767).astype(np.int16), 48000, "stereo")
    # Second node: 44.1 kHz mono, same underlying content (cross-rate duplicate).
    rate_b = 44100
    resampled = np.interp(
        np.linspace(0.0, 3.0, rate_b * 3, endpoint=False),
        np.linspace(0.0, 3.0, samples, endpoint=False), mono[0],
    ).astype(np.float32)
    _write_pcm_wav(audio / "node-b.wav", (resampled * 32767).astype(np.int16)[None, :],
                   rate_b, "mono")
    # Third node: six-channel source to exercise explicit channel mapping.
    rng6 = _rng(seed + 1)
    six = np.repeat(_tone(rng6, samples, 330)[None, :], 6, axis=0)
    _write_pcm_wav(audio / "node-c.wav", (six * 32767).astype(np.int16), 48000, "5.1")
    _build_damaged_aac(audio / "node-damaged.aac", samples, truth)


def _build_damaged_aac(path: Path, samples: int, truth: dict) -> None:
    rng = _rng(7)
    pcm = (_tone(rng, samples, 523) * 32767).astype(np.int16)[None, :]
    container = av.open(str(path), "w")
    stream = container.add_stream("aac", rate=48000)
    stream.layout = "mono"
    for start in range(0, pcm.shape[1], 1024):
        frame = av.AudioFrame.from_ndarray(pcm[:, start:start + 1024],
                                           format="s16p", layout="mono")
        frame.sample_rate = 48000
        for packet in stream.encode(frame):
            container.mux(packet)
    for packet in stream.encode():
        container.mux(packet)
    container.close()
    raw = bytearray(path.read_bytes())
    # Corrupt a fixed fraction with a zero marker. Clean ADTS AAC carries no
    # eight-byte zero run, so scan_corruption locates this exactly; the true
    # offset is recorded and must match within the verification tolerance.
    damage_offset = int(len(raw) * 0.4)
    raw[damage_offset:damage_offset + 12] = b"\x00" * 12
    path.write_bytes(raw)
    truth["media_damage"] = {
        "file": str(path.name),
        "true_offset_bytes": damage_offset,
        "tolerance_bytes": 32,
    }


def build_subtitles(out: Path, truth: dict) -> None:
    subs = out / "subtitles"
    subs.mkdir(parents=True, exist_ok=True)
    good_srt = (
        "1\n00:00:00,500 --> 00:00:01,200\nopening cue\n\n"
        "2\n00:00:01,500 --> 00:00:02,000\nsecond cue\n\n"
        "3\n00:00:02,250 --> 00:00:02,750\nthird cue\n"
    )
    (subs / "good.srt").write_text(good_srt)
    (subs / "good.vtt").write_text("WEBVTT\n\n" + good_srt.replace(",", "."))
    # Overlap: cue 2 starts before cue 1 ends; both kept, overlap reported.
    (subs / "overlap.srt").write_text(
        "1\n00:00:00,500 --> 00:00:01,500\nfirst\n\n"
        "2\n00:00:01,000 --> 00:00:02,000\noverlapping\n"
    )
    bad = (
        "1\n00:00:00,500 --> 00:00:01,000\nok before damage\n\n"
        "2\n99:99:99,000 --> 00:00:01,000\nillegal time range\n\n"
        "3\n00:00:02,000 --> 00:00:02,500\nok after damage\n\n"
        "4\n00:00:03,000 --> 00:00:03,500\ntruncated no blank line"
    )
    bad_path = subs / "damaged.srt"
    bad_path.write_text(bad)
    # The parser locates the whole offending record at its cue-block start.
    bad_offset = bad.index("2\n99:99:99")
    bad_line = bad[:bad_offset].count("\n") + 1  # line of the cue index
    truth["subtitle_damage"] = {
        "file": str(bad_path.name),
        "true_offset_bytes": bad_offset,
        "true_line": bad_line,
        "tolerance_bytes": 32,
        "tolerance_lines": 1,
    }


def build_timecodes(out: Path, truth: dict) -> None:
    """One record per line: negative offset, day wrap, repeated, unconvertible."""
    tc = out / "timecodes.txt"
    lines = [
        "# rate=30000/1001 offset=-0.5",
        "23:59:59:29",  # last frame before midnight
        "00:00:00:00",  # day wrap -> +1 day
        "00:00:00:00",  # repeated stamp, legal
        "00:00:00:15",
        "12:60:00:00",  # illegal minute -> error list, boundary must not move
        "00:00:01:00",
    ]
    tc.write_text("\n".join(lines) + "\n")
    bad_index = lines.index("12:60:00:00")
    bad_offset = sum(len(line) + 1 for line in lines[:bad_index])
    truth["timecode_damage"] = {
        "file": tc.name,
        "true_offset_bytes": bad_offset,
        "true_line": bad_index + 1,
        "tolerance_bytes": 32,
        "tolerance_lines": 1,
    }


def _write_mp4(path: Path, primaries: int | None) -> None:
    container = av.open(str(path), "w", format="mp4")
    stream = container.add_stream("libx264", rate=30)
    stream.width = 96
    stream.height = 72
    stream.pix_fmt = "yuv420p"
    cc = stream.codec_context
    if primaries is not None:
        cc.color_primaries = primaries
        cc.color_trc = primaries
        cc.colorspace = primaries
        cc.color_range = 1
    for _ in range(4):
        frame = av.VideoFrame.from_ndarray(
            np.zeros((72, 96, 3), dtype=np.uint8), format="rgb24"
        )
        for packet in stream.encode(frame):
            container.mux(packet)
    for packet in stream.encode():
        container.mux(packet)
    container.close()


def build_video(out: Path, truth: dict) -> None:
    video = out / "video"
    video.mkdir(parents=True, exist_ok=True)
    _write_mp4(video / "color-bt709.mp4", primaries=1)
    _write_mp4(video / "color-missing.mp4", primaries=None)
    conflict = video / "color-conflict.mp4"
    _write_mp4(conflict, primaries=1)
    raw = bytearray(conflict.read_bytes())
    idx = raw.find(b"colr")
    prim_field = idx + 4 + 4  # skip 'colr' then 'nclx'
    raw[prim_field:prim_field + 2] = (9).to_bytes(2, "big")  # BT.2020 in box only
    conflict.write_bytes(raw)
    truth["color_conflict"] = {"file": conflict.name, "box_primaries": 9,
                               "context_primaries": 1}


def generate_fixtures(out_dir: str | Path, seed: int = 42) -> dict:
    out = Path(out_dir)
    out.mkdir(parents=True, exist_ok=True)
    truth: dict = {"seed": seed, "frame_rate": FRAME_RATE,
                   "expected_silence_gaps": 2, "expected_short_pulse_ms": 24}
    build_audio(out, seed, truth)
    build_subtitles(out, truth)
    build_timecodes(out, truth)
    build_video(out, truth)
    (out / "truth.json").write_text(json.dumps(truth, indent=2, sort_keys=True))
    return truth


if __name__ == "__main__":  # pragma: no cover
    import argparse

    parser = argparse.ArgumentParser(description="Generate relaymedia fixtures")
    parser.add_argument("--out", required=True)
    parser.add_argument("--seed", type=int, default=42)
    args = parser.parse_args()
    result = generate_fixtures(args.out, args.seed)
    print(json.dumps({"generated": args.out, "truth_keys": sorted(result)}, indent=2))
