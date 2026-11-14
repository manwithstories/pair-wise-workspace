"""Deterministic synthetic test footage containing timeline faults."""

from __future__ import annotations

import random
from fractions import Fraction
from pathlib import Path
from typing import Any

import av
import numpy as np

from frameaudit.errors import ProcessingError


MICROSECOND_TIME_BASE = Fraction(1, 1_000_000)
FRAME_RATE = 24


def _render_frame(index: int, width: int, height: int) -> np.ndarray:
    x_grid = np.arange(width, dtype=np.uint16)[None, :]
    y_grid = np.arange(height, dtype=np.uint16)[:, None]
    pixels = np.empty((height, width, 3), dtype=np.uint8)
    pixels[..., 0] = ((x_grid * 3 + index * 7) % 251).astype(np.uint8)
    pixels[..., 1] = ((y_grid * 5 + index * 11) % 241).astype(np.uint8)
    pixels[..., 2] = ((x_grid + y_grid + index * 13) % 239).astype(np.uint8)
    top = index % max(1, height - 12)
    left = (index * 3) % max(1, width - 24)
    pixels[top : top + 12, left : left + 24] = 235
    return pixels


def _append_encoded(
    stream: av.VideoStream,
    frame: av.VideoFrame,
    container: av.OutputContainer,
    mapping: dict[int, int],
    clock_us: int,
) -> None:
    for packet in stream.encode(frame):
        if packet.pts is not None:
            seconds = packet.pts * (packet.time_base or MICROSECOND_TIME_BASE)
            mapping[packet.pts] = round(float(seconds) * 1_000_000 / clock_us)
        container.mux(packet)


def _encode_base(
    path: Path,
    frames: int,
    width: int,
    height: int,
    seed: int,
) -> dict[int, int]:
    del seed  # Pixel patterns are index-driven; seed only selects fault positions.
    clock_us = round(1_000_000 / FRAME_RATE)
    container = av.open(str(path), mode="w", format="mp4")
    mapping: dict[int, int] = {}
    try:
        stream = container.add_stream("libx264", rate=FRAME_RATE)
        stream.width = width
        stream.height = height
        stream.pix_fmt = "yuv420p"
        stream.codec_context.time_base = MICROSECOND_TIME_BASE
        stream.options = {
            "preset": "medium",
            "crf": "23",
            "g": "32",
            "bf": "3",
            "x264-params": (
                "b-adapt=2:b-pyramid=normal:keyint=32:min-keyint=32:"
                "scenecut=0:open-gop=0"
            ),
        }
        for index in range(frames):
            pixels = _render_frame(index, width, height)
            frame = av.VideoFrame.from_ndarray(pixels, format="rgb24").reformat(
                format="yuv420p"
            )
            frame.pts = round(index * 1_000_000 / FRAME_RATE)
            frame.time_base = MICROSECOND_TIME_BASE
            _append_encoded(stream, frame, container, mapping, clock_us)
        for packet in stream.encode(None):
            if packet.pts is not None:
                seconds = packet.pts * (packet.time_base or MICROSECOND_TIME_BASE)
                mapping[packet.pts] = round(float(seconds) * 1_000_000 / clock_us)
            container.mux(packet)
    except Exception as exc:
        raise ProcessingError(f"cannot encode fixture base: {exc}") from exc
    finally:
        container.close()
    return mapping


def _fault_steps(frames: int, clock_us: int, seed: int) -> dict[int, tuple[str, int]]:
    rng = random.Random(seed)
    target = {
        "duplicate_pts": 12,
        "pts_rollback": 10,
        "gap": 8,
        "segment_jump": 4,
    }
    candidates = list(range(48, frames - 3))
    rng.shuffle(candidates)
    positions: list[int] = []
    for position in candidates:
        if all(abs(position - existing) > 2 for existing in positions):
            positions.append(position)
            if len(positions) == sum(target.values()):
                break
    positions.sort()
    kinds: list[str] = []
    for kind, count in target.items():
        kinds.extend([kind] * count)
    rng.shuffle(kinds)

    steps: dict[int, tuple[str, int]] = {}
    for position, kind in zip(positions, kinds):
        if kind == "duplicate_pts":
            steps[position] = (kind, 0)
        elif kind == "pts_rollback":
            steps[position] = (kind, -clock_us)
        elif kind == "gap":
            steps[position] = (kind, rng.randint(180_000, 320_000))
        else:
            steps[position] = (kind, 1_800_000)
    return steps


def _raw_timeline(
    frames: int,
    clock_us: int,
    fault_steps: dict[int, tuple[str, int]],
) -> list[int]:
    timestamps = [0] * frames
    for index in range(1, frames):
        step = fault_steps.get(index, ("normal", clock_us))[1]
        timestamps[index] = timestamps[index - 1] + step
    return timestamps


def _write_faulted_mp4(
    base_path: Path,
    output_path: Path,
    raw_pts_us: list[int],
    mapping: dict[int, int],
    clock_us: int,
) -> None:
    input_container = av.open(str(base_path))
    output_container = av.open(
        str(output_path),
        mode="w",
        format="mp4",
        options={"movflags": "+faststart"},
    )
    try:
        input_stream = input_container.streams.video[0]
        output_stream = output_container.add_stream(template=input_stream)
        time_base = input_stream.time_base or MICROSECOND_TIME_BASE
        count = len(raw_pts_us)
        for packet_index, packet in enumerate(input_container.demux(input_stream)):
            if packet.size == 0:
                continue
            display_index = mapping.get(packet.pts)
            if display_index is None:
                raise ProcessingError("cannot map encoded packet to source frame")
            pts_us = raw_pts_us[display_index]
            packet.pts = round(pts_us * time_base.denominator / (time_base.numerator * 1_000_000))
            dts_us = (packet_index - count) * clock_us
            packet.dts = round(dts_us * time_base.denominator / (time_base.numerator * 1_000_000))
            packet.duration = round(clock_us * time_base.denominator / (time_base.numerator * 1_000_000))
            output_container.mux(packet)
    except Exception as exc:
        raise ProcessingError(f"cannot inject timeline faults: {exc}") from exc
    finally:
        output_container.close()
        input_container.close()


def generate_fixtures(
    out_dir: str | Path,
    seed: int = 7,
    frames: int = 1440,
    width: int = 160,
    height: int = 90,
) -> dict[str, Any]:
    output = Path(out_dir)
    output.mkdir(parents=True, exist_ok=True)
    clock_us = round(1_000_000 / FRAME_RATE)
    base_path = output / ".vfr_bpyramid_base.mp4"
    sample_path = output / "vfr_bpyramid.mp4"
    truncated_path = output / "truncated_packet.mp4"

    try:
        mapping = _encode_base(base_path, frames, width, height, seed)
        fault_steps = _fault_steps(frames, clock_us, seed)
        raw_pts = _raw_timeline(frames, clock_us, fault_steps)
        _write_faulted_mp4(base_path, sample_path, raw_pts, mapping, clock_us)

        size = sample_path.stat().st_size
        with sample_path.open("rb") as source, truncated_path.open("wb") as target:
            target.write(source.read(max(1, int(size * 0.99))))
    finally:
        base_path.unlink(missing_ok=True)

    injected = {kind: 0 for kind in ("duplicate_pts", "pts_rollback", "gap", "segment_jump")}
    for kind, _ in fault_steps.values():
        injected[kind] += 1
    return {
        "seed": seed,
        "frames": frames,
        "clock_us": clock_us,
        "files": {
            "vfr_bpyramid": str(sample_path),
            "truncated_packet": str(truncated_path),
        },
        "injected": injected,
        "raw_first_us": raw_pts[0],
        "raw_last_us": raw_pts[-1],
    }

