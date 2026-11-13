"""MP4/MOV/MKV frame extraction which reuses the image normalization layer."""

from __future__ import annotations

import re
import shutil
import subprocess
import warnings
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from mediaidx.canonical import MediaDecodeError, MediaFamily, VIDEO_FORMATS
from mediaidx.loaders.image_loader import normalize_rgb_array


CANONICAL_VIDEO_SIDE = 256
VIDEO_SAMPLE_FRAMES = 16

_DURATION_RE = re.compile(
    r"Duration:\s*(\d{1,2}):(\d{2}):(\d{2}(?:\.\d+)?)", re.IGNORECASE
)
_DIMENSION_RE = re.compile(r"Video:.*?(\d{2,})x(\d{2,})", re.IGNORECASE)
_FPS_RE = re.compile(r"(\d+(?:\.\d+)?)\s+fps", re.IGNORECASE)


@dataclass
class NormalizedVideo:
    frames: list[np.ndarray]
    timestamps: list[float]
    duration: float
    source_width: int
    source_height: int
    source_fps: float | None

    def metadata(self) -> dict[str, object]:
        return {
            "duration": self.duration,
            "source_width": self.source_width,
            "source_height": self.source_height,
            "source_fps": self.source_fps,
            "extracted_frames": len(self.frames),
            "frame_timestamps": [round(value, 6) for value in self.timestamps],
        }


def _ffmpeg_executable() -> str:
    try:
        with warnings.catch_warnings():
            warnings.simplefilter("ignore", UserWarning)
            import imageio_ffmpeg

        return imageio_ffmpeg.get_ffmpeg_exe()
    except Exception:
        executable = shutil.which("ffmpeg")
        if executable:
            return executable
        raise MediaDecodeError(
            "dependency_error", "imageio-ffmpeg and a system ffmpeg binary are unavailable"
        )


def probe_video(path: Path) -> tuple[float, int, int, float | None]:
    ffmpeg = _ffmpeg_executable()
    try:
        process = subprocess.run(
            [ffmpeg, "-hide_banner", "-i", str(path)],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=30,
            check=False,
        )
    except subprocess.TimeoutExpired as exc:
        raise MediaDecodeError("video_probe_timeout", "video probe timed out", retryable=True) from exc
    text = process.stderr.decode("utf-8", errors="replace")
    duration_match = _DURATION_RE.search(text)
    dimension_match = _DIMENSION_RE.search(text)
    if duration_match is None:
        raise MediaDecodeError("corrupt_media", "video duration could not be determined")
    hours, minutes, seconds = duration_match.groups()
    duration = int(hours) * 3600 + int(minutes) * 60 + float(seconds)
    if duration <= 0:
        raise MediaDecodeError("corrupt_media", "video has no positive duration")
    if dimension_match:
        width, height = int(dimension_match.group(1)), int(dimension_match.group(2))
    else:
        width = height = 0
    fps_match = _FPS_RE.search(text)
    fps = float(fps_match.group(1)) if fps_match else None
    return duration, width, height, fps


def _extract_frames(path: Path, duration: float, target_frames: int) -> list[np.ndarray]:
    ffmpeg = _ffmpeg_executable()
    frame_size = CANONICAL_VIDEO_SIDE * CANONICAL_VIDEO_SIDE * 3
    fps = target_frames / duration
    video_filter = (
        f"fps={fps:.12f},"
        "scale=256:256:force_original_aspect_ratio=decrease,"
        "pad=256:256:(ow-iw)/2:(oh-ih)/2:color=black"
    )
    command = [
        ffmpeg,
        "-hide_banner",
        "-loglevel",
        "error",
        "-i",
        str(path),
        "-vf",
        video_filter,
        "-an",
        "-sn",
        "-dn",
        "-f",
        "rawvideo",
        "-pix_fmt",
        "rgb24",
        "pipe:1",
    ]
    try:
        process = subprocess.run(
            command,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=max(60, int(duration * 4)),
            check=False,
        )
    except subprocess.TimeoutExpired as exc:
        raise MediaDecodeError("video_decode_timeout", "video frame extraction timed out", retryable=True) from exc
    if process.returncode != 0 or not process.stdout:
        message = process.stderr.decode("utf-8", errors="replace").strip()
        raise MediaDecodeError("corrupt_media", f"ffmpeg frame extraction failed: {message[:500]}")
    usable = len(process.stdout) // frame_size
    if usable == 0:
        raise MediaDecodeError("corrupt_media", "ffmpeg produced no complete video frames")
    raw = np.frombuffer(process.stdout[: usable * frame_size], dtype=np.uint8)
    raw = raw.reshape(usable, CANONICAL_VIDEO_SIDE, CANONICAL_VIDEO_SIDE, 3)
    frames = [normalize_rgb_array(frame) for frame in raw[:target_frames]]
    if not frames:
        raise MediaDecodeError("corrupt_media", "video contains no decodable frames")
    return frames


def decode_video_file(path: Path, data: bytes | None = None) -> NormalizedVideo:
    # A video must be opened by path; the explicit bytes argument is accepted so
    # the registry has one uniform loader signature.
    if data is not None:
        raise MediaDecodeError("unsupported_format", "video decoding requires a file path")
    duration, width, height, fps = probe_video(path)
    frames = _extract_frames(path, duration, VIDEO_SAMPLE_FRAMES)
    sample_rate = VIDEO_SAMPLE_FRAMES / duration
    timestamps = [index / sample_rate for index in range(len(frames))]
    return NormalizedVideo(
        frames=frames,
        timestamps=timestamps,
        duration=duration,
        source_width=width,
        source_height=height,
        source_fps=fps,
    )


class VideoLoader:
    name = "video-imageio-ffmpeg"
    formats = tuple(sorted(VIDEO_FORMATS))

    def register_into(self, registry: object) -> None:
        registry.register(  # type: ignore[attr-defined]
            MediaFamily.VIDEO,
            self.formats,
            self.name,
            decode_video_file,
            metadata={"loader": self.name},
        )


VIDEO_LOADER = VideoLoader()
load_video = decode_video_file
