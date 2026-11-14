"""Convert normalized sample arrays into MP4 sample-table boxes."""

from __future__ import annotations

import numpy as np

from mkvtomp4.mp4.boxes import box, full_box, u32, u64
from mkvtomp4.timing.normalize import NormalizedTrack


def _run_length(values: np.ndarray) -> list[tuple[int, int]]:
    if values.size == 0:
        return []
    runs: list[tuple[int, int]] = []
    start = 0
    for index in range(1, values.size + 1):
        if index == values.size or values[index] != values[start]:
            runs.append((int(values[start]), index - start))
            start = index
    return runs


def stts_box(track: NormalizedTrack) -> bytes:
    runs = _run_length(track.durations)
    payload = u32(len(runs)) + b"".join(u32(count) + u32(delta) for delta, count in runs)
    return full_box("stts", 0, 0, payload)


def stss_box(track: NormalizedTrack) -> bytes:
    indices = np.flatnonzero(track.keyframes) + 1
    payload = u32(indices.size) + b"".join(u32(int(index)) for index in indices)
    return full_box("stss", 0, 0, payload)


def stsc_box() -> bytes:
    return full_box("stsc", 0, 0, u32(1) + u32(1) + u32(1) + u32(1))


def stco_box(offsets: list[int]) -> bytes:
    payload = u32(len(offsets)) + b"".join(u32(offset) for offset in offsets)
    return full_box("stco", 0, 0, payload)


def stsz_box(track: NormalizedTrack) -> bytes:
    payload = u32(0) + u32(track.sample_count)
    payload += b"".join(u32(int(size)) for size in track.sizes)
    return full_box("stsz", 0, 0, payload)


def ctts_box(track: NormalizedTrack) -> bytes | None:
    deltas = track.pts.astype(np.int64) - track.dts.astype(np.int64)
    if deltas.size == 0 or np.all(deltas == 0):
        return None
    runs = _run_length(deltas)
    version = 1 if np.any(deltas < 0) else 0
    payload = u32(len(runs))
    for delta, count in runs:
        payload += u32(count) + (int(delta).to_bytes(4, "big", signed=True))
    return full_box("ctts", version, 0, payload)


def stbl_plain(track: NormalizedTrack, offsets: list[int], stsd: bytes) -> bytes:
    if len(offsets) != track.sample_count:
        raise ValueError("chunk offset count does not match sample count")
    children = [stsd, stts_box(track), stsc_box(), stsz_box(track)]
    ctts = ctts_box(track)
    if ctts is not None:
        children.append(ctts)
    children.extend([stco_box(offsets), stss_box(track)])
    return box("stbl", b"".join(children))


def stbl_empty(stsd: bytes) -> bytes:
    empty_stts = full_box("stts", 0, 0, u32(0))
    empty_stsc = full_box("stsc", 0, 0, u32(0))
    empty_stsz = full_box("stsz", 0, 0, u32(0) + u32(0))
    empty_stco = full_box("stco", 0, 0, u32(0))
    return box("stbl", stsd + empty_stts + empty_stsc + empty_stsz + empty_stco)


def mdat_payload(frames: list[bytes]) -> bytes:
    return b"".join(frames)


def tfdt_box(base_time: int) -> bytes:
    return full_box("tfdt", 1, 0, u64(base_time))
