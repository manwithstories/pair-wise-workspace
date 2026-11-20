"""Channel-layout mapping and sample-rate conversion.

Layouts are never inferred from channel counts: both sides must resolve to a
canonical FFmpeg layout, and the conversion is an explicit, reviewable
mixing matrix.  An unresolvable layout raises
:data:`~archive_media.errors.Code.UNSUPPORTED_LAYOUT` carrying the source
track description so the archive record keeps which track was rejected.
"""

from __future__ import annotations

import math
from dataclasses import dataclass
from fractions import Fraction
from typing import Sequence

import av
import numpy as np

from .errors import Code, RecoveryError

CANONICAL_LAYOUTS: dict[str, tuple[str, ...]] = {
    "mono": ("FC",),
    "stereo": ("FL", "FR"),
    "5.1": ("FL", "FR", "FC", "LFE", "BL", "BR"),
    "7.1": ("FL", "FR", "FC", "LFE", "BL", "BR", "SL", "SR"),
}

LAYOUT_ALIASES: dict[str, str] = {
    "1": "mono", "mono": "mono", "fc": "mono",
    "2": "stereo", "stereo": "stereo", "fl+fr": "stereo",
    "5.1": "5.1", "5.1(side)": "5.1", "6": "5.1",
    "7.1": "7.1", "7.1(wide)": "7.1", "8": "7.1",
}


def canonical_layout(name: str | Sequence[str]) -> str | None:
    """Resolve a layout name or channel-name tuple to a canonical name."""
    if isinstance(name, str):
        key = name.strip().lower()
        if key in LAYOUT_ALIASES:
            return LAYOUT_ALIASES[key]
        channels = tuple(p.strip().upper() for p in key.split("+") if p)
        if channels:
            for canonical, names in CANONICAL_LAYOUTS.items():
                if tuple(n.upper() for n in names) == channels:
                    return canonical
        return None
    channels = tuple(str(p).upper() for p in name)
    for canonical, names in CANONICAL_LAYOUTS.items():
        if tuple(n.upper() for n in names) == channels:
            return canonical
    return None


@dataclass(frozen=True)
class ChannelMatrix:
    source: str
    target: str
    weights: np.ndarray  # (target_channels, source_channels)
    source_track: str

    def apply(self, samples: np.ndarray) -> np.ndarray:
        if samples.ndim != 2 or samples.shape[1] != self.weights.shape[1]:
            raise RecoveryError(
                Code.LAYOUT_MISMATCH,
                f"expected {self.weights.shape[1]} source channels, got "
                f"{samples.shape[1] if samples.ndim == 2 else samples.ndim}",
                detail={"source_track": self.source_track},
            )
        return samples @ self.weights.T


def channel_matrix(
    source: str | Sequence[str],
    target: str | Sequence[str],
    *,
    source_track: str = "",
) -> ChannelMatrix:
    """Build the explicit down/up-mix matrix or raise with the source track."""
    src = canonical_layout(source)
    dst = canonical_layout(target)
    if src is None or dst is None:
        raise RecoveryError(
            Code.UNSUPPORTED_LAYOUT,
            f"unsupported layout conversion {source!r} -> {target!r}",
            detail={"source_track": source_track, "source": str(source),
                    "target": str(target)},
        )
    src_names = CANONICAL_LAYOUTS[src]
    dst_names = CANONICAL_LAYOUTS[dst]
    s_index = {n: i for i, n in enumerate(src_names)}
    weights = [[0.0] * len(src_names) for _ in dst_names]

    def put(dst_channel: str, src_channel: str, gain: float) -> None:
        if dst_channel in dst_names and src_channel in s_index:
            weights[dst_names.index(dst_channel)][s_index[src_channel]] += gain

    for name in dst_names:  # identity pass-through for shared channels
        if name in s_index:
            put(name, name, 1.0)
    if dst == "mono":
        if src == "stereo":
            put("FC", "FL", 0.5)
            put("FC", "FR", 0.5)
        else:
            names = [n for n in src_names if n != "LFE"]
            gain = 1.0 / math.sqrt(len(names))
            for name in names:
                put("FC", name, gain)
    elif src == "mono":  # duplicate at -3 dB to preserve loudness
        put("FL", "FC", math.sqrt(0.5))
        put("FR", "FC", math.sqrt(0.5))
    elif dst == "stereo" and src in ("5.1", "7.1"):
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
    return ChannelMatrix(src, dst, np.asarray(weights, dtype=np.float32),
                         source_track)


class StreamResampler:
    """Stateful PyAV resampler: chunked input equals whole-stream input.

    The resampler is opened with the *target* layout; the explicit matrix has
    already performed channel conversion, so PyAV only changes the rate.
    State carries across calls and :meth:`flush` emits the trailing filter
    samples, which is what makes the final sample count exact.
    """

    def __init__(self, source_rate: int, target_rate: int, layout: str) -> None:
        self.source_rate = int(source_rate)
        self.target_rate = int(target_rate)
        self.layout = canonical_layout(layout)
        if self.layout is None:
            raise RecoveryError(
                Code.UNSUPPORTED_LAYOUT, f"unsupported target layout {layout!r}"
            )
        self._resampler: av.AudioResampler | None = None
        self.channels = len(CANONICAL_LAYOUTS[self.layout])

    def _ensure(self) -> av.AudioResampler:
        if self._resampler is None:
            self._resampler = av.AudioResampler(
                format="fltp", layout=self.layout, rate=self.target_rate
            )
        return self._resampler

    def _collect(self, frame: av.AudioFrame | None) -> np.ndarray:
        resampler = self._ensure()
        collected = [f.to_ndarray() for f in resampler.resample(frame)]
        if not collected:
            return np.zeros((0, self.channels), dtype=np.float32)
        return np.concatenate(collected, axis=1).T.astype(np.float32)

    def process(self, samples: np.ndarray) -> np.ndarray:
        if self.source_rate == self.target_rate or samples.shape[0] == 0:
            return samples.astype(np.float32, copy=False)
        frame = av.AudioFrame.from_ndarray(
            np.ascontiguousarray(samples.T), format="fltp", layout=self.layout
        )
        frame.sample_rate = self.source_rate
        frame.time_base = Fraction(1, self.source_rate)
        return self._collect(frame)

    def flush(self) -> np.ndarray:
        if self._resampler is None or self.source_rate == self.target_rate:
            return np.zeros((0, self.channels), dtype=np.float32)
        return self._collect(None)


__all__ = [
    "CANONICAL_LAYOUTS",
    "ChannelMatrix",
    "StreamResampler",
    "canonical_layout",
    "channel_matrix",
]
