"""Audio processing facade: matrix -> resample -> measure/gain.

The two concrete pieces live in :mod:`archive_media.audio_layout` and
:mod:`archive_media.audio_loudness`; this module exposes the single
:class:`AudioProcessor` the pipeline drives chunk by chunk plus a convenience
whole-array path used by the tests.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .audio_layout import (
    CANONICAL_LAYOUTS,
    ChannelMatrix,
    StreamResampler,
    canonical_layout,
    channel_matrix,
)
from .audio_loudness import (
    ChunkGainProcessor,
    LoudnessMeter,
    compute_gain_db,
    expected_samples,
)
from .errors import Code, RecoveryError


@dataclass
class AudioConfig:
    target_rate: int
    target_layout: str
    target_lufs: float = -18.0
    peak_ceiling_dbfs: float = -1.0

    def __post_init__(self) -> None:
        if self.target_rate <= 0:
            raise RecoveryError(
                Code.RESAMPLE_FAILURE, "target sample rate must be positive",
                detail={"target_rate": self.target_rate},
            )
        if canonical_layout(self.target_layout) is None:
            raise RecoveryError(
                Code.UNSUPPORTED_LAYOUT,
                f"unsupported target layout {self.target_layout!r}",
            )


class AudioProcessor:
    """Stateful per-segment processor.

    One instance handles one segment so its resampler filter state and gain
    boundary accounting are scoped exactly to that segment's output file.
    """

    def __init__(
        self,
        source_rate: int,
        source_layout: str,
        config: AudioConfig,
        *,
        source_track: str = "",
    ) -> None:
        self.config = config
        self.matrix: ChannelMatrix = channel_matrix(
            source_layout, config.target_layout, source_track=source_track
        )
        self.resampler = StreamResampler(
            source_rate, config.target_rate, config.target_layout
        )
        target_channels = len(CANONICAL_LAYOUTS[self.matrix.target])
        self.meter = LoudnessMeter(config.target_rate, target_channels)
        self._gain_db: float | None = None
        self._gain: ChunkGainProcessor | None = None
        self._source_rate = source_rate

    def prepare_gain(
        self, input_lufs: float | None, input_peak_dbfs: float | None,
        expected_output_samples: int,
    ) -> tuple[float, bool]:
        gain_db, limited = compute_gain_db(
            input_lufs, input_peak_dbfs,
            target_lufs=self.config.target_lufs,
            peak_ceiling_dbfs=self.config.peak_ceiling_dbfs,
        )
        self._gain_db = gain_db
        self._gain = ChunkGainProcessor(
            gain_db, self.config.peak_ceiling_dbfs, expected_output_samples
        )
        return gain_db, limited

    def normalize(self, block: np.ndarray) -> np.ndarray:
        """Matrix mix then resample one decoded block."""
        if block.size == 0:
            channels = self.matrix.weights.shape[0]
            return np.zeros((0, channels), dtype=np.float32)
        mixed = self.matrix.apply(block.astype(np.float32, copy=False))
        return self.resampler.process(mixed)

    def measure(self, normalized: np.ndarray) -> None:
        self.meter.push(normalized)

    def apply_gain(self, normalized: np.ndarray) -> np.ndarray:
        if self._gain is None:
            raise RecoveryError(
                Code.RESAMPLE_FAILURE,
                "apply_gain called before prepare_gain; measure pass incomplete",
            )
        return self._gain.process(normalized).samples

    def flush(self) -> np.ndarray:
        tail = self.resampler.flush()
        if tail.size and self._gain is not None:
            tail = self._gain.process(tail).samples
        return tail

    @property
    def gain_results(self):
        return self._gain.results if self._gain else []


def process_array(
    samples: np.ndarray,
    source_rate: int,
    source_layout: str,
    config: AudioConfig,
    *,
    duration_seconds: object | None = None,
    source_track: str = "",
) -> tuple[np.ndarray, dict[str, object]]:
    """Whole-array convenience path with identical per-chunk semantics."""
    processor = AudioProcessor(
        source_rate, source_layout, config, source_track=source_track
    )
    normalized = processor.normalize(samples)
    processor.measure(normalized)
    tail = processor.resampler.flush()
    if tail.size:
        processor.measure(tail)
        normalized = np.concatenate([normalized, tail], axis=0)
    lufs, peak = processor.meter.result()
    if duration_seconds is None:
        expected = normalized.shape[0]
    else:
        expected = expected_samples(duration_seconds, config.target_rate)
    gain_db, limited = processor.prepare_gain(lufs, peak, expected)
    out = processor.apply_gain(normalized)
    boundary = out.shape[0] - expected
    return out, {
        "source_rate": source_rate,
        "target_rate": config.target_rate,
        "source_layout": processor.matrix.source,
        "target_layout": processor.matrix.target,
        "input_lufs": lufs,
        "input_peak_dbfs": peak,
        "gain_db": gain_db,
        "peak_limited": limited,
        "expected_samples": expected,
        "written_samples": int(out.shape[0]),
        "boundary_error_samples": boundary,
        "nonfinite_samples": processor.meter.nonfinite_samples,
    }


__all__ = [
    "AudioConfig",
    "AudioProcessor",
    "process_array",
]
