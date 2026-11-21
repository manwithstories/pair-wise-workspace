"""PyAV demultiplexing: ordered audio/video samples with error classification.

The probe is the single place that touches the container.  It iterates the
*global* demuxer (single-stream ``demux(stream)`` is unreliable for
multi-stream files in this PyAV build) and emits ordered, decoded
:class:`MediaSample` objects carrying raw container PTS plus the stream's
time base.  Timestamp correction (unwrap/drift/shared origin) happens
downstream in :mod:`archive_media.timebase`; the probe stays mechanical.

Recoverable container failures (truncation, a corrupt packet) are converted
to :class:`~archive_media.errors.RecoverableMediaError` with the byte/stream
context, while structural problems (missing required track, unsupported
codec, unopenable file) are raised as fatal.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Iterator

import av
import numpy as np

from .errors import (
    E_CORRUPT_PACKET,
    E_MISSING_TRACK,
    E_TRUNCATED,
    E_UNSUPPORTED,
    ErrorLog,
    FatalMediaError,
    RecoverableMediaError,
)

VIDEO = "video"
AUDIO = "audio"
MAX_FILE_BYTES = 512 * 1024 * 1024


@dataclass
class StreamInfo:
    """Static descriptor of one demuxed stream."""

    name: str
    codec: str
    time_base: float
    sample_rate: int | None = None
    channels: int | None = None
    width: int | None = None
    height: int | None = None
    frame_rate: float | None = None


@dataclass
class MediaSample:
    """One decoded frame on the shared processing path."""

    track: str
    raw_pts: int
    time_base: float
    ref_index: int
    data: np.ndarray | av.VideoFrame
    sample_rate: int | None = None
    keyframe: bool = False

    @property
    def pts_seconds(self) -> float:
        return self.raw_pts * self.time_base


class Probe:
    """Opens a container and streams ordered, decoded samples."""

    def __init__(self, path: str, errors: ErrorLog) -> None:
        self.path = path
        self.errors = errors
        self._enforce_size_limit()
        try:
            self.container = av.open(path)
        except av.FFmpegError as exc:
            raise FatalMediaError(
                E_UNSUPPORTED, f"cannot open container: {exc}", track=None
            ) from exc
        self.streams: dict[str, StreamInfo] = self._describe_streams()
        self._counters = {VIDEO: 0, AUDIO: 0}
        self._truncation_reported = False
        self._last_sample_s: dict[str, float] = {}

    def _enforce_size_limit(self) -> None:
        size = Path(self.path).stat().st_size
        if size > MAX_FILE_BYTES:
            raise FatalMediaError(
                E_UNSUPPORTED,
                f"file {size} bytes exceeds limit {MAX_FILE_BYTES}",
            )

    def _describe_streams(self) -> dict[str, StreamInfo]:
        infos: dict[str, StreamInfo] = {}
        for stream in self.container.streams:
            if stream.type not in (VIDEO, AUDIO):
                continue
            ctx = stream.codec_context
            avg_rate = getattr(stream, "average_rate", None)
            info = StreamInfo(
                name=stream.type,
                codec=getattr(ctx, "name", None) or "unknown",
                time_base=float(stream.time_base),
                sample_rate=getattr(ctx, "sample_rate", None),
                channels=getattr(ctx, "channels", None),
                width=getattr(ctx, "width", None),
                height=getattr(ctx, "height", None),
                frame_rate=float(avg_rate) if avg_rate else None,
            )
            infos[stream.type] = info
        if VIDEO not in infos:
            raise FatalMediaError(E_MISSING_TRACK, "no video track found", track=VIDEO)
        return infos

    def require_audio(self) -> StreamInfo:
        if AUDIO not in self.streams:
            raise FatalMediaError(E_MISSING_TRACK, "no audio track found", track=AUDIO)
        return self.streams[AUDIO]

    def samples(self) -> Iterator[MediaSample]:
        """Yield decoded samples in container order, isolating bad packets."""
        stream_by_index = {s.index: s for s in self.container.streams}
        for packet in self.container.demux():
            stream = stream_by_index.get(packet.stream.index)
            if stream is None or stream.type not in (VIDEO, AUDIO):
                continue
            track = stream.type
            try:
                frames = packet.decode()
            except av.FFmpegError as exc:
                self._report_packet_error(track, packet, exc)
                continue
            for frame in frames:
                ref_index = self._counters[track]
                self._counters[track] += 1
                sample = self._to_sample(track, frame, ref_index)
                self._last_sample_s[track] = sample.pts_seconds
                yield sample
        self._report_truncation_if_needed()

    def _to_sample(self, track: str, frame, ref_index: int) -> MediaSample:
        if track == AUDIO:
            arr = frame.to_ndarray()
            # Planar layouts arrive as (channels, n); normalize to (n, ch).
            if arr.ndim == 2 and arr.shape[0] <= 2:
                arr = np.ascontiguousarray(arr.T)
            return MediaSample(
                track=AUDIO,
                raw_pts=int(frame.pts if frame.pts is not None else ref_index * 1024),
                time_base=float(frame.time_base) if frame.time_base else 1 / (frame.sample_rate or 24000),
                ref_index=ref_index,
                data=np.asarray(arr, dtype=np.float32),
                sample_rate=frame.sample_rate,
            )
        return MediaSample(
            track=VIDEO,
            raw_pts=int(frame.pts if frame.pts is not None else ref_index),
            time_base=float(frame.time_base) if frame.time_base else self.streams[VIDEO].time_base,
            ref_index=ref_index,
            data=frame,
            keyframe=bool(frame.key_frame),
        )

    def _report_packet_error(self, track: str, packet, exc: av.FFmpegError) -> None:
        pts = packet.pts
        time_s = None
        if pts is not None:
            tb = float(packet.time_base) if packet.time_base else 0.0
            time_s = pts * tb
        # End-of-file / partial frame messages are truncation; others are
        # corrupt packets.  FFmpeg exposes EOF via errno EOF (AVERROR_EOF).
        code = E_TRUNCATED if self._looks_like_truncation(exc) else E_CORRUPT_PACKET
        err = RecoverableMediaError(
            code,
            f"{track}: {exc.__class__.__name__} while decoding packet",
            time_s=time_s,
            track=track,
        )
        self.errors.recoverable(err, end_s=time_s)
        if code == E_TRUNCATED:
            self._truncation_reported = True

    @staticmethod
    def _looks_like_truncation(exc: av.FFmpegError) -> bool:
        text = str(exc).lower()
        return any(token in text for token in ("end of file", "eof", "partial", "truncat"))

    def _report_truncation_if_needed(self) -> None:
        """Flag truncation when a stream ends well before its declared duration.

        FFmpeg tolerates many cut tails without raising, so compare the last
        decoded sample time against the container's declared stream duration.
        Missing more than half a second of declared content is truncation.
        """
        if self._truncation_reported:
            return
        for stream in self.container.streams:
            if stream.type not in (VIDEO, AUDIO) or stream.duration is None:
                continue
            declared_s = float(stream.duration * stream.time_base)
            actual_s = self._last_sample_s.get(stream.type, 0.0)
            if declared_s - actual_s > 0.5:
                err = RecoverableMediaError(
                    E_TRUNCATED,
                    f"{stream.type}: stream ends at {actual_s:.3f}s "
                    f"but declares {declared_s:.3f}s",
                    time_s=actual_s,
                    track=stream.type,
                )
                self.errors.recoverable(err, end_s=declared_s)
                self._truncation_reported = True
                return

    def close(self) -> None:
        self.container.close()

    def __enter__(self) -> "Probe":
        return self

    def __exit__(self, *exc_info) -> None:
        self.close()
