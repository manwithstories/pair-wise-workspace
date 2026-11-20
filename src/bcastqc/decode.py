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
    "VideoDecoder",
    "decode_video_blocks",
    "reader_for_codec",
    "LossPolicy",
    "AudioBlock",
    "AudioReader",
    "resample_window",
    "decode_video_blocks",
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


class VideoDecoder:
    """Stateful raw-stream decoder with explicit loss recovery.

    Access units are fed in stream order; decoded frames are released in
    display order through a bounded reorder buffer.  After an unrecoverable
    loss the decoder is marked damaged and skips input until the next
    keyframe, which guarantees that every frame emitted afterwards matches a
    clean decode (decoders do not reliably report reference damage).
    """

    def __init__(self, codec: Codec, frame_rate: Fraction, *, reorder_limit: int = 64) -> None:
        self.codec = codec
        self.frame_rate = Fraction(frame_rate)
        if self.frame_rate <= 0:
            raise make_error(ErrorCategory.TIMELINE_INVALID, "frame rate must be positive", frame_rate=str(frame_rate))
        self.reorder_limit = reorder_limit
        self._ctx = self._new_context()
        self._pending: dict[int, DecodedFrame] = {}
        self._next_emit = 0
        self._damaged = False
        self._last_frame: DecodedFrame | None = None
        self.stats = DecodeStats()

    # -- context ---------------------------------------------------------
    def _new_context(self) -> av.CodecContext:
        name = {Codec.H264: "h264", Codec.H265: "hevc", Codec.AV1: "libdav1d"}[self.codec]
        try:
            ctx = av.CodecContext.create(name, "r")
        except Exception as exc:  # pragma: no cover - environment dependent
            raise DecodeError(
                ErrorCategory.DECODER_UNAVAILABLE,
                f"decoder {name!r} is not available in this FFmpeg build",
                details={"decoder": name, "reason": str(exc)},
            ) from exc
        ctx.thread_count = 1  # deterministic output ordering
        return ctx

    def reset(self) -> None:
        self._ctx = self._new_context()
        self._damaged = False
        self.stats.decoder_resets += 1

    @property
    def damaged(self) -> bool:
        return self._damaged

    @property
    def last_frame(self) -> DecodedFrame | None:
        return self._last_frame

    def _time_of(self, pts: int) -> Fraction:
        return Fraction(pts, 1) / self.frame_rate

    def _make_frame(self, frame: av.VideoFrame, pts: int, *, keyframe: bool, source: str,
                    recovered: bool, au: AccessUnit, gap_ms: Fraction = Fraction(0)) -> DecodedFrame:
        rgb = frame.to_ndarray(format="rgb24")
        return DecodedFrame(
            pts=pts,
            time=self._time_of(pts),
            array=np.ascontiguousarray(rgb),
            keyframe=keyframe,
            source=source,
            recovered=recovered,
            gap_ms=gap_ms,
            source_offset=au.offset,
            source_size=au.size,
            au_index=au.index,
        )

    def _drain_pending(self, *, force: bool = False) -> Iterator[DecodedFrame]:
        while self._pending and len(self._pending) > self.reorder_limit:
            frame = self._pending.pop(min(self._pending))
            if frame.pts > self._next_emit:
                self._next_emit = frame.pts
        while True:
            if self._next_emit in self._pending:
                frame = self._pending.pop(self._next_emit)
            elif force:
                if not self._pending:
                    return
                frame = self._pending.pop(min(self._pending))
                self._next_emit = frame.pts
            else:
                return
            if frame.pts != self._next_emit:
                # the decoder skipped or repeated a display position
                self.stats.timestamp_jumps += 1
            self._next_emit = frame.pts + 1
            self._last_frame = frame
            yield frame

    def feed(self, au: AccessUnit, payload: bytes, *, recovered: bool = False,
             source: str = SOURCE_DECODE) -> Iterator[DecodedFrame]:
        """Decode one access unit supplied as bytes (possibly from a retry)."""
        if self._damaged:
            if not au.is_keyframe:
                self.stats.delivered += 1
                return
            self.reset()
            self.stats.keyframe_resyncs += 1
        packet = av.Packet(payload)
        packet.pts = au.index
        packet.dts = au.index
        packet.time_base = Fraction(1, 1)
        try:
            frames = list(self._ctx.decode(packet))
        except (av.error.InvalidDataError, av.error.EOFError) as exc:
            self.stats.illegal_units += 1
            self.mark_damaged()
            raise make_error(
                ErrorCategory.STREAM_CORRUPT,
                f"decoder rejected access unit {au.index}: {exc}",
                au_index=au.index,
                offset=au.offset,
                size=au.size,
            ) from exc
        for frame in frames:
            pts = frame.pts if frame.pts is not None else au.index
            decoded = self._make_frame(
                frame, int(pts), keyframe=au.is_keyframe, source=source,
                recovered=recovered, au=au,
            )
            self._pending[decoded.pts] = decoded
        yield from self._drain_pending()

    def mark_damaged(self) -> None:
        """Declare the stream damaged: discard pending frames, skip to keyframe."""
        self._damaged = True
        self._pending.clear()

    def conceal(self, gap_frames: int, *, au: AccessUnit | None = None,
                until_pts: int | None = None) -> Iterator[DecodedFrame]:
        """Emit concealed copies of the last good frame for a lost region.

        These frames are explicitly *not* recovered; their ``gap_ms`` states
        how long the gap was.  A concealed frame is skipped when no reference
        picture exists yet (loss before the first decoded frame).

        ``until_pts`` clamps the gap so concealment never claims a display
        position that the resynchronising keyframe will occupy: the concealed
        count is at most ``until_pts - _next_emit``.
        """
        if until_pts is not None:
            gap_frames = max(0, min(gap_frames, until_pts - self._next_emit))
        if self._last_frame is None or gap_frames <= 0:
            if gap_frames > 0:
                self._next_emit += gap_frames
            return
        gap_ms = self._time_of(gap_frames) * 1000
        offset_au = au or AccessUnit(-1, 0, 0, False)
        for offset in range(gap_frames):
            pts = self._next_emit + offset
            yield DecodedFrame(
                pts=pts,
                time=self._time_of(pts),
                array=self._last_frame.array.copy(),
                keyframe=False,
                source=SOURCE_CONCEALED,
                recovered=False,
                gap_ms=gap_ms,
                source_offset=offset_au.offset,
                source_size=offset_au.size,
                au_index=offset_au.index,
            )
        self._next_emit += gap_frames
        self.stats.concealed_frames += gap_frames

    def flush(self) -> Iterator[DecodedFrame]:
        try:
            for frame in self._ctx.decode(None):
                pts = frame.pts if frame.pts is not None else self._next_emit
                decoded = self._make_frame(
                    frame, int(pts), keyframe=bool(frame.key_frame),
                    source=SOURCE_FLUSH, recovered=False,
                    au=AccessUnit(-1, 0, 0, bool(frame.key_frame)),
                )
                self._pending[decoded.pts] = decoded
        except (av.error.InvalidDataError, av.error.EOFError):
            pass
        yield from self._drain_pending(force=True)


# ==========================================================================
# Block driver
# ==========================================================================


@dataclass
class LossPolicy:
    """How a lost access unit is treated before decoding."""

    model: LossModel | None = None
    retry: bool = True
    conceal: bool = True
    # a retry whose byte range lies beyond EOF is unrecoverable
    truncate_at_eof: bool = True


def _frame_time(pts: int, frame_rate: Fraction) -> Fraction:
    return Fraction(pts, 1) / frame_rate


def decode_video_blocks(
    path: Path,
    codec: Codec,
    frame_rate: Fraction,
    *,
    block_frames: int = 8,
    policy: LossPolicy | None = None,
    chunk_size: int = 1 << 16,
    token=None,
    stats: DecodeStats | None = None,
    total_units: int | None = None,
) -> Iterator[FrameBlock]:
    """Decode a raw stream into fixed-size blocks, applying the loss policy.

    The file is read incrementally; a lost access unit is retried by seeking
    back to its exact byte range (a local retransmission).  Unrecoverable
    losses mark the decoder damaged, concealed frames with an explicit gap
    cover the missing interval, and decoding resumes at the next keyframe.
    """
    import time

    path = Path(path)
    size = path.stat().st_size
    policy = policy or LossPolicy()
    if policy.model is not None and total_units is not None:
        policy.model.configure(total_units)
    decoder = VideoDecoder(codec, frame_rate)
    if stats is not None:
        decoder.stats = stats
    block: list[DecodedFrame] = []
    block_index = 0
    started = time.perf_counter_ns()
    gap_pending = 0
    scanner = reader_for_codec(codec)

    def flush_block() -> FrameBlock | None:
        nonlocal block, block_index, started
        if not block:
            return None
        out = FrameBlock(index=block_index, frames=block, started_ns=started, finished_ns=time.perf_counter_ns())
        block = []
        block_index += 1
        started = time.perf_counter_ns()
        return out

    def emit(frames: Iterable[DecodedFrame]) -> Iterator[FrameBlock]:
        for frame in frames:
            block.append(frame)
            if len(block) >= block_frames:
                produced = flush_block()
                if produced is not None:
                    yield produced

    def close_gap(*, until_pts: int | None = None) -> Iterator[FrameBlock]:
        nonlocal gap_pending
        if gap_pending:
            gap_ms = _frame_time(gap_pending, frame_rate) * 1000
            decoder.stats.max_gap_ms = max(decoder.stats.max_gap_ms, gap_ms)
            yield from emit(decoder.conceal(gap_pending, until_pts=until_pts))
            gap_pending = 0

    def attempt(au: AccessUnit, payload: bytes, *, recovered: bool, source: str) -> list[DecodedFrame]:
        try:
            return list(decoder.feed(au, payload, recovered=recovered, source=source))
        except QcError:
            decoder.mark_damaged()
            return []

    # Two handles: one sequential for scanning, one seekable for payload and
    # retry reads.  Interleaving seeks with the scanning handle would re-read
    # or skip bytes.
    with path.open("rb") as handle, path.open("rb") as payload_handle:
        def read_unit(au: AccessUnit) -> bytes | None:
            if au.offset + au.size > size:
                return None
            payload_handle.seek(au.offset)
            payload = payload_handle.read(au.size)
            return payload if len(payload) == au.size else None

        def handle_unit(au: AccessUnit) -> Iterator[FrameBlock]:
            nonlocal gap_pending
            decoder.stats.access_units += 1
            if token is not None and token.cancelled:
                return
            if decoder.damaged and not au.is_keyframe:
                gap_pending += 1
                return
            if decoder.damaged:
                # Conceal the skipped units only once the resync keyframe is
                # actually present; clamp so concealment never overlaps it.
                yield from close_gap(until_pts=au.index)

            lost = policy.model is not None and policy.model.drops(au.index)
            if lost:
                decoder.stats.packets_lost += 1
                if policy.retry and not policy.model.hard(au.index):
                    payload = read_unit(au)
                    if payload is not None and validate_access_unit(codec, payload, strict=True) is None:
                        frames = attempt(au, payload, recovered=True, source=SOURCE_RETRY)
                        if frames:
                            decoder.stats.recovered_packets += 1
                            decoder.stats.delivered += 1
                            decoder.stats.decoded_frames += len(frames)
                            yield from emit(frames)
                            return
                decoder.stats.unrecoverable_packets += 1
                decoder.mark_damaged()
                gap_pending += 1
                return

            payload = read_unit(au)
            if payload is None:
                # the unit's declared range runs past EOF: truncated tail
                decoder.stats.truncated_units += 1
                decoder.stats.unrecoverable_packets += 1
                decoder.mark_damaged()
                gap_pending += 1
                return
            problem = validate_access_unit(codec, payload, strict=True)
            if problem is not None:
                # structural conformance failure: an illegal access unit even
                # if the decoder would silently swallow it.  Not a recovered
                # packet: there is no picture to recover.
                decoder.stats.illegal_units += 1
                decoder.stats.unrecoverable_packets += 1
                decoder.mark_damaged()
                gap_pending += 1
                return
            frames = attempt(au, payload, recovered=False, source=SOURCE_DECODE)
            if not frames and decoder.damaged:
                gap_pending += 1
                return
            decoder.stats.delivered += 1
            decoder.stats.decoded_frames += len(frames)
            # A decoder may silently discard a corrupt unit (no exception, no
            # frame).  Detect that as an illegal access unit rather than
            # letting the display clock drift.
            if not frames and not au.is_keyframe:
                decoder.stats.illegal_units += 1
                decoder.mark_damaged()
                gap_pending += 1
                return
            yield from emit(frames)

        while True:
            if token is not None and token.cancelled:
                break
            chunk = handle.read(chunk_size)
            if not chunk:
                try:
                    tail_units = list(scanner.finish())
                except QcError as exc:
                    if exc.category is ErrorCategory.STREAM_EMPTY:
                        # nothing was ever parseable: an invalid input stream
                        raise
                    tail_units = []
                    decoder.stats.truncated_units += 1
                for au in tail_units:
                    yield from handle_unit(au)
                break
            for au in scanner.feed(chunk):
                yield from handle_unit(au)

    yield from close_gap()
    trailing = list(decoder.flush())
    decoder.stats.decoded_frames += len(trailing)
    yield from emit(trailing)
    produced = flush_block()
    if produced is not None:
        yield produced


# ==========================================================================
# Audio decoding
# ==========================================================================


@dataclass
class AudioBlock:
    """A block of interleaved float32 samples plus its exact sample window."""

    index: int
    start_sample: int          # first sample index in the *source* track
    sample_rate: int
    samples: np.ndarray        # shape (n, channels), float32
    tail_padded: int = 0
    source: str = "wav"

    @property
    def frames(self) -> int:
        return int(self.samples.shape[0])


class AudioReader:
    """Reads a PCM/WAV track block-wise, never loading the whole file."""

    def __init__(self, path: Path, *, channels: int | None = None) -> None:
        import soundfile as sf

        self.path = Path(path)
        try:
            self._handle = sf.SoundFile(str(self.path))
        except Exception as exc:
            raise make_error(
                ErrorCategory.AUDIO_FORMAT,
                f"cannot open audio file: {exc}",
                path=self.path.name,
            ) from exc
        self.sample_rate = int(self._handle.samplerate)
        self.channels = int(self._handle.channels)
        self.frames_total = int(self._handle.frames)
        self.target_channels = channels
        self.stats = {"read_frames": 0, "tail_padded": 0, "channel_mismatch": 0}

    def close(self) -> None:
        self._handle.close()

    def __enter__(self) -> "AudioReader":
        return self

    def __exit__(self, *exc_info) -> None:
        self.close()

    def read_blocks(self, block_frames: int = 4096) -> Iterator[AudioBlock]:
        """Yield blocks in source order, resampling block boundaries exactly."""
        if block_frames <= 0:
            raise ValueError("block_frames must be positive")
        index = 0
        position = 0
        while position < self.frames_total:
            want = min(block_frames, self.frames_total - position)
            data = self._handle.read(want, dtype="float32", always_2d=True)
            got = int(data.shape[0])
            if got == 0:
                break
            data = self._match_channels(data)
            yield AudioBlock(
                index=index,
                start_sample=position,
                sample_rate=self.sample_rate,
                samples=np.ascontiguousarray(data, dtype=np.float32),
            )
            self.stats["read_frames"] += got
            position += got
            index += 1

    def _match_channels(self, data: np.ndarray) -> np.ndarray:
        target = self.target_channels
        if target is None or target == data.shape[1]:
            return data
        self.stats["channel_mismatch"] += 1
        if data.shape[1] == 1:
            # mono -> N: duplicate the channel (documented upmix)
            return np.repeat(data, target, axis=1)
        if target == 1:
            # N -> mono: arithmetic mean, order-independent
            return data.mean(axis=1, keepdims=True, dtype=np.float32)
        # other combinations: take the first `min` channels and pad silence
        out = np.zeros((data.shape[0], target), dtype=np.float32)
        take = min(target, data.shape[1])
        out[:, :take] = data[:, :take]
        return out

    def read_window(self, start_sample: int, count: int) -> tuple[np.ndarray, int]:
        """Read ``count`` samples from ``start_sample``, zero-padding past EOF.

        Returns ``(samples, padded)`` where ``padded`` is the number of
        zero samples appended because the track ended early.
        """
        channels = self.target_channels or self.channels
        out = np.zeros((max(count, 0), channels), dtype=np.float32)
        if count <= 0:
            return out, 0
        available = max(0, min(count, self.frames_total - start_sample))
        if available > 0:
            self._handle.seek(start_sample)
            data = self._handle.read(available, dtype="float32", always_2d=True)
            data = self._match_channels(data)
            out[: data.shape[0], : data.shape[1]] = data
        padded = count - available
        if padded > 0:
            self.stats["tail_padded"] += padded
        return out, padded


def resample_window(samples: np.ndarray, source_rate: int, target_rate: int) -> np.ndarray:
    """Resample one block with an exact rational ratio (numpy linear interpolation).

    Interpolation is deliberately simple and deterministic: the output sample
    times are ``k * source_rate / target_rate``, evaluated exactly on integer
    numerator/denominator arithmetic, so block boundaries never drift.
    """
    if source_rate == target_rate or samples.shape[0] == 0:
        return samples
    n_in = samples.shape[0]
    out_len = int(round(n_in * target_rate / source_rate))
    if out_len == 0:
        return np.zeros((0, samples.shape[1]), dtype=np.float32)
    # position k in source samples: k * source_rate / target_rate
    numerator = np.arange(out_len, dtype=np.int64) * source_rate
    index = numerator // target_rate
    frac = (numerator % target_rate).astype(np.float32) / target_rate
    index = np.minimum(index, n_in - 1)
    nxt = np.minimum(index + 1, n_in - 1)
    out = samples[index] * (1.0 - frac[:, None]) + samples[nxt] * frac[:, None]
    return out.astype(np.float32)
