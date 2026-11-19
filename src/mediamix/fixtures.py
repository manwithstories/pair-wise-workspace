"""Synthetic fixture generation for demos and tests.

Every input file is produced by code — no external media is referenced. The
module emits five self-describing track files:

* ``studio.mmx``   — 8-bit sequence counter that wraps, small +42 ppm drift
* ``location.mmx`` — 16-bit counters, negative drift (-75 ppm) and jitter
* ``proxy.mmx``    — small frames, 8-bit *clock* counter that wraps
* ``truncated.mmx``— header truncated mid-stream (known ground-truth offset)
* ``badhash.mmx``  — payload corrupted after the SHA-256 digest was written

Ground-truth corruption offsets are returned alongside the files so callers
can prove the parser's reported byte offsets are exact.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

from .model import (
    FRAME_HEADER_SIZE,
    TrackHeader,
    safe_resolve,
)
from .probe import make_frame_header

WIDTH = 24
HEIGHT = 12
RGB_PAYLOAD = WIDTH * HEIGHT * 3
CLOCK_RATE = 1_000_000  # acquisition clock: 1 MHz -> microsecond ticks


@dataclass
class FixtureSpec:
    name: str
    track_id: str
    frames: int
    seq_bits: int
    clock_bits: int
    drift_ppm: float
    jitter_ticks: int
    start_gap_ns: int = 0


@dataclass
class CorruptionGroundTruth:
    name: str
    kind: str                       # truncated_header | checksum
    expected_offset: int
    expected_frame_seq: int


def _payload(seed: int, width: int = WIDTH, height: int = HEIGHT) -> bytes:
    """Deterministic RGB payload: gradients seeded per frame."""
    import numpy as np

    rows = np.arange(height, dtype=np.uint16)[:, None]
    cols = np.arange(width, dtype=np.uint16)[None, :]
    r = (rows * 7 + seed * 3) & 0xFF
    g = (cols * 11 + seed * 5) & 0xFF
    b = (rows + cols + seed * 13) & 0xFF
    rgb = np.zeros((height, width, 3), dtype=np.uint8)
    rgb[..., 0] = r.astype(np.uint8)
    rgb[..., 1] = g.astype(np.uint8)
    rgb[..., 2] = b.astype(np.uint8)
    return rgb.tobytes()


def _frame_file_offset(frame_index: int) -> int:
    """Byte offset of frame ``frame_index``'s header in a standard track file."""
    from .model import FILE_HEADER_SIZE

    return FILE_HEADER_SIZE + frame_index * (FRAME_HEADER_SIZE + RGB_PAYLOAD)


def build_clean_track(
    path: Path,
    spec: FixtureSpec,
    *,
    fps_num: int = 30,
    fps_den: int = 1,
) -> None:
    """Generate one well-formed track with wrap + drift baked into counters."""
    seq_mod = 1 << spec.seq_bits
    clock_mod = 1 << spec.clock_bits
    # Microsecond spacing for the nominal frame rate, then drift + jitter.
    base_step_us = 1_000_000 * fps_den / fps_num
    clock = spec.start_gap_ns // 1000
    frames = []
    raw_clock = clock
    track_seed = hash(spec.track_id) % 97
    # Start the sequence near the counter boundary so an 8-bit track wraps
    # visibly inside the captured window.
    seq_origin = (1 << spec.seq_bits) - 12 if spec.seq_bits <= 8 else 0
    for i in range(spec.frames):
        seq = (seq_origin + i) % seq_mod
        step = base_step_us * (1 + spec.drift_ppm / 1_000_000)
        if i:
            jitter = ((i * 37) % (2 * spec.jitter_ticks + 1)) - spec.jitter_ticks
            raw_clock = clock + int(round(step)) + jitter
            clock = raw_clock
        payload = _payload(seed=i + track_seed)
        hdr = make_frame_header(
            seq=seq,
            clock=raw_clock % clock_mod,
            payload=payload,
            fps_num=fps_num, fps_den=fps_den,
            width=WIDTH, height=HEIGHT,
        )
        frames.append((hdr, payload))
    header = TrackHeader(
        track_id=spec.track_id, clock_rate=CLOCK_RATE,
        clock_bits=spec.clock_bits, seq_bits=spec.seq_bits,
    )
    from .probe import write_track_file

    write_track_file(path, header, frames)


def build_truncated_track(path: Path) -> CorruptionGroundTruth:
    """Write a valid track then cut the 5th frame's header in half."""
    spec = FixtureSpec(
        name="truncated.mmx", track_id="broken-trunc", frames=6,
        seq_bits=16, clock_bits=32, drift_ppm=0.0, jitter_ticks=0,
    )
    build_clean_track(path, spec)
    cut_frame = 4
    offset = _frame_file_offset(cut_frame)
    blob = path.read_bytes()
    path.write_bytes(blob[: offset + FRAME_HEADER_SIZE // 2])
    return CorruptionGroundTruth(
        name=path.name, kind="truncated_header",
        expected_offset=offset, expected_frame_seq=cut_frame,
    )


def build_bad_digest_track(path: Path) -> CorruptionGroundTruth:
    """Write a valid track then flip payload bytes without updating the digest."""
    spec = FixtureSpec(
        name="badhash.mmx", track_id="broken-hash", frames=6,
        seq_bits=16, clock_bits=32, drift_ppm=0.0, jitter_ticks=0,
    )
    build_clean_track(path, spec)
    corrupt_frame = 3
    header_off = _frame_file_offset(corrupt_frame)
    payload_off = header_off + FRAME_HEADER_SIZE
    blob = bytearray(path.read_bytes())
    blob[payload_off] ^= 0xFF  # flip one payload byte; digest now mismatches
    path.write_bytes(bytes(blob))
    return CorruptionGroundTruth(
        name=path.name, kind="checksum",
        expected_offset=payload_off, expected_frame_seq=corrupt_frame,
    )


def generate_fixtures(workdir: str | Path) -> list[CorruptionGroundTruth]:
    """Create the full fixture set under ``workdir/inputs``."""
    inputs = safe_resolve(workdir, "inputs")
    inputs.mkdir(parents=True, exist_ok=True)
    clean_specs = [
        FixtureSpec("studio.mmx", "studio-a", 160, seq_bits=8, clock_bits=32,
                    drift_ppm=42.0, jitter_ticks=2),
        FixtureSpec("location.mmx", "location-b", 160, seq_bits=16, clock_bits=32,
                    drift_ppm=-75.0, jitter_ticks=3, start_gap_ns=20_000),
        FixtureSpec("proxy.mmx", "proxy-c", 160, seq_bits=8, clock_bits=32,
                    drift_ppm=15.0, jitter_ticks=1, start_gap_ns=40_000),
    ]
    for spec in clean_specs:
        build_clean_track(inputs / spec.name, spec)
    truths = [
        build_truncated_track(inputs / "truncated.mmx"),
        build_bad_digest_track(inputs / "badhash.mmx"),
    ]
    return truths


def fixture_input_names() -> list[str]:
    return ["studio.mmx", "location.mmx", "proxy.mmx", "truncated.mmx", "badhash.mmx"]
