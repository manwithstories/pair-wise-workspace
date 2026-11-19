"""Offscreen review-frame rendering with PyAV.

Frames are generated deterministically from the mixed audio: each video
frame shows per-channel level bars for its sample window.  The encoded
stream shares the master tick time base with the audio timeline
(pts in master ticks), and every emitted frame's raw RGB bytes are
SHA-256 hashed for the manifest.

Rendering is offscreen only — no display, no external service.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

import av
import numpy as np

from .clock import MASTER_RATE, frame_to_ticks, ticks_to_samples
from .errors import StageflowError
from .timeline import Delivery


@dataclass
class FrameRecord:
    index: int
    pts_ticks: int
    sha256: str


@dataclass
class FrameRenderer:
    """Consumes delivery-rate PCM, emits fixed-size video frames."""

    delivery: Delivery
    out_path: Path
    target_bitrate: int
    records: list[FrameRecord] = field(default_factory=list)

    def __post_init__(self) -> None:
        d = self.delivery
        self._carry = np.zeros((0, d.channels), np.float32)
        self._next_frame = 0
        self._container: av.container.OutputContainer | None = None
        self._stream = None
        self.total_frames = int(
            (Fraction(d.duration_ticks) * d.fps + MASTER_RATE - 1) // MASTER_RATE
        )

    def open(self) -> None:
        d = self.delivery
        self._container = av.open(str(self.out_path), mode="w")
        self._stream = self._container.add_stream("libx264", rate=d.fps)
        self._stream.width = d.width
        self._stream.height = d.height
        self._stream.pix_fmt = "yuv420p"
        self._stream.bit_rate = self.target_bitrate
        self._stream.time_base = Fraction(1, MASTER_RATE)  # shared with audio timeline
        self._stream.options = {"preset": "ultrafast", "tune": "zerolatency"}

    def frame_window_samples(self, frame: int) -> tuple[int, int]:
        """Delivery-sample window [s0, s1) covered by one video frame."""
        d = self.delivery
        t0 = frame_to_ticks(frame, d.fps)
        t1 = frame_to_ticks(frame + 1, d.fps)
        s0, _ = ticks_to_samples(t0 - d.start_tick, d.sample_rate)
        s1, _ = ticks_to_samples(t1 - d.start_tick, d.sample_rate)
        return s0, s1

    def push(self, samples: np.ndarray) -> None:
        """Append mixed PCM; emit every frame whose window is now complete."""
        self._carry = np.concatenate([self._carry, samples], axis=0)
        while self._next_frame < self.total_frames:
            s0, s1 = self.frame_window_samples(self._next_frame)
            if self._carry.shape[0] < s1:
                break
            window = self._carry[s0:s1]
            self._emit(self._next_frame, window)
            self._next_frame += 1
            # Keep only samples beyond the next frame's start (bounded: < 2 frames).
            n0, _ = self.frame_window_samples(self._next_frame) if (
                self._next_frame < self.total_frames) else (self._carry.shape[0], 0)
            self._carry = self._carry[n0:]

    def flush(self, total_samples: int) -> None:
        """Emit remaining frames, zero-padding the final partial window."""
        while self._next_frame < self.total_frames:
            s0, s1 = self.frame_window_samples(self._next_frame)
            window = np.zeros((s1 - s0, self.delivery.channels), np.float32)
            avail = min(max(0, self._carry.shape[0] - s0), s1 - s0)
            if avail > 0:
                window[:avail] = self._carry[s0 : s0 + avail]
            self._emit(self._next_frame, window)
            self._next_frame += 1
        self._carry = np.zeros((0, self.delivery.channels), np.float32)

    def _emit(self, index: int, window: np.ndarray) -> None:
        rgb = draw_frame(window, self.delivery.width, self.delivery.height, index)
        digest = hashlib.sha256(rgb.tobytes()).hexdigest()
        pts = frame_to_ticks(index, self.delivery.fps)
        self.records.append(FrameRecord(index, pts, digest))
        if self._stream is not None:
            frame = av.VideoFrame.from_ndarray(rgb, format="rgb24")
            frame.pts = pts
            frame.time_base = Fraction(1, MASTER_RATE)
            for packet in self._stream.encode(frame):
                self._container.mux(packet)

    def close(self) -> None:
        if self._stream is not None:
            for packet in self._stream.encode():
                self._container.mux(packet)
            self._stream = None
        if self._container is not None:
            self._container.close()
            self._container = None

    def actual_bitrate(self) -> int:
        """Overall bitrate of the finished file, bits per second."""
        size = self.out_path.stat().st_size
        seconds = Fraction(self.delivery.duration_ticks, MASTER_RATE)
        if seconds <= 0:
            return 0
        return int(size * 8 / float(seconds))


def draw_frame(window: np.ndarray, width: int, height: int, index: int) -> np.ndarray:
    """Deterministic level-bar visualization of one frame's audio window."""
    img = np.zeros((height, width, 3), np.uint8)
    img[:, :, 0] = (index * 7) % 32  # faint deterministic background
    img[:, :, 1] = 24
    img[:, :, 2] = 40
    if window.size:
        rms = np.sqrt(np.mean(window.astype(np.float64) ** 2, axis=0))
    else:
        rms = np.zeros(window.shape[1] if window.ndim == 2 else 1)
    n_ch = max(1, len(rms))
    band = height // n_ch
    for ch in range(n_ch):
        level = min(1.0, float(rms[ch]) * 4.0)
        bar_w = int(level * (width - 8))
        y0 = ch * band + 2
        y1 = min(height, y0 + band - 4)
        if bar_w > 0 and y1 > y0:
            img[y0:y1, 4 : 4 + bar_w, 0] = 80
            img[y0:y1, 4 : 4 + bar_w, 1] = 200
            img[y0:y1, 4 : 4 + bar_w, 2] = 90
    return img


def verify_video(path: Path, expected_frames: int, fps: int) -> tuple[int, bool]:
    """Re-open the encoded file; check frame count and monotonic timestamps."""
    try:
        container = av.open(str(path))
    except av.error.FFmpegError as e:
        raise StageflowError(
            "VERIFY_VIDEO_OPEN", f"cannot re-open rendered video: {e}", stage="render",
            path=path.name,
        )
    count = 0
    monotonic = True
    prev_pts: int | None = None
    with container:
        for frame in container.decode(video=0):
            count += 1
            if prev_pts is not None and frame.pts is not None and frame.pts <= prev_pts:
                monotonic = False
            if frame.pts is not None:
                prev_pts = frame.pts
    return count, monotonic and count == expected_frames
