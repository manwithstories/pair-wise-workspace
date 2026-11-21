"""Silence detection with NumPy, mapped onto the corrected segment timeline.

Audio arrives as decoded float32 frames on the shared timeline.  A sliding
window (configurable threshold and minimum duration) classifies each window
as silent when its peak amplitude is at or below the linear threshold.
Contiguous silent windows are merged into runs and reported with start/end
times, peak and RMS over the whole run.

Edge cases handled explicitly:
* Runs shorter than ``min_duration_s`` are rejected as non-silent.
* A partial final window (fewer samples than the window length) is still
  evaluated rather than dropped, so tail silence is not missed.
* RMS and peak are computed over the exact sample span of each run.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np


@dataclass
class SilenceRegion:
    """One detected silent interval on the corrected timeline."""

    start_s: float
    end_s: float
    peak: float
    rms: float
    sample_count: int

    def to_dict(self) -> dict:
        return {
            "start_s": round(self.start_s, 6),
            "end_s": round(self.end_s, 6),
            "duration_s": round(self.end_s - self.start_s, 6),
            "peak": round(float(self.peak), 8),
            "rms": round(float(self.rms), 8),
            "sample_count": int(self.sample_count),
        }


class SilenceDetector:
    """Streaming, windowed silence detector for one audio track."""

    def __init__(
        self,
        sample_rate: int,
        *,
        threshold: float = 0.01,
        min_duration_s: float = 0.1,
        window_s: float = 0.02,
    ) -> None:
        if not 0 <= threshold <= 1:
            raise ValueError("threshold must be within [0, 1]")
        if min_duration_s <= 0 or window_s <= 0:
            raise ValueError("durations must be positive")
        self.sample_rate = int(sample_rate)
        self.threshold = float(threshold)
        self.min_duration_s = float(min_duration_s)
        self.window = max(1, int(round(window_s * self.sample_rate)))
        # Accumulate a continuous sample buffer with its corrected start time.
        self._buffer = np.zeros((0,), dtype=np.float32)
        self._buffer_start_s = 0.0
        self._cursor_s = 0.0
        self._runs: list[SilenceRegion] = []
        self._open_run: list[tuple[float, float, np.ndarray]] | None = None

    def add_frame(self, corrected_s: float, samples: np.ndarray) -> None:
        """Append one decoded audio frame positioned at ``corrected_s``."""
        mono = samples
        if mono.ndim > 1:
            mono = mono.mean(axis=1)
        mono = np.asarray(mono, dtype=np.float32)
        if self._buffer.size == 0:
            self._buffer_start_s = corrected_s
            self._cursor_s = corrected_s
        # If frames are contiguous in time, just append; otherwise reset the
        # window boundary so spans never bridge a real timeline gap.
        expected = self._buffer_start_s + self._buffer.size / self.sample_rate
        if abs(corrected_s - expected) > 1.5 / self.sample_rate:
            self._flush_buffer()
            self._buffer_start_s = corrected_s
            self._cursor_s = corrected_s
        self._buffer = np.concatenate([self._buffer, mono])
        self._consume_windows(partial=False)

    def finalize(self) -> list[SilenceRegion]:
        """Evaluate any remaining partial window and close open runs."""
        self._consume_windows(partial=True)
        self._close_run()
        return list(self._runs)

    def _consume_windows(self, *, partial: bool) -> None:
        while self._buffer.size >= self.window or (partial and self._buffer.size > 0):
            if not partial and self._buffer.size < self.window:
                break
            n = min(self.window, self._buffer.size)
            chunk = self._buffer[:n]
            start_s = self._cursor_s
            end_s = start_s + n / self.sample_rate
            is_silent = float(np.max(np.abs(chunk))) <= self.threshold
            if is_silent:
                if self._open_run is None:
                    self._open_run = [(start_s, end_s, chunk.copy())]
                else:
                    self._open_run.append((start_s, end_s, chunk.copy()))
            else:
                self._close_run()
            self._buffer = self._buffer[n:]
            self._buffer_start_s = end_s
            self._cursor_s = end_s
            if partial and n < self.window:
                break

    def _close_run(self) -> None:
        if not self._open_run:
            return
        start_s = self._open_run[0][0]
        end_s = self._open_run[-1][1]
        audio = np.concatenate([part[2] for part in self._open_run])
        duration = end_s - start_s
        if duration >= self.min_duration_s and audio.size > 0:
            peak = float(np.max(np.abs(audio)))
            rms = float(np.sqrt(np.mean(np.square(audio, dtype=np.float64))))
            self._runs.append(
                SilenceRegion(
                    start_s=start_s,
                    end_s=end_s,
                    peak=peak,
                    rms=rms,
                    sample_count=audio.size,
                )
            )
        self._open_run = None


def map_silence_to_segment(region: SilenceRegion, segment_start_s: float) -> dict:
    """Project a silence region into segment-relative coordinates."""
    payload = region.to_dict()
    payload["segment_relative_start_s"] = round(region.start_s - segment_start_s, 6)
    payload["segment_relative_end_s"] = round(region.end_s - segment_start_s, 6)
    return payload
