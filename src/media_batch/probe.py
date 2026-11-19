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

    @property
    def seconds(self) -> Fraction | None:
        return None if self.pts is None else self.pts * self.time_base


def stream_info(path: str | Path, log: ErrorLog | None = None) -> StreamInfo:
    src = str(path)
    try:
        container = av.open(src)
    except av.error.InvalidDataError as exc:
        raise MediaBatchError("bad_container", f"cannot open {src}: {exc}", src, 0)
    except (FileNotFoundError, OSError) as exc:
        raise MediaBatchError("io_error", f"cannot open {src}: {exc}", src, 0)
    with container:
        audio = next((s for s in container.streams if s.type == "audio"), None)
        video = next((s for s in container.streams if s.type == "video"), None)
        if audio is None and log is not None:
            log.add("missing_audio_track", f"{src} has no audio stream", src, 0, -1)
        fps = None
        if video is not None and video.average_rate:
            fps = Fraction(video.average_rate.numerator, video.average_rate.denominator)
        duration = None
        if container.duration is not None:
            duration = Fraction(container.duration, av.time_base) if isinstance(
                av.time_base, int) else Fraction(container.duration, 1) * av.time_base
        return StreamInfo(
            path=src,
            has_audio=audio is not None,
            has_video=video is not None,
            sample_rate=audio.sample_rate if audio is not None else 0,
            channels=audio.channels if audio is not None else 0,
            video_fps=fps,
            vfr=False,  # refined by probe_packets
            duration=duration,
        )


def probe_packets(path: str | Path, log: ErrorLog,
                  max_packets: int | None = None) -> list[PacketInfo]:
    """Demux all packets with their rational timestamps.

    Corrupt packets are logged with their byte position and skipped.
    """
    src = str(path)
    try:
        container = av.open(src)
    except (av.error.InvalidDataError, FileNotFoundError, OSError) as exc:
        raise MediaBatchError("bad_container", f"cannot open {src}: {exc}", src, 0)
    packets: list[PacketInfo] = []
    with container:
        bases = {s.index: Fraction(s.time_base.numerator, s.time_base.denominator)
                 for s in container.streams}
        types = {s.index: s.type for s in container.streams}
        try:
            demux = container.demux()
            for pkt in demux:
                if pkt.stream_index not in bases:
                    continue
                packets.append(PacketInfo(
                    stream_type=types[pkt.stream_index],
                    pts=pkt.pts, dts=pkt.dts,
                    time_base=bases[pkt.stream_index],
                    duration=pkt.duration or 0,
                    size=pkt.size,
                ))
                if max_packets is not None and len(packets) >= max_packets:
                    break
        except av.error.InvalidDataError as exc:
            log.add("bad_packet", f"corrupt packet in {src}: {exc}",
                    src, packets[-1].size if packets else 0, len(packets))
    return packets


def video_frame_times(path: str | Path, log: ErrorLog) -> list[Fraction]:
    """Sorted-by-presentation video packet timestamps (seconds, rational)."""
    pkts = probe_packets(path, log)
    times = [p.seconds for p in pkts if p.stream_type == "video" and p.seconds is not None]
    return sorted(times)


def detect_vfr(packets: list[PacketInfo]) -> bool:
    durations = {p.duration for p in packets
                 if p.stream_type == "video" and p.duration > 0}
    return len(durations) > 1
