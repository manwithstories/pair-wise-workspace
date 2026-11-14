"""Procedural H.264/H.265 fixtures with controlled timestamp damage."""

from __future__ import annotations

import tempfile
from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path
from typing import Iterable

import av
import numpy as np

from .compat import add_stream_like
from .decode import iter_frame_records
from .models import PacketMeta
from .probe import iter_packet_meta, probe_file

PROFILES = ("clean", "bframes", "broken")


@dataclass(frozen=True, slots=True)
class SynthResult:
    profile: str
    path: str
    codec: str
    frames: int
    bytes: int
    injected: dict[str, int]


def _frame_array(index: int, width: int, height: int) -> np.ndarray:
    yy, xx = np.mgrid[0:height, 0:width]
    frame = np.empty((height, width, 3), dtype=np.uint8)
    frame[:, :, 0] = (xx * 255 // max(width - 1, 1) + index * 7) % 256
    frame[:, :, 1] = (yy * 255 // max(height - 1, 1) + index * 11) % 256
    frame[:, :, 2] = (index * 13 + 30) % 256
    size = max(18, min(width, height) // 5)
    x0 = 8 + (index * 7) % max(width - size - 16, 1)
    y0 = 8 + (index * 11) % max(height - size - 16, 1)
    frame[y0 : y0 + size, x0 : x0 + size] = (250, 245, 15)
    frame[height // 2 - 2 : height // 2 + 2, :] = (10, 10, 10)
    return frame


def encode_clip(
    path: str,
    *,
    codec: str = "h264",
    frames: int = 150,
    width: int = 320,
    height: int = 180,
    fps: int = 25,
    bframes: int = 0,
    gop: int = 12,
) -> None:
    """Encode a deterministic synthetic clip with PyAV (no external process)."""
    if codec not in {"h264", "hevc"}:
        raise ValueError(f"unsupported synth codec: {codec}")
    encoder = "libx264" if codec == "h264" else "libx265"
    Path(path).parent.mkdir(parents=True, exist_ok=True)
    with av.open(path, "w") as container:
        stream = container.add_stream(encoder, rate=fps)
        stream.width = width
        stream.height = height
        stream.pix_fmt = "yuv420p"
        stream.options = {
            "preset": "ultrafast",
            "crf": "23",
            "bf": str(bframes),
            "g": str(gop),
            "keyint_min": str(gop),
            "sc_threshold": "0",
        }
        if codec == "hevc":
            stream.options["x265-params"] = f"bframes={bframes}:keyint={gop}:min-keyint={gop}"
        for index in range(frames):
            frame = av.VideoFrame.from_ndarray(_frame_array(index, width, height), format="rgb24")
            frame.pts = index
            frame.time_base = Fraction(1, fps)
            for packet in stream.encode(frame):
                container.mux(packet)
        for packet in stream.encode():
            container.mux(packet)


def _remux(
    source: str,
    target: str,
    mutation,
    *,
    metas: list[PacketMeta] | None = None,
) -> None:
    meta_rows = metas if metas is not None else list(iter_packet_meta(source))
    info = probe_file(source)
    with av.open(source) as input_container, av.open(target, "w") as output_container:
        output_stream = add_stream_like(output_container, input_container.streams.video[0])
        seq = 0
        for packet in input_container.demux():
            if not packet.size:
                continue
            packet.stream = output_stream
            if packet.stream_index == info.stream_index:
                packet.pts, packet.dts = mutation(seq, meta_rows[seq], packet.pts, packet.dts)
                seq += 1
            output_container.mux(packet)


def _normalize(source: str, target: str) -> None:
    metas = list(iter_packet_meta(source))
    dts_values = [item.dts for item in metas if item.dts is not None]
    shift = -min(dts_values) if dts_values else 0

    def mutation(_seq: int, _meta: PacketMeta, pts: int | None, dts: int | None):
        return (None if pts is None else pts + shift, None if dts is None else dts + shift)

    _remux(source, target, mutation, metas=metas)


def _inject_broken(source: str, target: str, *, max_jump_ms: int = 500) -> dict[str, int]:
    metas = list(iter_packet_meta(source))
    if len(metas) < 12:
        raise ValueError("broken synth needs at least 12 video packets")
    info = probe_file(source)
    duration = int(round((Fraction(1, 1) / info.average_rate) / info.time_base))
    jump_ticks = int(round((Fraction(max_jump_ms, 1000)) / info.time_base))
    rollback_seq = None
    start = max(2, len(metas) // 4)
    end = max(start + 1, len(metas) * 2 // 5)
    for meta in metas[start:end]:
        if meta.pts is not None and meta.dts is not None and meta.pts - meta.dts >= 3 * duration:
            rollback_seq = meta.seq
            break
    if rollback_seq is None:
        rollback_seq = start
    # libavformat's MP4 muxer accepts a repeated DTS most reliably while the
    # initial negative-DTS edit list is still active.
    duplicate_seq = 3 if len(metas) > 6 else 1
    jump_seq = max(rollback_seq + 2, len(metas) * 7 // 10) % len(metas)

    def mutation(seq: int, meta: PacketMeta, pts: int | None, dts: int | None):
        if seq == duplicate_seq and seq > 0:
            prior = metas[seq - 1].dts
            dts = prior
        if seq == rollback_seq and dts is not None:
            pts = dts + 2 * duration
        if seq >= jump_seq:
            if pts is not None:
                pts += jump_ticks
            if dts is not None:
                dts += jump_ticks
        return pts, dts

    _remux(source, target, mutation, metas=metas)
    return {
        "pts_rollback": 1,
        "dts_duplicate": 1,
        "negative_initial_dts": 1,
        "jump": 1,
    }


def synth_profile(
    out_path: str,
    profile: str,
    *,
    codec: str = "h264",
    frames: int = 150,
    width: int = 320,
    height: int = 180,
) -> SynthResult:
    if profile not in PROFILES:
        raise ValueError(f"unknown profile {profile!r}; choose from {', '.join(PROFILES)}")
    target = Path(out_path)
    target.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="framefix-synth-") as temporary:
        raw = str(Path(temporary) / "raw.mp4")
        normalized = str(Path(temporary) / "normalized.mp4")
        bframes = 0 if profile == "clean" else 3
        encode_clip(raw, codec=codec, frames=frames, width=width, height=height, bframes=bframes)
        _normalize(raw, normalized)
        injected = {"pts_rollback": 0, "dts_duplicate": 0, "negative_initial_dts": 0, "jump": 0}
        if profile == "broken":
            injected = _inject_broken(raw, str(target))
        else:
            Path(target).unlink(missing_ok=True)
            normalized_path = Path(normalized)
            normalized_path.replace(target)
        _validate_profile(str(target), profile)
    return SynthResult(
        profile=profile,
        path=str(target),
        codec=codec,
        frames=frames,
        bytes=target.stat().st_size,
        injected=injected,
    )


def _validate_profile(path: str, profile: str) -> None:
    records = list(iter_frame_records(path, include_pixels=False))
    if not records:
        raise RuntimeError(f"synthetic {profile} clip decoded zero frames")
    if profile != "clean" and not any(record.pict_type == "B" for record in records):
        raise RuntimeError(f"synthetic {profile} clip contains no B frames")
    if profile == "broken":
        previous = None
        rollback = False
        for record in records:
            if previous is not None and record.pts is not None and record.pts < previous:
                rollback = True
            if record.pts is not None:
                previous = record.pts
        if not rollback:
            raise RuntimeError("broken synth did not produce a decoded PTS rollback")


def synth_many(
    out_dir: str,
    profiles: Iterable[str],
    *,
    codec: str = "h264",
    frames: int = 150,
    width: int = 320,
    height: int = 180,
) -> list[SynthResult]:
    results = []
    for profile in profiles:
        results.append(
            synth_profile(
                str(Path(out_dir) / f"{profile}.mp4"),
                profile,
                codec=codec,
                frames=frames,
                width=width,
                height=height,
            )
        )
    return results

