"""Streaming 4x true-peak measurement and clipping counters."""

from __future__ import annotations

import numpy as np

from .loudness import amplitude_to_db


OVERSAMPLE = 4
TP_HALF_SPAN = 16
CLIP_THRESHOLD = 0.999
PLATEAU_THRESHOLD = 0.990
PLATEAU_RUN = 3


def _interpolation_table() -> np.ndarray:
    offsets = np.arange(-TP_HALF_SPAN, TP_HALF_SPAN + 1, dtype=np.float64)
    window = np.kaiser(len(offsets), 8.6)
    table = np.empty((OVERSAMPLE, len(offsets)), dtype=np.float64)
    for phase in range(OVERSAMPLE):
        values = np.sinc(offsets + phase / OVERSAMPLE) * window
        total = float(values.sum())
        if abs(total) < 1e-15:
            raise RuntimeError("degenerate true-peak interpolation kernel")
        table[phase] = values / total
    return table


_TP_TABLE = _interpolation_table()


class TruePeakMeter:
    def __init__(self, channels: int = 2) -> None:
        self.channels = int(channels)
        self._buffer = np.empty((0, self.channels), dtype=np.float64)
        self._base = 0
        self._received = 0
        self._next_out = 0
        self._max_abs = 0.0
        self._result: float | None = None

    def process(self, block: np.ndarray) -> None:
        data = np.asarray(block, dtype=np.float64)
        if data.ndim == 1:
            data = data[:, None]
        if data.shape[1] != self.channels:
            raise ValueError("channel count changed during true-peak measurement")
        if not len(data):
            return
        self._max_abs = max(self._max_abs, float(np.max(np.abs(data))))
        self._buffer = np.concatenate((self._buffer, data), axis=0)
        self._received += len(data)
        max_q = self._received - 1 - TP_HALF_SPAN
        if max_q >= 0:
            self._emit(OVERSAMPLE * (max_q + 1), final=False)

    def _emit(self, end: int, final: bool) -> None:
        if end <= self._next_out:
            return
        offsets = np.arange(-TP_HALF_SPAN, TP_HALF_SPAN + 1, dtype=np.int64)
        chunk = max(1, min(4096, 100_000 // (len(offsets) * self.channels)))
        for start in range(self._next_out, end, chunk):
            stop = min(end, start + chunk)
            high = np.arange(start, stop, dtype=np.int64)
            q = high // OVERSAMPLE
            phase = high % OVERSAMPLE
            indices = q[:, None] - offsets[None, :]
            relative = indices - self._base
            valid = (indices >= 0) & (indices < self._received)
            clipped = np.clip(relative, 0, max(0, len(self._buffer) - 1))
            values = self._buffer[clipped]
            if not valid.all():
                values[~valid] = 0.0
            values *= _TP_TABLE[phase][:, :, None]
            peak = float(np.max(np.abs(np.sum(values, axis=1))))
            self._max_abs = max(self._max_abs, peak)
        self._next_out = end
        next_q = self._next_out // OVERSAMPLE
        keep_from = max(0, next_q - TP_HALF_SPAN)
        cut = min(max(0, keep_from - self._base), len(self._buffer))
        if cut:
            self._buffer = self._buffer[cut:].copy()
            self._base += cut

    def finalize(self) -> float:
        if self._result is None:
            self._emit(OVERSAMPLE * self._received, final=True)
            self._result = amplitude_to_db(self._max_abs)
        return self._result


def plateau_mask(samples: np.ndarray) -> np.ndarray:
    data = np.asarray(samples, dtype=np.float64)
    if data.ndim == 1:
        data = data[:, None]
    mask = np.zeros_like(data, dtype=bool)
    equality_tolerance = 1e-12
    for channel in range(data.shape[1]):
        values = data[:, channel]
        eligible = np.abs(values) >= PLATEAU_THRESHOLD
        equal = np.zeros(len(values), dtype=bool)
        if len(values) > 1:
            equal[1:] = (
                (np.abs(np.diff(values)) <= equality_tolerance)
                & eligible[1:]
                & eligible[:-1]
            )
        starts = np.concatenate(([0], np.flatnonzero(~equal[1:]) + 1))
        ends = np.concatenate((starts[1:], [len(values)]))
        for first, last in zip(starts, ends):
            if last - first >= PLATEAU_RUN and eligible[first:last].all():
                mask[first:last, channel] = True
    return mask


class ClipCounter:
    """Count threshold clips plus non-threshold samples on flat runs."""

    def __init__(self) -> None:
        self._tail = np.empty((0, 0), dtype=np.float64)
        self._samples = 0
        self._resolved = 0
        self._threshold_count = 0
        self._plateau_extra = 0
        self._finalized = False

    def process(self, block: np.ndarray) -> None:
        if self._finalized:
            raise RuntimeError("clip counter already finalized")
        data = np.asarray(block, dtype=np.float64)
        if data.ndim == 1:
            data = data[:, None]
        if not self._tail.size:
            self._tail = np.empty((0, data.shape[1]), dtype=np.float64)
        self._threshold_count += int(np.count_nonzero(np.abs(data) >= CLIP_THRESHOLD))
        combined = np.concatenate((self._tail, data), axis=0)
        base = self._samples - len(self._tail)
        total = self._samples + len(data)
        resolve_to = max(0, total - 2)
        if resolve_to > self._resolved:
            first = self._resolved - base
            last = resolve_to - base
            mask = plateau_mask(combined)
            threshold = np.abs(combined[first:last]) >= CLIP_THRESHOLD
            self._plateau_extra += int(np.count_nonzero(mask[first:last] & ~threshold))
            self._resolved = resolve_to
        self._samples = total
        self._tail = combined[-5:].copy()

    def finalize(self) -> int:
        if not self._finalized:
            if len(self._tail):
                sentinel = np.full((1, self._tail.shape[1]), np.nan)
                combined = np.concatenate((self._tail, sentinel), axis=0)
                mask = plateau_mask(combined)
                base = self._samples - len(self._tail)
                first = self._resolved - base
                last = self._samples - base
                if last > first:
                    threshold = np.abs(combined[first:last]) >= CLIP_THRESHOLD
                    self._plateau_extra += int(
                        np.count_nonzero(mask[first:last] & ~threshold)
                    )
                self._resolved = self._samples
            self._finalized = True
        return self._threshold_count + self._plateau_extra

    @property
    def threshold_count(self) -> int:
        return self._threshold_count

    @property
    def plateau_extra(self) -> int:
        return self._plateau_extra


def count_clipping(samples: np.ndarray, block_size: int = 4096) -> int:
    data = np.asarray(samples, dtype=np.float64)
    counter = ClipCounter()
    for start in range(0, len(data), block_size):
        counter.process(data[start : start + block_size])
    return counter.finalize()


def true_peak(samples: np.ndarray, block_size: int = 4096) -> float:
    data = np.asarray(samples, dtype=np.float64)
    channels = 1 if data.ndim == 1 else data.shape[1]
    meter = TruePeakMeter(channels)
    for start in range(0, len(data), block_size):
        meter.process(data[start : start + block_size])
    return meter.finalize()
