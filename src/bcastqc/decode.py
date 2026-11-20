"""Time-based decoding of video access units and PCM/WAV audio.

Video
-----
Raw H.264/H.265/AV1 streams carry no container timestamps, so this module
assigns each access unit an integer timestamp in *frames* (``1/frame_rate``)
and decodes through PyAV one access unit at a time.  Decoders may emit frames
out of order (B-frames), so a small, bounded reorder buffer sorts them into
display order before they leave the module as :class:`FrameBlock` objects.
Block size is fixed, which is what bounds memory: at most one block plus the
reorder buffer is alive regardless of material length.

Loss handling
-------------
A deterministic :class:`LossModel` (seeded, count-accurate selection over the
access-unit index) decides which access units the "receiver" fails to
deliver.  Every loss is retried once by reading the exact byte range back from
local storage - a retransmission.  If the bytes are intact the access unit is decoded and the
resulting frames are marked ``recovered=True`` with ``source="retry"``.  If
the byte range is beyond EOF or fails to decode, the loss is unrecoverable:
the decoder is reset, access units are skipped until the next keyframe, and
concealed frames are emitted from the last good frame with
``recovered=False`` and an explicit gap duration.  Concealed frames are never
counted as recovered.

Audio
-----
WAV/PCM is read block-wise with :mod:`soundfile`; sample positions are derived
from the timeline with exact rational arithmetic (see :mod:`bcastqc.render`),
so no floating-point drift accumulates.  Sample-rate differences are resolved
by resampling at the sample-index level, channel mismatches by a documented
rule, and a short tail is zero-padded and recorded.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path
from typing import Iterable, Iterator

import av
import numpy as np

from .bitstream import (
    AccessUnit,
    Codec,
    StreamScanner,
    detect_codec,
    validate_access_unit,
)
from .errors import ErrorCategory, DecodeError, QcError, make_error

__all__ = [
    "DecodedFrame",
    "FrameBlock",
    "DecodeStats",
    "LossModel",
    "reader_for_codec",
    "probe_stream",
]

# Frames produced for a lost region are flagged with this source.
SOURCE_DECODE = "decode"
SOURCE_RETRY = "retry"
SOURCE_CONCEALED = "concealed"
SOURCE_FLUSH = "flush"


@dataclass
class DecodedFrame:
    """One decoded picture in display order."""

    pts: int                     # frame index in the stream's frame clock
    time: Fraction               # exact seconds from the start of the stream
    array: np.ndarray            # HxWx3 uint8 RGB
    keyframe: bool
    source: str = SOURCE_DECODE
    recovered: bool = False
    gap_ms: Fraction = Fraction(0)
    source_offset: int = 0
    source_size: int = 0
    au_index: int = -1

    @property
    def pixel_hash(self) -> str:
        return hashlib.sha256(np.ascontiguousarray(self.array).tobytes()).hexdigest()

    @property
    def width(self) -> int:
        return int(self.array.shape[1])

    @property
    def height(self) -> int:
        return int(self.array.shape[0])


@dataclass
class FrameBlock:
    """A fixed-size, display-ordered group of frames."""

    index: int
    frames: list[DecodedFrame]
    started_ns: int = 0
    finished_ns: int = 0

    @property
    def latency_ms(self) -> float:
        return (self.finished_ns - self.started_ns) / 1e6

    def __len__(self) -> int:
        return len(self.frames)


@dataclass
class DecodeStats:
    access_units: int = 0
    delivered: int = 0
    packets_lost: int = 0
    recovered_packets: int = 0
    unrecoverable_packets: int = 0
    concealed_frames: int = 0
    decoded_frames: int = 0
    decoder_resets: int = 0
    truncated_units: int = 0
    illegal_units: int = 0
    timestamp_jumps: int = 0
    keyframe_resyncs: int = 0
    max_gap_ms: Fraction = Fraction(0)

    @property
    def recovery_rate(self) -> Fraction:
        attempts = self.recovered_packets + self.unrecoverable_packets
        if attempts == 0:
            return Fraction(1)
        return Fraction(self.recovered_packets, attempts)

    def to_json(self) -> dict:
        return {
            "access_units": self.access_units,
            "delivered": self.delivered,
            "packets_lost": self.packets_lost,
            "recovered_packets": self.recovered_packets,
            "unrecoverable_packets": self.unrecoverable_packets,
            "concealed_frames": self.concealed_frames,
            "decoded_frames": self.decoded_frames,
            "decoder_resets": self.decoder_resets,
            "truncated_units": self.truncated_units,
            "illegal_units": self.illegal_units,
            "timestamp_jumps": self.timestamp_jumps,
            "keyframe_resyncs": self.keyframe_resyncs,
            "recovery_rate": float(self.recovery_rate),
            "max_gap_ms": float(self.max_gap_ms),
        }


class LossModel:
    """Deterministic, count-accurate packet-loss simulation.

    The model places exactly ``round(drop_rate * total)`` losses among the
    access units, choosing positions from a seeded, uniformly-strided
    sequence.  A hash-per-index model (drop if ``hash(i) < rate``) is simpler
    but a short program can legitimately draw zero losses, which makes the
    recovery metrics vacuous; exact counts make ``--drop-rate 0.02`` always
    mean two percent.  ``total`` must be supplied before the first query.

    ``hard(au_index)`` marks a deterministic subset of losses whose retry is
    destined to fail (a physically damaged region).  Because both functions
    are pure functions of ``(seed, total, au_index)`` they do not depend on
    iteration order or retries, which keeps reruns reproducible.
    """

    def __init__(self, seed: int, drop_rate: float, hard_loss_rate: float = 0.0,
                 *, total: int | None = None) -> None:
        if not 0.0 <= drop_rate <= 1.0:
            raise ValueError("drop_rate must be within [0, 1]")
        if not 0.0 <= hard_loss_rate <= 1.0:
            raise ValueError("hard_loss_rate must be within [0, 1]")
        self.seed = seed
        self.drop_rate = drop_rate
        self.hard_loss_rate = hard_loss_rate
        self._total = total
        self._dropped: frozenset[int] | None = None
        self._hard: frozenset[int] | None = None

    def configure(self, total: int) -> "LossModel":
        """Bind the number of access units (idempotent for the same total)."""
        if total < 0:
            raise ValueError("total must be non-negative")
        if self._total != total:
            self._total = total
            self._dropped = None
            self._hard = None
        return self

    def _indices(self, count: int, salt: str) -> frozenset[int]:
        if count <= 0:
            return frozenset()
        total = self._total if self._total is not None else count * 4
        if total <= 0:
            return frozenset()
        rng = np.random.default_rng(
            int.from_bytes(hashlib.sha256(f"{self.seed}:{salt}:{total}".encode()).digest()[:8], "big")
        )
        picked = rng.choice(total, size=min(count, total), replace=False)
        return frozenset(int(v) for v in picked)

    def _ensure(self) -> None:
        if self._dropped is None:
            total = self._total or 0
            count = int(round(self.drop_rate * total))
            self._dropped = self._indices(count, "drop")
            hard_count = int(round(self.hard_loss_rate * count))
            base = sorted(self._dropped)
            rng = np.random.default_rng(
                int.from_bytes(hashlib.sha256(f"{self.seed}:hard:{total}".encode()).digest()[:8], "big")
            )
            chosen = rng.choice(len(base), size=min(hard_count, len(base)), replace=False) if base else []
            self._hard = frozenset(base[int(i)] for i in chosen)

    def drops(self, au_index: int) -> bool:
        self._ensure()
        return au_index in self._dropped  # type: ignore[operator]

    def hard(self, au_index: int) -> bool:
        self._ensure()
        return au_index in self._hard  # type: ignore[operator]

    @property
    def planned_losses(self) -> int:
        self._ensure()
        return len(self._dropped or ())

    def to_json(self) -> dict:
        self._ensure()
        return {
            "seed": self.seed,
            "drop_rate": self.drop_rate,
            "hard_loss_rate": self.hard_loss_rate,
            "total": self._total,
            "planned_losses": self.planned_losses,
            "hard_losses": len(self._hard or ()),
        }


def reader_for_codec(codec: Codec, max_buffer: int = 8 * 1024 * 1024) -> StreamScanner:
    return StreamScanner(codec, max_buffer=max_buffer)


def _read_access_units(path: Path, codec: Codec, chunk_size: int = 1 << 16) -> list[AccessUnit]:
    scanner = reader_for_codec(codec)
    units: list[AccessUnit] = []
    with path.open("rb") as handle:
        while True:
            chunk = handle.read(chunk_size)
            if not chunk:
                break
            units.extend(scanner.feed(chunk))
    try:
        units.extend(scanner.finish())
    except QcError:
        # trailing bytes are reported by the decoder as a truncated tail
        if not units:
            raise
    return units


def probe_stream(path: Path) -> tuple[Codec, int]:
    """Detect codec and file size for a raw stream."""
    size = path.stat().st_size
    with path.open("rb") as handle:
        head = handle.read(4096)
    return detect_codec(head), size


# ==========================================================================
# Video decoding
# ==========================================================================
