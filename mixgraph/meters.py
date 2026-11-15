"""Incremental peak, RMS, and clipping statistics."""

from __future__ import annotations

import math

import numpy as np

from .errors import ManifestError
from .util import linear_to_db


class Meter:
    """Accumulate statistics without retaining audio samples."""

    def __init__(self, *, ceiling_db: float = 0.0, field: str = "meter") -> None:
        if not math.isfinite(ceiling_db):
            raise ManifestError("ceiling must be finite", f"{field}.ceiling_db")
        self.ceiling = 10.0 ** (ceiling_db / 20.0)
        self.peak = 0.0
        self.sum_squares = 0.0
        self.sample_count = 0
        self.clip_samples = 0
        self.clip_segments = 0
        self.first_clip_sample: int | None = None
        self._in_clip = False
        self._frames_seen = 0

    @property
    def frames_seen(self) -> int:
        return self._frames_seen

    def update(self, block: np.ndarray, absolute_start: int | None = None) -> None:
        values = np.asarray(block, dtype=np.float64)
        if values.size == 0:
            return
        if values.ndim == 1:
            values = values[:, None]
        if not np.all(np.isfinite(values)):
            raise ManifestError("audio contains NaN or infinity", "audio.samples")
        start = self._frames_seen if absolute_start is None else int(absolute_start)
        self.peak = max(self.peak, float(np.max(np.abs(values))))
        self.sum_squares += float(np.sum(values * values, dtype=np.float64))
        self.sample_count += int(values.size)

        over = np.abs(values) > self.ceiling
        clipped = int(np.count_nonzero(over))
        self.clip_samples += clipped
        frames_over = np.any(over, axis=1)
        if frames_over.any() and self.first_clip_sample is None:
            self.first_clip_sample = start + int(np.argmax(frames_over))
        if frames_over.size:
            starts = int(frames_over[0] and not self._in_clip)
            starts += int(np.count_nonzero(frames_over[1:] & ~frames_over[:-1]))
            self.clip_segments += starts
            self._in_clip = bool(frames_over[-1])
        self._frames_seen = start + values.shape[0]

    def report(self) -> dict[str, float | int | None]:
        peak_dbfs = linear_to_db(self.peak)
        if self.sample_count:
            rms = math.sqrt(self.sum_squares / self.sample_count)
        else:
            rms = 0.0
        return {
            "peak_dbfs": round(peak_dbfs, 6),
            "rms_dbfs": round(linear_to_db(rms), 6),
            "clip_samples": self.clip_samples,
            "clip_segments": self.clip_segments,
            "first_clip_sample": self.first_clip_sample,
        }


def meter_wav(path: str, *, block_size: int = 4096, ceiling_db: float = 0.0) -> Meter:
    import soundfile as sf

    meter = Meter(ceiling_db=ceiling_db, field=str(path))
    with sf.SoundFile(path, "r") as handle:
        while True:
            block = handle.read(block_size, dtype="float64", always_2d=True)
            if block.shape[0] == 0:
                break
            meter.update(block)
    return meter

