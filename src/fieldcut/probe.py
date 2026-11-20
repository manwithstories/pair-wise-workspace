"""Stream-probe MP4/MOV containers: tracks, time bases, cadence, damage.

The probe never buffers whole media: packets are demuxed one at a time and
frames are decoded only to (a) confirm readability and (b) locate the first
media time at which a damaged packet fails. A later keyframe that decodes
again ends the damaged range, so everything after a hole stays available.

Two timestamp views are kept per video stream:

* the container's own PTS converted to 90 kHz ticks (the decode timeline);
* the capture clock from an optional sidecar
  ``<file>.clock.json`` (``{"wrap_modulus": m, "pts_mod": [...]}``), which the
  sample synthesizer uses to inject wraps, drift and VFR without making the
  MP4 itself undecodable.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field
from fractions import Fraction

import av

from .errors import ErrorLog, Recovery, Stage
from .timebase import (
    TICKS_PER_MS,
    TICKS_PER_SECOND,
    fit_drift,
    profile_frame_rate,
    time_base_to_ticks,
    unwrap_timestamps,
)


@dataclass
class VideoProbe:
    camera: str
    path: str
    codec: str
    width: int
    height: int
    time_base: Fraction
    packet_count: int
    keyframe_count: int
    frame_decoded: int
    profile: object
    pts_ticks: list[int] = field(default_factory=list)
    keyframe_ticks: list[int] = field(default_factory=list)
    readable_start_ticks: int = 0
    readable_end_ticks: int = 0
    truncated: bool = False
    damaged_ranges: list[tuple[int, int]] = field(default_factory=list)
    wrap_indices: list[int] = field(default_factory=list)
    regression_indices: list[int] = field(default_factory=list)
    drift: object = None

    @property
    def duration_ticks(self) -> int:
        return max(0, self.readable_end_ticks - self.readable_start_ticks)


@dataclass
class AudioProbe:
    camera: str
    codec: str
    sample_rate: int
    channels: int
    packet_count: int
    samples_decoded: int
    duration_ticks: int


@dataclass
class ProbeResult:
    path: str
    size_bytes: int
    videos: list[VideoProbe] = field(default_factory=list)
    audios: list[AudioProbe] = field(default_factory=list)


def _merge_ranges(ranges: list[tuple[int, int]]) -> list[tuple[int, int]]:
    """Coalesce overlapping/adjacent (start, end) intervals; drop empty ones."""
    valid = sorted((s, e) for s, e in ranges if e > s)
    merged: list[tuple[int, int]] = []
    for start, end in valid:
        if merged and start <= merged[-1][1]:
            merged[-1] = (merged[-1][0], max(merged[-1][1], end))
        else:
            merged.append((start, end))
    return merged


def _load_clock_sidecar(path: str) -> dict | None:
    side = path + ".clock.json"
    if not os.path.isfile(side):
        return None
    try:
        with open(side, "r", encoding="utf-8") as f:
            data = json.load(f)
    except (OSError, json.JSONDecodeError):
        return None
    if isinstance(data, dict) and isinstance(data.get("pts_mod"), list):
        return data
    return None


def _probe_video(container: av.container.InputContainer, stream, camera: str,
                 path: str, sidecar: dict | None,
                 log: ErrorLog) -> VideoProbe:
    tb = Fraction(stream.time_base)
    packets: list[tuple[int, int, bool]] = []
    # Whole-container demux filtered by stream index: demuxing a single stream
    # of an interleaved file silently drops the other stream's packets.
    for pkt in container.demux():
        if pkt.stream.index != stream.index or pkt.pts is None:
            continue
        packets.append((pkt.pts, pkt.pos or 0, bool(pkt.is_keyframe)))

    pts_ticks = [time_base_to_ticks(pts, tb) for pts, _pos, _k in packets]
    key_ticks = [time_base_to_ticks(pts, tb)
                 for pts, _pos, key in packets if key]

    # Decode pass: locate damaged media times; resync at the next keyframe.
    decoded = 0
    damaged: list[tuple[int, int]] = []
    open_start: int | None = None
    last_good_ticks = pts_ticks[0] if pts_ticks else 0
    decode_container = av.open(path)
    dstream = next((s for s in decode_container.streams
                    if s.index == stream.index), None)
    try:
        for pkt in decode_container.demux():
            if pkt.stream.index != dstream.index or pkt.pts is None:
                continue
            mtime = time_base_to_ticks(pkt.pts, tb)
            try:
                frames = pkt.decode()
            except av.AVError as exc:
                log.add(Stage.PROBE, camera,
                        f"decode error at media time "
                        f"{mtime / TICKS_PER_MS:.1f} ms: {exc}",
                        media_time_ticks=mtime,
                        byte_offset=pkt.pos, recovery=Recovery.SEGMENTED)
                if open_start is None:
                    open_start = mtime
                continue
            if frames:
                decoded += len(frames)
                if open_start is not None:
                    damaged.append((open_start, mtime))
                    log.add(Stage.PROBE, camera,
                            f"resynced at {mtime / TICKS_PER_MS:.1f} ms; "
                            f"damaged range quarantined",
                            media_time_ticks=mtime,
                            recovery=Recovery.RESYNCED)
                    open_start = None
                last_good_ticks = mtime
    except av.AVError as exc:  # hard EOF mid-box (some containers do raise)
        log.add(Stage.PROBE, camera,
                f"container ends prematurely near "
                f"{last_good_ticks / TICKS_PER_MS:.1f} ms: {exc}",
                media_time_ticks=last_good_ticks,
                recovery=Recovery.SEGMENTED)
        hard_truncation = True
    else:
        hard_truncation = False
    finally:
        decode_container.close()

    # moov-first truncation often demuxes cleanly to EOF, so a missing tail is
    # detected by comparing the last decoded time against the declared
    # container duration rather than relying solely on a demux exception.
    declared_ticks = time_base_to_ticks(stream.duration or 0, tb)
    soft_truncation = (
        declared_ticks > 0
        and declared_ticks - last_good_ticks > TICKS_PER_MS * 250)
    truncated = hard_truncation or soft_truncation

    if open_start is not None:
        # A decode error that never healed: the range runs from the first bad
        # media time to the declared end (truncation) or the last good frame.
        tail_end = declared_ticks if truncated else last_good_ticks
        tail_end = max(tail_end, open_start)
        damaged.append((open_start, tail_end))
    elif truncated:
        # Decoding stayed clean but the file simply ends early.
        damaged.append((last_good_ticks, declared_ticks))
    if truncated:
        log.add(Stage.PROBE, camera,
                f"truncated tail: readable to "
                f"{last_good_ticks / TICKS_PER_MS:.1f} ms, declared "
                f"{declared_ticks / TICKS_PER_MS:.1f} ms",
                media_time_ticks=last_good_ticks,
                recovery=Recovery.SEGMENTED)

    # Merge overlapping/adjacent ranges into disjoint sorted intervals.
    damaged = _merge_ranges(damaged)

    readable_end = last_good_ticks
    prof = profile_frame_rate(pts_ticks)

    probe = VideoProbe(
        camera=camera, path=path, codec=stream.codec_context.name,
        width=stream.codec_context.width, height=stream.codec_context.height,
        time_base=tb, packet_count=len(packets),
        keyframe_count=len(key_ticks), frame_decoded=decoded, profile=prof,
        pts_ticks=pts_ticks, keyframe_ticks=key_ticks,
        readable_start_ticks=pts_ticks[0] if pts_ticks else 0,
        readable_end_ticks=readable_end, truncated=truncated,
        damaged_ranges=damaged)

    _apply_capture_clock(probe, sidecar, log)
    return probe


def _apply_capture_clock(probe: VideoProbe, sidecar: dict | None,
                         log: ErrorLog) -> None:
    """Unwrap the injected capture clock and fit its drift vs decode time."""
    if not sidecar:
        return
    modulus = int(sidecar.get("wrap_modulus", 1 << 33))
    raw = [int(v) for v in sidecar["pts_mod"]][: len(probe.pts_ticks)]
    if len(raw) < 2:
        return
    unwrap = unwrap_timestamps(raw, modulus)
    probe.wrap_indices = unwrap.wrap_indices
    probe.regression_indices = unwrap.regression_indices
    for idx in unwrap.wrap_indices:
        log.add(Stage.PROBE, probe.camera,
                f"timestamp wrap at frame {idx}; unwrapped onto monotonic axis",
                        media_time_ticks=probe.pts_ticks[idx],
                        recovery=Recovery.UNWRAPPED)
    for idx in unwrap.regression_indices:
        log.add(Stage.PROBE, probe.camera,
                f"genuine timestamp regression at frame {idx}; "
                "left in place for alignment review",
                media_time_ticks=probe.pts_ticks[idx])
    # Fit capture clock against the container decode timeline (reference).
    try:
        probe.drift = fit_drift(unwrap.values, probe.pts_ticks[: len(raw)])
    except ValueError:
        probe.drift = None
    # Cadence comes from the real capture clock (VFR lives there), not the
    # container's normalized constant-rate decode timeline.
    capture_profile = profile_frame_rate(unwrap.values)
    if capture_profile.variable:
        probe.profile = capture_profile


def _probe_audio(stream, camera: str, path: str) -> AudioProbe:
    tb = Fraction(stream.time_base)
    count = 0
    samples = 0
    container = av.open(path)
    astream = next((s for s in container.streams
                    if s.index == stream.index), None)
    try:
        for pkt in container.demux():
            if pkt.stream.index != astream.index:
                continue
            count += 1
            try:
                for fr in pkt.decode():
                    samples += fr.samples
            except av.AVError:
                break
    except av.AVError:
        pass
    finally:
        container.close()
    ctx = stream.codec_context
    duration = (round(samples / ctx.sample_rate * TICKS_PER_SECOND)
                if ctx.sample_rate else 0)
    return AudioProbe(camera, ctx.name, ctx.sample_rate,
                      ctx.channels, count, samples, duration)


def probe_file(path: str, camera: str, log: ErrorLog) -> ProbeResult:
    """Probe one MP4/MOV file; raises :class:`UsageError` if it cannot open."""
    if not os.path.isfile(path):
        from .errors import UsageError
        raise UsageError(f"input file not readable: {path}")
    result = ProbeResult(path=path, size_bytes=os.path.getsize(path))
    sidecar = _load_clock_sidecar(path)
    container = av.open(path)
    try:
        for stream in container.streams:
            if stream.type == "video":
                result.videos.append(
                    _probe_video(container, stream, camera, path,
                                 sidecar, log))
            elif stream.type == "audio":
                result.audios.append(
                    _probe_audio(stream, camera, path))
    finally:
        container.close()
    return result
