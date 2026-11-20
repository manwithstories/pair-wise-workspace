"""Per-rendition segment encoding.

The encoder consumes the composed program (video frames at the timeline rate,
mixed stereo audio at 48 kHz) and writes one MP4 per rendition and segment.
Every segment starts with a keyframe, so players can switch rendition at any
shared boundary.  Encoding parameters are fixed and thread counts pinned to
one, which makes output byte-identical across runs.
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from fractions import Fraction
from typing import Sequence

import av
import numpy as np

from .errors import MediaError, MediaLocation, Severity, Stage
from .model import Rate
from .normalize import atomic_temp, safe_remove
from .probe import file_sha256
from .segment import Segment

AUDIO_SAMPLE_RATE = 48_000


@dataclass(frozen=True)
class Rendition:
    """One output representation."""

    name: str
    width: int
    height: int
    video_bitrate: int
    audio_bitrate: int
    preset: str = "ultrafast"
    crf: int = 30

    def to_dict(self) -> dict:
        return {
            "name": self.name,
            "width": self.width,
            "height": self.height,
            "video_bitrate": self.video_bitrate,
            "audio_bitrate": self.audio_bitrate,
            "preset": self.preset,
            "crf": self.crf,
        }


@dataclass(frozen=True)
class EncodedSegment:
    rendition: str
    index: int
    path: str
    sha256: str
    size: int
    duration: Fraction
    video_frames: int
    audio_samples: int
    actual_video_frames: int
    actual_audio_samples: int

    def to_dict(self, root: str | None = None) -> dict:
        path = os.path.relpath(self.path, root) if root else self.path
        return {
            "rendition": self.rendition,
            "index": self.index,
            "path": path,
            "sha256": self.sha256,
            "size": self.size,
            "duration": str(self.duration),
            "video_frames": self.video_frames,
            "audio_samples": self.audio_samples,
            "actual_video_frames": self.actual_video_frames,
            "actual_audio_samples": self.actual_audio_samples,
        }


class Program:
    """Composed timeline media used as encoder input."""

    def __init__(
        self,
        video_frames: Sequence[np.ndarray],
        audio: np.ndarray,
        rate: Rate,
        audio_rate: int = AUDIO_SAMPLE_RATE,
    ) -> None:
        self.video_frames = video_frames
        self.audio = audio
        self.rate = rate
        self.audio_rate = audio_rate

    @property
    def audio_samples(self) -> int:
        return int(self.audio.shape[1])


def _video_frame_at(program: Program, index: int) -> np.ndarray | None:
    if 0 <= index < len(program.video_frames):
        return program.video_frames[index]
    return None


def _slice_audio(program: Program, start: int, count: int) -> np.ndarray:
    end = min(start + count, program.audio_samples)
    if start >= end:
        return np.zeros((2, 0), dtype=np.float32)
    return np.ascontiguousarray(program.audio[:, start:end])


def encode_segment(
    program: Program,
    segment: Segment,
    rendition: Rendition,
    out_dir: str,
    video_rate: Rate,
) -> tuple[EncodedSegment | None, MediaError | None]:
    """Encode one segment for one rendition."""
    os.makedirs(out_dir, exist_ok=True)
    path = os.path.join(out_dir, f"{rendition.name}_{segment.index:05d}.mp4")
    temp = atomic_temp(out_dir, ".mp4")
    frames = _slice_video(program, segment)
    audio = _slice_audio(program, segment.audio_start_sample, segment.audio_samples)
    has_video = len(frames) > 0
    has_audio = audio.shape[1] > 0
    if not has_video and not has_audio:
        return None, MediaError(
            code="empty_segment",
            stage=Stage.ENCODE,
            message=f"segment {segment.index} has neither video nor audio",
            location=MediaLocation(track=rendition.name, pts=segment.start),
            severity=Severity.ERROR,
        )
    try:
        container = av.open(
            temp, "w", format="mp4", options={"fflags": "+bitexact", "movflags": "+faststart"}
        )
        try:
            video_stream = None
            if has_video:
                video_stream = container.add_stream("libx264", rate=video_rate.value)
                video_stream.width = rendition.width
                video_stream.height = rendition.height
                video_stream.pix_fmt = "yuv420p"
                video_stream.time_base = video_rate.frame_duration
                video_stream.bit_rate = rendition.video_bitrate
                video_stream.options = {
                    "preset": rendition.preset,
                    "crf": str(rendition.crf),
                    "x264-params": (
                        f"keyint={max(len(frames), 1)}:min-keyint={max(len(frames), 1)}"
                        ":scenecut=0:open-gop=0:bframes=0:threads=1"
                    ),
                    "threads": "1",
                }
            audio_stream = None
            if has_audio:
                audio_stream = container.add_stream("aac", rate=AUDIO_SAMPLE_RATE)
                audio_stream.layout = "stereo"
                audio_stream.bit_rate = rendition.audio_bitrate
            if video_stream is not None:
                for offset, frame in enumerate(frames):
                    source = av.VideoFrame.from_ndarray(
                        np.ascontiguousarray(frame), format="rgb24"
                    )
                    converted = source.reformat(
                        width=rendition.width, height=rendition.height, format="yuv420p"
                    )
                    converted.pts = offset
                    converted.time_base = video_rate.frame_duration
                    for packet in video_stream.encode(converted):
                        container.mux(packet)
                for packet in video_stream.encode():
                    container.mux(packet)
            if audio_stream is not None:
                pcm = av.AudioFrame.from_ndarray(
                    np.ascontiguousarray(audio), format="fltp", layout="stereo"
                )
                pcm.sample_rate = AUDIO_SAMPLE_RATE
                pcm.pts = 0
                pcm.time_base = Fraction(1, AUDIO_SAMPLE_RATE)
                for packet in audio_stream.encode(pcm):
                    container.mux(packet)
                for packet in audio_stream.encode():
                    container.mux(packet)
        finally:
            container.close()
        os.replace(temp, path)
    except (av.error.FFmpegError, OSError, ValueError) as exc:
        safe_remove(temp)
        return None, MediaError(
            code="encode_failed",
            stage=Stage.ENCODE,
            message=f"cannot encode {rendition.name} segment {segment.index}: {exc}",
            location=MediaLocation(track=rendition.name, pts=segment.start),
            severity=Severity.ERROR,
        )
    actual = inspect_segment(path)
    if actual is None:
        return None, MediaError(
            code="encode_unreadable",
            stage=Stage.ENCODE,
            message=f"encoded segment {path} cannot be reopened",
            location=MediaLocation(track=rendition.name, pts=segment.start),
            severity=Severity.ERROR,
        )
    actual_frames, actual_samples = actual
    encoded = EncodedSegment(
        rendition=rendition.name,
        index=segment.index,
        path=path,
        sha256=file_sha256(path),
        size=os.path.getsize(path),
        duration=segment.duration,
        video_frames=segment.video_frames,
        audio_samples=segment.audio_samples,
        actual_video_frames=actual_frames,
        actual_audio_samples=actual_samples,
    )
    return encoded, None


def _slice_video(program: Program, segment: Segment) -> list[np.ndarray]:
    frames: list[np.ndarray] = []
    for offset in range(segment.video_frames):
        frame = _video_frame_at(program, segment.video_start_frame + offset)
        if frame is None:
            black = np.zeros((program.video_frames[0].shape if program.video_frames else (1, 1, 1)),
                             dtype=np.uint8)
            frames.append(black)
        else:
            frames.append(frame)
    return frames


def inspect_segment(path: str) -> tuple[int, int] | None:
    """Return (video frames, audio samples) as recorded in the container."""
    try:
        container = av.open(path)
    except (av.error.FFmpegError, OSError, ValueError):
        return None
    try:
        frames = 0
        samples = 0
        for stream in container.streams:
            if stream.type == "video":
                step = _stream_frame_duration(stream)
                if step and stream.duration:
                    frames = int(round((stream.duration * stream.time_base) / step))
                else:
                    frames = int(stream.frames or 0)
            elif stream.type == "audio":
                if stream.duration and stream.time_base:
                    samples = int(round(stream.duration * stream.time_base * AUDIO_SAMPLE_RATE))
        return frames, samples
    finally:
        container.close()


def _stream_frame_duration(stream: av.stream.Stream) -> Fraction | None:
    rate = stream.average_rate
    if rate is None:
        return None
    try:
        value = Fraction(rate)
    except (TypeError, ValueError, ZeroDivisionError):
        return None
    if value <= 0:
        return None
    return 1 / value


def encode_all(
    program: Program,
    segments: Sequence[Segment],
    renditions: Sequence[Rendition],
    out_dir: str,
    video_rate: Rate,
) -> tuple[list[EncodedSegment], list[MediaError]]:
    encoded: list[EncodedSegment] = []
    errors: list[MediaError] = []
    for rendition in renditions:
        rendition_dir = os.path.join(out_dir, rendition.name)
        for segment in segments:
            result, error = encode_segment(program, segment, rendition, rendition_dir, video_rate)
            if error is not None:
                errors.append(error)
                continue
            assert result is not None
            encoded.append(result)
    return encoded, errors
