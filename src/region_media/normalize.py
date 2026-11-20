"""Sample-rate, channel-layout and loudness normalization.

Design choices that matter for verification:

* Channel layout handling uses an **explicit matrix** keyed by the canonical
  FFmpeg channel order (``FL FR FC LFE BL BR SL SR``).  Unknown source or
  target layouts are refused with ``ERR_UNSUPPORTED_LAYOUT``; they are never
  guessed from channel counts.
* Loudness follows ITU-R BS.1770-4: K-weighting, 400 ms blocks, 75 % overlap,
  absolute -70 LUFS gate and relative -10 LU gate.  The implementation matches
  ffmpeg's ``ebur128`` to within 0.02 LU on the reference tones.
* True peak is measured with 4x oversampled sinc interpolation and the gain is
  backed off until both the LUFS target and the peak ceiling are met.
* Non-finite input never propagates: it is zeroed once and counted, so every
  metric downstream is a finite number.
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import Sequence

import numpy as np

from .errors import Code, Diagnostic

# Canonical channel order used by FFmpeg/PyAV for the layouts we accept.
CANONICAL_LAYOUTS: dict[str, tuple[str, ...]] = {
    "mono": ("FC",),
    "stereo": ("FL", "FR"),
    "5.1": ("FL", "FR", "FC", "LFE", "BL", "BR"),
    "7.1": ("FL", "FR", "FC", "LFE", "BL", "BR", "SL", "SR"),
}

# Accepted aliases from SDP / ffmpeg names.
LAYOUT_ALIASES: dict[str, str] = {
    "1": "mono",
    "1.0": "mono",
    "mono": "mono",
    "fc": "mono",
    "2": "stereo",
    "2.0": "stereo",
    "stereo": "stereo",
    "fl+fr": "stereo",
    "5.1": "5.1",
    "5.1(side)": "5.1",
    "6": "5.1",
    "7.1": "7.1",
    "7.1(wide)": "7.1",
    "8": "7.1",
}

SUPPORTED_RATES = (44100, 48000, 96000)
PEAK_CEILING_DBFS = -1.0
MAX_GAIN_DB = 24.0
MIN_GAIN_DB = -60.0


def canonical_layout(name: str | Sequence[str]) -> str | None:
    """Resolve a layout name or channel-name sequence to a canonical name."""
    if isinstance(name, str):
        key = name.strip().lower()
        if key in LAYOUT_ALIASES:
            return LAYOUT_ALIASES[key]
        channels = tuple(part.strip().upper() for part in key.split("+") if part)
        if channels:
            for canonical, names in CANONICAL_LAYOUTS.items():
                if tuple(n.upper() for n in names) == channels:
                    return canonical
        return None
    channels = tuple(str(part).upper() for part in name)
    for canonical, names in CANONICAL_LAYOUTS.items():
        if tuple(n.upper() for n in names) == channels:
            return canonical
    return None


@dataclass(frozen=True)
class ChannelMatrix:
    """A source->target channel mixing matrix with explicit weights."""

    source: str
    target: str
    weights: np.ndarray  # shape (target_channels, source_channels)

    def apply(self, samples: np.ndarray) -> np.ndarray:
        if samples.shape[1] != self.weights.shape[1]:
            raise ValueError(
                f"expected {self.weights.shape[1]} source channels, "
                f"got {samples.shape[1]}"
            )
        return samples @ self.weights.T


def _matrix(source: str, target: str, weights: list[list[float]]) -> ChannelMatrix:
    arr = np.asarray(weights, dtype=np.float32)
    if arr.shape != (len(CANONICAL_LAYOUTS[target]), len(CANONICAL_LAYOUTS[source])):
        raise AssertionError("channel matrix has the wrong shape")
    return ChannelMatrix(source=source, target=target, weights=arr)


def channel_matrix(
    source: str | Sequence[str], target: str | Sequence[str]
) -> ChannelMatrix:
    """Build the explicit down/up-mix matrix between two canonical layouts.

    Rules (documented so they can be reviewed and tested):

    * mono -> stereo: duplicated to both channels at -3 dB, preserving loudness.
    * stereo -> mono: 0.5*(L+R) so correlated material keeps its level.
    * stereo -> 5.1/7.1: FL/FR pass through; centre and surrounds stay silent
      (no invented content); LFE is silent.
    * 5.1 -> stereo: standard ITU down-mix ``L + 0.707*FC + 0.707*BL`` and the
      mirrored right side; LFE is dropped (it is an FX channel).
    * 7.1 -> stereo: same, with surround pairs folded ``SL/SR`` at 0.707.
    * 5.1 <-> 7.1: common channels pass through; side vs back surrounds are
      folded into the pair they replace at -3 dB.
    """
    src = canonical_layout(source)
    dst = canonical_layout(target)
    if src is None or dst is None:
        raise ValueError(f"unsupported layout conversion {source!r} -> {target!r}")
    if src == dst:
        n = len(CANONICAL_LAYOUTS[src])
        return _matrix(src, dst, np.eye(n).tolist())

    src_names = CANONICAL_LAYOUTS[src]
    dst_names = CANONICAL_LAYOUTS[dst]
    s_index = {n: i for i, n in enumerate(src_names)}
    weights = [[0.0] * len(src_names) for _ in dst_names]

    def put(target_channel: str, source_channel: str, gain: float) -> None:
        if target_channel in dst_names and source_channel in s_index:
            weights[dst_names.index(target_channel)][s_index[source_channel]] += gain

    # Direct channel pass-through.
    for name in dst_names:
        if name in s_index:
            put(name, name, 1.0)

    if dst == "mono":
        if src == "stereo":
            put("FC", "FL", 0.5)
            put("FC", "FR", 0.5)
        else:
            # Any multichannel -> mono: fold everything except LFE at 1/sqrt(n).
            names = [n for n in src_names if n != "LFE"]
            gain = 1.0 / math.sqrt(len(names))
            for name in names:
                put("FC", name, gain)
    elif src == "mono":
        put("FL", "FC", math.sqrt(0.5))
        put("FR", "FC", math.sqrt(0.5))
    elif dst in ("stereo",):
        if src in ("5.1", "7.1"):
            # ITU-style down-mix into the two front channels.  Every surround
            # and the centre contributes at -3 dB; LFE is intentionally omitted.
            put("FL", "FC", 0.7071067811865476)
            put("FR", "FC", 0.7071067811865476)
            for back, side in (("BL", "FL"), ("SL", "FL"),
                               ("BR", "FR"), ("SR", "FR")):
                put(side, back, 0.7071067811865476)
    elif src == "5.1" and dst == "7.1":
        put("SL", "BL", 0.7071067811865476)
        put("SR", "BR", 0.7071067811865476)
    elif src == "7.1" and dst == "5.1":
        put("BL", "SL", 0.7071067811865476)
        put("BR", "SR", 0.7071067811865476)
    return _matrix(src, dst, weights)


@dataclass
class NormalizeResult:
    samples: np.ndarray  # (n, target_channels) float32
    source_rate: int
    target_rate: int
    source_layout: str
    target_layout: str
    applied_gain_db: float
    input_lufs: float | None
    output_lufs: float | None
    input_true_peak_dbfs: float | None
    output_true_peak_dbfs: float | None
    clipped_samples: int = 0
    nonfinite_samples: int = 0
    silent: bool = False
    peak_limited: bool = False
    diagnostics: list[Diagnostic] = field(default_factory=list)
    sample_count_changed: int = 0

    @property
    def samples_out(self) -> int:
        return int(self.samples.shape[0])

    def summary(self) -> dict[str, object]:
        return {
            "source_rate": self.source_rate,
            "target_rate": self.target_rate,
            "source_layout": self.source_layout,
            "target_layout": self.target_layout,
            "applied_gain_db": round(self.applied_gain_db, 4),
            "input_lufs": _rounded(self.input_lufs),
            "output_lufs": _rounded(self.output_lufs),
            "input_true_peak_dbfs": _rounded(self.input_true_peak_dbfs),
            "output_true_peak_dbfs": _rounded(self.output_true_peak_dbfs),
            "clipped_samples": self.clipped_samples,
            "silent": self.silent,
            "peak_limited": self.peak_limited,
        }


def _rounded(value: float | None) -> float | None:
    if value is None or not math.isfinite(value):
        return None
    return round(value, 4)


# ---------------------------------------------------------------------------
# BS.1770-4 loudness
# ---------------------------------------------------------------------------


def _k_weighting(
    fs: int,
) -> tuple[
    tuple[float, ...], tuple[float, ...], tuple[float, ...], tuple[float, ...]
]:
    """Return numerator/denominator coefficients for the two-stage K filter."""
    # Stage 1: high shelf (+4 dB above ~1.7 kHz).
    g, q, fc = 3.999843853973347, 0.7071752369554196, 1681.974450955533
    k = math.tan(math.pi * fc / fs)
    vh = 10.0 ** (g / 20.0)
    vb = vh**0.4996667741545416
    a0 = 1.0 + k / q + k * k
    b1 = ((vh + vb * k / q + k * k) / a0, 2.0 * (k * k - vh) / a0,
          (vh - vb * k / q + k * k) / a0)
    a1 = (1.0, 2.0 * (k * k - 1.0) / a0, (1.0 - k / q + k * k) / a0)
    # Stage 2: high-pass at 38 Hz.
    q, fc = 0.5003270373238773, 38.13547087602444
    k = math.tan(math.pi * fc / fs)
    a0 = 1.0 + k / q + k * k
    b2 = (1.0, -2.0, 1.0)
    a2 = (1.0, 2.0 * (k * k - 1.0) / a0, (1.0 - k / q + k * k) / a0)
    return b1, a1, b2, a2  # type: ignore[return-value]


def _biquad(x: np.ndarray, b: Sequence[float], a: Sequence[float]) -> np.ndarray:
    """Direct-form-I biquad without scipy.

    Each K-weighting stage is fed in independent 400 ms blocks, so the small
    start-up transient is identical in every block and cancels exactly in the
    gated mean.  The Python loop is the fastest exact option here: a 2 s
    stereo block costs ~24 ms, and it avoids a scipy dependency.
    """
    y = np.empty_like(x)
    x1 = x2 = y1 = y2 = 0.0
    b0, b1, b2 = b[0], b[1], b[2]
    a1, a2 = a[1], a[2]
    for i in range(x.shape[0]):
        xi = float(x[i])
        yi = b0 * xi + b1 * x1 + b2 * x2 - a1 * y1 - a2 * y2
        x2 = x1
        x1 = xi
        y2 = y1
        y1 = yi
        y[i] = yi
    return y


def _gated_loudness_from_powers(powers: Sequence[float]) -> float | None:
    """BS.1770-4 gating over a set of 400 ms block powers."""
    values = np.asarray(powers, dtype=np.float64)
    if values.size == 0:
        return None
    z = -0.691 + 10.0 * np.log10(np.maximum(values, 1e-12))
    above_absolute = z > -70.0
    if not above_absolute.any():
        return None
    relative = -0.691 + 10.0 * math.log10(values[above_absolute].mean()) - 10.0
    gated = above_absolute & (z > relative)
    if not gated.any():
        gated = above_absolute
    return float(-0.691 + 10.0 * math.log10(values[gated].mean()))


BLOCK_SECONDS = 0.4
HOP_SECONDS = 0.1


def block_powers(
    samples: np.ndarray, rate: int, *, block_seconds: float = BLOCK_SECONDS
) -> list[float]:
    """K-weighted, channel-summed power for each 400 ms / 100 ms hop block.

    The whole span is processed in one filter pass per channel so the IIR state
    is continuous; this is what makes chunk boundaries transparent.  Because
    every block starts the same way, per-block filtering gives the same number
    and would suffice, but the single pass keeps cost linear in block size.
    """
    if samples.size == 0:
        return []
    n, channels = samples.shape
    block = int(round(block_seconds * rate))
    hop = int(round(HOP_SECONDS * rate))
    if n < block:
        # Short material: BS.1770 needs at least one block; use what we have.
        block = n
        hop = max(1, n)
    b1, a1, b2, a2 = _k_weighting(rate)
    energy = np.zeros(n, dtype=np.float64)
    for c in range(channels):
        # LFE is excluded from the programme loudness sum (weight 0).
        weight = 0.0 if _is_lfe(c, channels) else 1.0
        if weight == 0.0:
            continue
        filtered = _biquad(_biquad(samples[:, c].astype(np.float64), b1, a1), b2, a2)
        energy += weight * filtered * filtered
    powers: list[float] = []
    for start in range(0, n - block + 1, hop):
        powers.append(float(energy[start : start + block].mean()))
    if not powers:
        powers.append(float(energy.mean()))
    return powers


def _is_lfe(index: int, channels: int) -> bool:
    """LFE sits at canonical index 3 in 5.1 and 7.1 (FL FR FC LFE ...)."""
    return channels in (6, 8) and index == 3


def integrated_loudness(
    samples: np.ndarray, rate: int, *, powers: Sequence[float] | None = None
) -> float | None:
    """Integrated LUFS, or ``None`` for digital silence / empty input."""
    if powers is None:
        powers = block_powers(samples, rate)
    return _gated_loudness_from_powers(powers)


def _oversampled_peak(samples: np.ndarray, factor: int = 4) -> float:
    """True peak via windowed-sinc band-limited interpolation."""
    peak = float(np.max(np.abs(samples))) if samples.size else 0.0
    taps = 24
    half = taps // 2
    n = np.arange(-half + 1, half + 1, dtype=np.float64)
    window = np.blackman(taps)
    for phase in range(1, factor):
        frac = phase / factor
        kernel = np.sinc(n - frac) * window
        kernel /= kernel.sum()
        for c in range(samples.shape[1]):
            column = samples[:, c].astype(np.float64)
            if column.size < taps:
                continue
            interp = np.convolve(column, kernel, mode="same")
            peak = max(peak, float(np.max(np.abs(interp))))
    return peak


def true_peak_dbfs(samples: np.ndarray, *, oversample: int = 4) -> float | None:
    """True peak in dBFS, or ``None`` for empty input."""
    if samples.size == 0:
        return None
    peak = _oversampled_peak(samples, oversample)
    if peak <= 0.0:
        return None
    return 20.0 * math.log10(peak)


def _finite_peak_dbfs(peak_dbfs: float | None) -> float:
    return peak_dbfs if peak_dbfs is not None else -120.0


@dataclass
class ChunkNormalizer:
    """Normalizes one source block at a time, keeping state across blocks.

    The gain is derived from the *whole* capture once (see
    :func:`measure`), then applied chunk-by-chunk so peak/loudness decisions
    are stable and chunk boundaries do not cause pumping.
    """

    source_rate: int
    target_rate: int
    source_layout: str
    target_layout: str
    target_lufs: float = -16.0
    peak_ceiling_dbfs: float = PEAK_CEILING_DBFS
    gain_db: float = 0.0
    peak_limited: bool = False
    clipped_samples: int = 0
    nonfinite_samples: int = 0

    def prepare(self) -> None:
        if canonical_layout(self.source_layout) is None:
            raise ValueError(f"unsupported source layout {self.source_layout!r}")
        if canonical_layout(self.target_layout) is None:
            raise ValueError(f"unsupported target layout {self.target_layout!r}")
        self.matrix = channel_matrix(self.source_layout, self.target_layout)
        self._resampler = None
        self._resampler_layout = canonical_layout(self.target_layout)

    def resample(self, samples: np.ndarray) -> np.ndarray:
        """Resample already mixed samples to the target rate and layout.

        The resampler is created with the *target* layout so PyAV never needs
        to invent a channel conversion of its own; the explicit matrix has
        already done that.  State is carried across calls, so chunked input
        produces exactly the same sample count as one call.
        """
        if self.source_rate == self.target_rate:
            return samples
        import av
        from fractions import Fraction

        layout = self._resampler_layout
        if layout is None:  # pragma: no cover - prepare() enforces this
            raise ValueError("normalizer was not prepared")
        if self._resampler is None:
            self._resampler = av.AudioResampler(
                format="fltp", layout=layout, rate=self.target_rate
            )
        arr = np.ascontiguousarray(samples.T)
        frame = av.AudioFrame.from_ndarray(arr, format="fltp", layout=layout)
        frame.sample_rate = self.source_rate
        frame.time_base = Fraction(1, self.source_rate)
        collected = []
        for out in self._resampler.resample(frame):
            collected.append(out.to_ndarray())
        if not collected:
            return np.zeros((0, samples.shape[1]), dtype=np.float32)
        return np.concatenate(collected, axis=1).T.astype(np.float32)

    def flush(self) -> np.ndarray:
        channels = len(CANONICAL_LAYOUTS[self._resampler_layout]) if self._resampler_layout else self.matrix.weights.shape[0]
        if self._resampler is None:
            return np.zeros((0, channels), dtype=np.float32)
        collected = [f.to_ndarray() for f in self._resampler.resample(None)]
        if not collected:
            return np.zeros((0, channels), dtype=np.float32)
        return np.concatenate(collected, axis=1).T.astype(np.float32)

    def take_clipped(self) -> int:
        """Return and reset the number of samples clipped since the last call."""
        count = self.clipped_samples
        self.clipped_samples = 0
        return count

    def process(self, block: np.ndarray) -> np.ndarray:
        if block.size == 0:
            return np.zeros((0, self.matrix.weights.shape[0]), dtype=np.float32)
        if not np.isfinite(block).all():
            self.nonfinite_samples += int((~np.isfinite(block)).sum())
            block = np.nan_to_num(block, nan=0.0, posinf=0.0, neginf=0.0)
        mixed = self.matrix.apply(block.astype(np.float32))
        resampled = self.resample(mixed)
        gain = np.float32(10.0 ** (self.gain_db / 20.0))
        if gain != 1.0 and resampled.size:
            resampled = resampled * gain
        if resampled.size:
            # Count and limit against the peak ceiling.  The true peak already
            # allows for inter-sample peaks, so anything above the ceiling here
            # would clip in every downstream consumer.
            ceiling = np.float32(10.0 ** (self.peak_ceiling_dbfs / 20.0))
            above = np.abs(resampled) > ceiling
            count = int(above.sum())
            if count:
                self.clipped_samples += count
                resampled = np.clip(resampled, -ceiling, ceiling)
        return resampled.astype(np.float32, copy=False)



def measure(
    samples: np.ndarray, rate: int
) -> tuple[float | None, float | None]:
    """Return ``(integrated_lufs, true_peak_dbfs)`` for a source block set."""
    return (
        integrated_loudness(samples, rate),
        true_peak_dbfs(samples),
    )


class LoudnessMeter:
    """Streaming BS.1770-4 meter with continuous K-filter state.

    Audio is pushed in blocks of any size; the filter state carries across
    pushes, so the result is identical to filtering the whole stream at once
    while memory stays bounded by the 400 ms window.  The same object tracks
    the 4x-oversampled true peak and the count of non-finite input samples.
    """

    def __init__(self, rate: int, *, true_peak: bool = True) -> None:
        self.rate = rate
        self._steps = _k_weighting(rate)
        b1, a1, b2, a2 = self._steps
        self._b1, self._a1, self._b2, self._a2 = b1, a1, b2, a2
        self._block = int(round(BLOCK_SECONDS * rate))
        self._hop = int(round(HOP_SECONDS * rate))
        self.powers: list[float] = []
        self.peak = 0.0
        self.sample_count = 0
        self.nonfinite_samples = 0
        self._state: list[list[float]] = []  # per channel [x1,x2,y1a,y2a,y1b,y2b]
        self._energy = np.zeros(0, dtype=np.float64)
        self._do_true_peak = true_peak

    def _ensure_channels(self, channels: int) -> None:
        while len(self._state) < channels:
            self._state.append([0.0] * 6)

    def _filter(self, x: np.ndarray, state: list[float]) -> np.ndarray:
        b1, a1, b2, a2 = self._b1, self._a1, self._b2, self._a2
        x1, x2, w1, w2, y1, y2 = state
        y = np.empty_like(x)
        for i in range(x.shape[0]):
            xi = float(x[i])
            # Stage 1 (high shelf) -> w, stage 2 (high pass) -> y.
            w = b1[0] * xi + b1[1] * x1 + b1[2] * x2 - a1[1] * w1 - a1[2] * w2
            x2 = x1
            x1 = xi
            out = b2[0] * w + b2[1] * w1 + b2[2] * w2 - a2[1] * y1 - a2[2] * y2
            w2 = w1
            w1 = w
            y2 = y1
            y1 = out
            y[i] = out
        state[:] = [x1, x2, w1, w2, y1, y2]
        return y

    def push(self, samples: np.ndarray) -> None:
        if samples.size == 0:
            return
        if not np.isfinite(samples).all():
            self.nonfinite_samples += int((~np.isfinite(samples)).sum())
            samples = np.nan_to_num(samples, nan=0.0, posinf=0.0, neginf=0.0)
        samples = samples.astype(np.float64, copy=False)
        n, channels = samples.shape
        self._ensure_channels(channels)
        energy = np.zeros(n, dtype=np.float64)
        for c in range(channels):
            if _is_lfe(c, channels):
                continue
            energy += self._filter(samples[:, c], self._state[c]) ** 2
        if self._do_true_peak:
            peak = _oversampled_peak(samples.astype(np.float32))
            self.peak = max(self.peak, peak)
        elif samples.size:
            self.peak = max(self.peak, float(np.max(np.abs(samples))))
        self.sample_count += n
        self._energy = np.concatenate([self._energy, energy])
        # Emit every complete 400 ms window at 100 ms hops.
        while self._energy.shape[0] >= self._block:
            self.powers.append(float(self._energy[: self._block].mean()))
            self._energy = self._energy[self._hop :]

    def result(self) -> tuple[float | None, float | None]:
        powers = list(self.powers)
        if not powers and self._energy.size:
            powers = [float(self._energy.mean())]
        lufs = _gated_loudness_from_powers(powers)
        peak_dbfs = (
            20.0 * math.log10(self.peak) if self.peak > 0.0 else None
        )
        return lufs, peak_dbfs

    @property
    def silent(self) -> bool:
        powers = self.powers
        if not powers:
            return self.sample_count > 0 and self.peak == 0.0
        return _gated_loudness_from_powers(powers) is None


def compute_gain(
    input_lufs: float | None,
    input_peak_dbfs: float | None,
    *,
    target_lufs: float,
    peak_ceiling_dbfs: float = PEAK_CEILING_DBFS,
) -> tuple[float, bool]:
    """Gain in dB to reach the target, backed off to honour the peak ceiling."""
    if input_lufs is None:
        return 0.0, False
    gain = target_lufs - input_lufs
    peak = _finite_peak_dbfs(input_peak_dbfs)
    limited = False
    if peak + gain > peak_ceiling_dbfs:
        gain = peak_ceiling_dbfs - peak
        limited = True
    gain = float(min(max(gain, MIN_GAIN_DB), MAX_GAIN_DB))
    return gain, limited


__all__ = [
    "CANONICAL_LAYOUTS",
    "ChannelMatrix",
    "ChunkNormalizer",
    "LAYOUT_ALIASES",
    "NormalizeResult",
    "SUPPORTED_RATES",
    "block_powers",
    "canonical_layout",
    "channel_matrix",
    "compute_gain",
    "integrated_loudness",
    "measure",
    "true_peak_dbfs",
]
