"""Media probing with structured error localisation.

The probe opens one file per track and reports the stream inventory, the exact
keyframe times of the video stream and every anomaly as a structured
:class:`~avtimeline.errors.MediaError` tied to a track, media timestamp, frame
index or sample range.  Errors never carry media payload.

Decoding is bounded: files whose declared length is small are decoded in full
so truncation can be detected, longer files only decode a prefix.  The
comparison of decoded units against the declared duration tells the probe where
the file stops being readable, which is the first anomaly the acceptance
criteria ask for (within one frame or one audio block).
"""

from __future__ import annotations

import hashlib
import os
from dataclasses import dataclass, field, replace
from fractions import Fraction
from typing import Any

import av

from .errors import MediaError, MediaLocation, Severity, Stage
from .model import Rate, TimeBase

# Decode bounds: short streams are fully decoded to detect truncation, long
# streams only a prefix.  Discarded frames keep memory flat.
FULL_DECODE_SECONDS = 60
FULL_DECODE_FRAMES = 900
AUDIO_BLOCK = 1024

SUPPORTED_AUDIO_LAYOUTS = {
    "mono": 1,
    "stereo": 2,
    "2.1": 3,
    "3.0": 3,
    "4.0": 4,
    "quad": 4,
    "5.0": 5,
    "5.1": 6,
    "6.1": 7,
    "7.1": 8,
}


@dataclass(frozen=True)
class StreamInfo:
    """Normalized view of one container stream."""

    index: int
    kind: str
    codec: str
    time_base: TimeBase
    start_pts: int = 0
    duration: int = 0
    frames: int | None = None
    width: int | None = None
    height: int | None = None
    rate: Rate | None = None
    sample_rate: int | None = None
    channels: int | None = None
    layout: str | None = None
    sample_format: str | None = None
    color_primaries: int | None = None
    color_trc: int | None = None
    colorspace: int | None = None
    color_range: int | None = None
    language: str | None = None

    @property
    def start_time(self) -> Fraction:
        return Fraction(self.start_pts, self.time_base.ticks_per_second)

    @property
    def duration_seconds(self) -> Fraction:
        return Fraction(self.duration, self.time_base.ticks_per_second)

    def with_duration_seconds(self, duration: Fraction) -> "StreamInfo":
        ticks = int(round(duration * self.time_base.ticks_per_second))
        return replace(self, duration=max(ticks, 0))

    def declared_frames(self) -> int | None:
        """Frame count derived from the duration when the header omits it."""
        if self.kind != "video" or self.rate is None or self.duration <= 0:
            return None
        return int(round(self.duration_seconds / self.rate.frame_duration))

    def declared_samples(self) -> int | None:
        if self.kind != "audio" or not self.sample_rate or self.duration <= 0:
            return None
        return int(round(self.duration_seconds * self.sample_rate))


@dataclass
class ProbeResult:
    """Everything the probe learned about one media file."""

    path: str
    sha256: str
    size: int
    format_name: str
    duration: Fraction | None
    streams: list[StreamInfo]
    keyframe_times: list[Fraction] = field(default_factory=list)
    errors: list[MediaError] = field(default_factory=list)
    decoded_video_frames: int | None = None
    decoded_audio_samples: int | None = None

    def video(self) -> StreamInfo | None:
        return next((s for s in self.streams if s.kind == "video"), None)

    def audio(self) -> StreamInfo | None:
        return next((s for s in self.streams if s.kind == "audio"), None)

    @property
    def media_duration(self) -> Fraction | None:
        durations = [s.duration_seconds for s in self.streams if s.duration > 0]
        if durations:
            return max(durations)
        return self.duration

    def keyframe_index(self) -> list[int]:
        video = self.video()
        if video is None or video.rate is None:
            return []
        step = video.rate.frame_duration
        return [int(round(t / step)) for t in self.keyframe_times]


def file_sha256(path: str, chunk: int = 1 << 20) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        while True:
            block = handle.read(chunk)
            if not block:
                break
            digest.update(block)
    return digest.hexdigest()


def _time_base(stream: av.stream.Stream) -> TimeBase:
    tb = stream.time_base
    if tb is None:
        return TimeBase(1_000_000)
    return TimeBase(int(tb.denominator))


def _rate_of(stream: av.stream.Stream) -> Rate | None:
    for attr in ("average_rate", "guessed_rate", "base_rate"):
        value = getattr(stream, attr, None)
        if value is None:
            continue
        try:
            fraction = Fraction(value)
        except (TypeError, ValueError, ZeroDivisionError):
            continue
        if fraction > 0:
            return Rate(fraction.numerator, fraction.denominator)
    return None


def _color_value(context: Any, attr: str) -> int | None:
    try:
        raw = getattr(context, attr, None)
    except Exception:  # pragma: no cover - defensive against wrapper errors
        return None
    if raw is None:
        return None
    try:
        return int(raw)
    except (TypeError, ValueError):
        return None


def _describe_stream(stream: av.stream.Stream) -> StreamInfo:
    context = stream.codec_context
    fields: dict[str, Any] = {
        "index": stream.index,
        "kind": stream.type,
        "codec": context.name or "unknown",
        "time_base": _time_base(stream),
        "start_pts": int(stream.start_time or 0),
        "duration": int(stream.duration or 0),
        "frames": int(stream.frames or 0) or None,
        "language": stream.metadata.get("language"),
    }
    if stream.type == "video":
        fields.update(
            width=int(getattr(context, "width", 0) or 0),
            height=int(getattr(context, "height", 0) or 0),
            rate=_rate_of(stream),
            color_primaries=_color_value(context, "color_primaries"),
            color_trc=_color_value(context, "color_trc"),
            colorspace=_color_value(context, "colorspace"),
            color_range=_color_value(context, "color_range"),
        )
    elif stream.type == "audio":
        layout = getattr(context, "layout", None)
        fmt = getattr(context, "format", None)
        fields.update(
            sample_rate=int(getattr(context, "sample_rate", 0) or 0),
            layout=getattr(layout, "name", None) if layout is not None else None,
            channels=len(getattr(layout, "channels", ())) if layout is not None else 0,
            sample_format=getattr(fmt, "name", None),
        )
    return StreamInfo(**fields)


def _decode_stream(
    container: av.container.InputContainer,
    info: StreamInfo,
) -> tuple[int, MediaError | None, bool]:
    """Decode a stream (bounded) and return (units, first_error, exhausted).

    "units" are decoded video frames or decoded audio samples. "exhausted" is
    true when the decoder reached end of stream before the decode bound, which
    is the only situation where the declared-length comparison applies.
    """
    stream = container.streams[info.index]
    units = 0
    frames_seen = 0
    exhausted = True
    limit = FULL_DECODE_FRAMES if info.kind == "video" else FULL_DECODE_FRAMES * 8
    try:
        for frame in container.decode(stream):
            frames_seen += 1
            units += 1 if info.kind == "video" else int(frame.samples)
            if frames_seen >= limit:
                exhausted = False
                break
    except (av.error.FFmpegError, OSError, ValueError) as exc:
        if info.kind == "video":
            step = info.rate.frame_duration if info.rate else Fraction(0)
            location = MediaLocation(
                pts=step * frames_seen if step else None,
                frame=frames_seen,
                stream_index=info.index,
            )
        else:
            location = MediaLocation(
                sample_start=units,
                sample_end=units + AUDIO_BLOCK,
                stream_index=info.index,
            )
        return units, MediaError(
            code="decode_failed",
            stage=Stage.PROBE,
            message=f"decoder stopped after {frames_seen} frames: {exc}",
            location=location,
            severity=Severity.ERROR,
        ), False
    return units, None, exhausted


def _truncation_error(info: StreamInfo, decoded: int) -> MediaError | None:
    """Return an error when the decodable units fall short of the declared length."""
    if info.kind == "video":
        declared = info.declared_frames()
        if declared is None or declared <= 0 or decoded <= 0:
            return None
        if decoded + 1 < declared:
            step = info.rate.frame_duration if info.rate else Fraction(0)
            return MediaError(
                code="truncated_stream",
                stage=Stage.PROBE,
                message=(
                    f"video stream declares {declared} frames but only {decoded} are decodable"
                ),
                location=MediaLocation(
                    pts=step * decoded if step else None,
                    frame=decoded,
                    stream_index=info.index,
                ),
                severity=Severity.ERROR,
                context={"declared_frames": declared, "decoded_frames": decoded},
            )
        return None
    declared_samples = info.declared_samples()
    if declared_samples is None or declared_samples <= 0 or decoded <= 0:
        return None
    # Audio encoders pad the final block, so a shortfall only counts when it
    # exceeds one block.
    if decoded + AUDIO_BLOCK < declared_samples:
        return MediaError(
            code="truncated_stream",
            stage=Stage.PROBE,
            message=(
                f"audio stream declares {declared_samples} samples but only "
                f"{decoded} are decodable"
            ),
            location=MediaLocation(
                sample_start=decoded,
                sample_end=declared_samples,
                stream_index=info.index,
            ),
            severity=Severity.ERROR,
            context={"declared_samples": declared_samples, "decoded_samples": decoded},
        )
    return None


def _located(error: MediaError, track: str) -> MediaError:
    loc = error.location
    return replace(
        error,
        location=MediaLocation(
            track=track,
            pts=loc.pts,
            frame=loc.frame,
            sample_start=loc.sample_start,
            sample_end=loc.sample_end,
            stream_index=loc.stream_index,
        ),
    )


def _rewind(container: av.container.InputContainer) -> None:
    """Seek back to the start before a second pass over a stream."""
    try:
        container.seek(0, backward=True)
    except (av.error.FFmpegError, OSError, ValueError):
        pass


def _probe_keyframes(
    container: av.container.InputContainer, video: StreamInfo
) -> tuple[list[Fraction], list[str]]:
    """Return keyframe times and demux anomalies (with packet position)."""
    times: list[Fraction] = []
    anomalies: list[str] = []
    for packet in container.demux(container.streams[video.index]):
        if packet.size == 0 or not packet.is_keyframe:
            continue
        pts = packet.pts if packet.pts is not None else packet.dts
        if pts is None:
            anomalies.append(f"keyframe at pos={packet.pos} has no timestamp")
            continue
        times.append(Fraction(int(pts), video.time_base.ticks_per_second))
    return sorted(set(times)), anomalies


def _validate_stream_metadata(
    track: str, video: StreamInfo | None, audio: StreamInfo | None
) -> list[MediaError]:
    errors: list[MediaError] = []
    if video is not None:
        if not video.rate:
            errors.append(
                MediaError(
                    code="unknown_frame_rate",
                    stage=Stage.PROBE,
                    message="video stream has no usable frame rate",
                    location=MediaLocation(track=track, stream_index=video.index),
                )
            )
        if not video.width or not video.height:
            errors.append(
                MediaError(
                    code="unknown_frame_size",
                    stage=Stage.PROBE,
                    message="video stream has no usable frame size",
                    location=MediaLocation(track=track, stream_index=video.index),
                )
            )
    if audio is not None:
        if not audio.sample_rate:
            errors.append(
                MediaError(
                    code="unknown_sample_rate",
                    stage=Stage.PROBE,
                    message="audio stream has no sample rate",
                    location=MediaLocation(track=track, stream_index=audio.index),
                )
            )
        layout = audio.layout or ""
        expected = SUPPORTED_AUDIO_LAYOUTS.get(layout)
        if expected is None:
            errors.append(
                MediaError(
                    code="unsupported_layout",
                    stage=Stage.PROBE,
                    message=f"audio layout {layout!r} ({audio.channels} channels) is unsupported",
                    location=MediaLocation(track=track, stream_index=audio.index),
                    context={"layout": layout, "channels": audio.channels},
                )
            )
        elif audio.channels != expected:
            errors.append(
                MediaError(
                    code="layout_channel_mismatch",
                    stage=Stage.PROBE,
                    message=(
                        f"layout {layout!r} declares {expected} channels but the stream has "
                        f"{audio.channels}"
                    ),
                    location=MediaLocation(track=track, stream_index=audio.index),
                )
            )
    return errors


def _empty_result(
    path: str, sha: str, size: int, errors: list[MediaError], fmt: str = ""
) -> ProbeResult:
    return ProbeResult(
        path=path, sha256=sha, size=size, format_name=fmt, duration=None, streams=[], errors=errors
    )


def probe_media(path: str, track_id: str | None = None) -> ProbeResult:
    """Probe *path* and return metadata plus structured errors.

    Media problems never raise: they are returned in ``errors`` so the caller
    can apply a stop/skip policy.
    """
    track = track_id or os.path.basename(path)
    size = os.path.getsize(path)
    if size == 0:
        return _empty_result(
            path,
            hashlib.sha256(b"").hexdigest(),
            0,
            [
                MediaError(
                    code="empty_file",
                    stage=Stage.PROBE,
                    message="media file is empty",
                    location=MediaLocation(track=track),
                    severity=Severity.FATAL,
                )
            ],
        )
    sha = file_sha256(path)
    try:
        container = av.open(path)
    except (av.error.FFmpegError, OSError, ValueError) as exc:
        return _empty_result(
            path,
            sha,
            size,
            [
                MediaError(
                    code="cannot_open",
                    stage=Stage.PROBE,
                    message=f"cannot open media: {exc}",
                    location=MediaLocation(track=track),
                    severity=Severity.FATAL,
                )
            ],
        )
    errors: list[MediaError] = []
    decoded_video: int | None = None
    decoded_audio: int | None = None
    keyframes: list[Fraction] = []
    try:
        format_name = container.format.name or ""
        duration = (
            Fraction(int(container.duration), 1_000_000) if container.duration else None
        )
        streams = [
            _describe_stream(s) for s in container.streams if s.type in ("video", "audio")
        ]
        if not streams:
            errors.append(
                MediaError(
                    code="no_media_streams",
                    stage=Stage.PROBE,
                    message="file contains no audio or video stream",
                    location=MediaLocation(track=track),
                    severity=Severity.FATAL,
                )
            )
            return _empty_result(path, sha, size, errors, format_name)
        if duration is not None:
            streams = [s if s.duration else s.with_duration_seconds(duration) for s in streams]
        video = next((s for s in streams if s.kind == "video"), None)
        audio = next((s for s in streams if s.kind == "audio"), None)
        errors.extend(_validate_stream_metadata(track, video, audio))
        # Decode first, then rewind and demux because demuxing drains the
        # container's packet queue for the whole file.
        for info in streams:
            _rewind(container)
            units, decode_error, exhausted = _decode_stream(container, info)
            if info.kind == "video":
                decoded_video = units
            else:
                decoded_audio = units
            if decode_error is not None:
                errors.append(_located(decode_error, track))
                continue
            if not exhausted:
                continue
            truncation = _truncation_error(info, units)
            if truncation is not None:
                errors.append(_located(truncation, track))
        if video is not None:
            _rewind(container)
            try:
                keyframes, anomalies = _probe_keyframes(container, video)
            except (av.error.FFmpegError, OSError, ValueError) as exc:
                anomalies = [f"demuxing keyframes failed: {exc}"]
            for anomaly in anomalies:
                errors.append(
                    MediaError(
                        code="demux_failed",
                        stage=Stage.PROBE,
                        message=anomaly,
                        location=MediaLocation(track=track, stream_index=video.index),
                    )
                )
            if not keyframes and not anomalies:
                errors.append(
                    MediaError(
                        code="no_keyframes",
                        stage=Stage.PROBE,
                        message="video stream exposes no keyframes",
                        location=MediaLocation(track=track, stream_index=video.index),
                    )
                )
    finally:
        container.close()
    return ProbeResult(
        path=path,
        sha256=sha,
        size=size,
        format_name=format_name,
        duration=duration,
        streams=streams,
        keyframe_times=keyframes,
        errors=errors,
        decoded_video_frames=decoded_video,
        decoded_audio_samples=decoded_audio,
    )
