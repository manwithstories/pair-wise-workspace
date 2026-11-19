"""Container probing via PyAV: stream discovery and packet-level timestamps.

Handles variable frame rate by always using actual packet pts/dts with the
stream's rational time_base. Bad packets and missing audio tracks are
recoverable errors in the log; an unopenable container is fatal.
"""

from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path

import av

from .errors import ErrorLog, MediaBatchError

_AV_ERRORS = (av.error.FFmpegError, ValueError, OSError)


@dataclass
class StreamInfo:
    path: str
    has_audio: bool
    has_video: bool
    sample_rate: int
    channels: int
    video_fps: Fraction | None
    vfr: bool
    duration: Fraction | None  # seconds


@dataclass
class PacketInfo:
    stream_type: str      # "audio" | "video"
    pts: int | None       # in stream time_base units
    dts: int | None
    time_base: Fraction
    duration: int
    size: int
    pos: int = -1         # byte offset of the packet in the source file
    corrupt: bool = False

    @property
    def seconds(self) -> Fraction | None:
        return None if self.pts is None else self.pts * self.time_base


def _open(path: str | Path):
    src = str(path)
    try:
        return av.open(src)
    except av.error.FileNotFoundError as exc:
        raise MediaBatchError("io_error", f"cannot open {src}: {exc}", src, 0)
    except _AV_ERRORS as exc:
        raise MediaBatchError("bad_container", f"cannot open {src}: {exc}", src, 0)


def stream_info(path: str | Path, log: ErrorLog | None = None) -> StreamInfo:
    container = _open(path)
    src = str(path)
    with container:
        streams = list(container.streams)
        audio = next((s for s in streams if s.type == "audio"), None)
        video = next((s for s in streams if s.type == "video"), None)
        if audio is None and log is not None:
            log.add("missing_audio_track", f"{src} has no audio stream", src, 0, -1)
        fps = None
        if video is not None and video.average_rate:
            fps = Fraction(video.average_rate.numerator, video.average_rate.denominator)
        duration = None
        if container.duration is not None:
            duration = Fraction(container.duration, av.time_base)
        vfr = False
        if video is not None and video.average_rate:
            vfr = bool(getattr(video, "is_vfr", False))
        return StreamInfo(
            path=src,
            has_audio=audio is not None,
            has_video=video is not None,
            sample_rate=audio.sample_rate if audio is not None else 0,
            channels=audio.channels if audio is not None else 0,
            video_fps=fps,
            vfr=vfr,
            duration=duration,
        )


def probe_packets(path: str | Path, log: ErrorLog,
                  max_packets: int | None = None) -> list[PacketInfo]:
    """Demux all packets with their rational timestamps.

    Corrupt packets are logged with their byte position and skipped.
    """
    src = str(path)
    container = _open(path)
    packets: list[PacketInfo] = []
    with container:
        bases = {s.index: Fraction(s.time_base.numerator, s.time_base.denominator)
                 for s in container.streams}
        types = {s.index: s.type for s in container.streams}
        try:
            for pkt in container.demux():
                if pkt.stream_index not in bases:
                    continue
                info = PacketInfo(
                    stream_type=types[pkt.stream_index],
                    pts=pkt.pts, dts=pkt.dts,
                    time_base=bases[pkt.stream_index],
                    duration=pkt.duration or 0,
                    size=pkt.size,
                    pos=pkt.pos if pkt.pos is not None else -1,
                    corrupt=bool(pkt.is_corrupt),
                )
                if info.corrupt:
                    log.add("bad_packet",
                            f"corrupt {info.stream_type} packet in {src}",
                            src, max(info.pos, 0), len(packets))
                packets.append(info)
                if max_packets is not None and len(packets) >= max_packets:
                    break
        except _AV_ERRORS as exc:
            pos = packets[-1].pos if packets else 0
            log.add("bad_packet", f"demux failed in {src}: {exc}",
                    src, max(pos, 0), len(packets))
    return packets


def video_frame_times(path: str | Path, log: ErrorLog) -> list[Fraction]:
    """Presentation-order video packet timestamps (seconds, exact rationals)."""
    pkts = probe_packets(path, log)
    times = [p.seconds for p in pkts
             if p.stream_type == "video" and p.seconds is not None]
    return sorted(times)


def detect_vfr(packets: list[PacketInfo]) -> bool:
    durations = {p.duration for p in packets
                 if p.stream_type == "video" and p.duration > 0}
    return len(durations) > 1


def audio_clock_anchors(path: str | Path, log: ErrorLog
                        ) -> tuple[list[int], list[Fraction], int]:
    """Decode audio and pair each frame's sample position with its packet time.

    Returns (anchor_samples, packet_times_seconds, sample_rate). The decoded
    sample counter is the audio clock; packet pts is the container clock. Their
    divergence is what drift estimation measures.
    """
    src = str(path)
    container = _open(path)
    anchors: list[int] = []
    times: list[Fraction] = []
    rate = 0
    with container:
        try:
            stream = next((s for s in container.streams if s.type == "audio"), None)
            if stream is None:
                log.add("missing_audio_track", f"{src} has no audio stream", src, 0, -1)
                return [], [], 0
            rate = stream.sample_rate
            tb = Fraction(stream.time_base.numerator, stream.time_base.denominator)
            acc = 0
            for frame in container.decode(stream):
                if frame.pts is not None:
                    anchors.append(acc)
                    times.append(frame.pts * tb)
                acc += frame.samples
        except _AV_ERRORS as exc:
            log.add("bad_packet", f"audio decode failed in {src}: {exc}",
                    src, 0, len(anchors))
    return anchors, times, rate
