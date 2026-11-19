"""Streaming probe for relay packets, codec frames and audio blocks.

The probe never loads a whole input into memory.  It walks the relay container
packet by packet (each entry carries its source byte range) and separately
opens the rendered MP4/WAV media through PyAV and soundfile so that real codec
parameters are reported alongside relay-level observations.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field

import av
import soundfile as sf

from .container import ParsedPacket, iter_packets
from .timeline import TimeBase


@dataclass(slots=True)
class TrackInfo:
    track_id: str
    time_base: TimeBase
    codec: str = ""
    packet_count: int = 0
    last_pts: int | None = None
    last_dts: int | None = None
    pts_monotonic: bool = True
    dts_monotonic: bool = True
    first_offset: int | None = None
    last_offset: int | None = None
    keyframe_count: int = 0
    payload_bytes: int = 0


@dataclass(slots=True)
class ProbeReport:
    tracks: dict[str, TrackInfo]
    packet_total: int
    bytes_total: int
    media_codecs: dict[str, dict]
    header: dict

    def to_dict(self) -> dict:
        return {
            "packet_total": self.packet_total,
            "bytes_total": self.bytes_total,
            "tracks": {
                tid: {
                    "codec": t.codec,
                    "time_base": [t.time_base.num, t.time_base.den],
                    "packets": t.packet_count,
                    "keyframes": t.keyframe_count,
                    "payload_bytes": t.payload_bytes,
                    "byte_range": [t.first_offset, t.last_offset],
                    "pts_monotonic": t.pts_monotonic,
                    "dts_monotonic": t.dts_monotonic,
                }
                for tid, t in sorted(self.tracks.items())
            },
            "media_codecs": self.media_codecs,
        }


def probe_relay(stream_path: str) -> ProbeReport:
    """Stream the relay container once, collecting per-track statistics."""
    tracks: dict[str, TrackInfo] = {}
    header: dict = {}
    total = 0
    last_end = 0
    for header, parsed in iter_packets(stream_path):
        total += 1
        rec = parsed.record
        info = tracks.get(rec.track)
        if info is None:
            base = _base_for(header, rec.track)
            info = TrackInfo(rec.track, base,
                             codec=_codec_for(header, rec.track))
            tracks[rec.track] = info
        info.packet_count += 1
        if info.last_pts is not None and rec.pts <= info.last_pts:
            info.pts_monotonic = False
        if info.last_dts is not None and rec.dts <= info.last_dts:
            info.dts_monotonic = False
        info.last_pts = rec.pts
        info.last_dts = rec.dts
        if rec.keyframe:
            info.keyframe_count += 1
        lo, hi = parsed.packet_range
        info.first_offset = lo if info.first_offset is None else min(info.first_offset, lo)
        info.last_offset = hi if info.last_offset is None else max(info.last_offset, hi)
        info.payload_bytes += len(rec.payload)
        last_end = max(last_end, hi)
    media = probe_media_files(os.path.dirname(stream_path))
    return ProbeReport(tracks, total, last_end, media, header)


def stream_packets(stream_path: str):
    """Yield ``(header, ParsedPacket)`` pairs for downstream stages."""
    yield from iter_packets(stream_path)


def probe_media_files(base_dir: str) -> dict[str, dict]:
    """Read real codec parameters from the rendered MP4 and WAV media."""
    result: dict[str, dict] = {}
    mp4_path = os.path.join(base_dir, "media.mp4")
    if os.path.exists(mp4_path):
        result["media.mp4"] = _probe_mp4(mp4_path)
    for name in ("audio1.m4a",):
        path = os.path.join(base_dir, name)
        if os.path.exists(path):
            result[name] = _probe_mp4(path)
    wav_path = os.path.join(base_dir, "audio2.wav")
    if os.path.exists(wav_path):
        info = sf.info(wav_path)
        result["audio2.wav"] = {
            "codec": "pcm16",
            "samplerate": info.samplerate,
            "channels": info.channels,
            "frames": info.frames,
            "duration_s": round(info.duration, 6),
        }
    return result


def _probe_mp4(path: str) -> dict:
    container = av.open(path)
    streams = []
    try:
        for stream in container.streams:
            entry = {
                "type": str(stream.type),
                "codec": stream.codec_context.name,
                "time_base": [stream.time_base.numerator,
                              stream.time_base.denominator],
                "frames_estimated": int(stream.frames or 0),
            }
            if stream.type == "video":
                ctx = stream.codec_context
                entry.update({
                    "width": ctx.width,
                    "height": ctx.height,
                    "pix_fmt": ctx.pix_fmt,
                    "average_rate": [stream.average_rate.numerator,
                                     stream.average_rate.denominator],
                })
            elif stream.type == "audio":
                ctx = stream.codec_context
                entry.update({
                    "samplerate": ctx.sample_rate,
                    "channels": ctx.channels,
                    "layout": ctx.layout.name,
                })
            streams.append(entry)
    finally:
        container.close()
    return {"container": os.path.basename(path), "streams": streams}


def _base_for(header: dict, track: str) -> TimeBase:
    for entry in header.get("tracks", []):
        if entry["id"] == track:
            num, den = entry["time_base"]
            return TimeBase(num, den)
    return TimeBase(1, 90000)


def _codec_for(header: dict, track: str) -> str:
    for entry in header.get("tracks", []):
        if entry["id"] == track:
            return entry.get("codec", "")
    return ""
