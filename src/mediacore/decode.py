"""Bounded decode helpers: keyframe seek validation and quality scoring.

PyAV is imported lazily so `mediacore --help` and pure-JSON utilities work even
before the codec stack is installed.  Two operations are exposed:

* :func:`decode_keyframe` decodes exactly one random-access unit (the AU at the
  index offset) with a fresh codec context and returns its picture, which is how
  seek verification observes a real frame rather than trusting the index.
* :func:`score_quality` decodes a bounded prefix of the stream and compares it
  with the deterministic reference frames, producing PSNR/SSIM-like statistics
  without ever holding more than one decoded frame at a time.

Both walk units in order and rely on the bounded scanner upstream; no function
here retains more than a few frames.
"""

from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction
from typing import Any, Callable, Iterable, Sequence

import numpy as np

from .bitstream import AccessUnit
from .errors import MediaCorruptError

_DECODERS = {"h264": "h264", "h265": "hevc", "av1": "libaom-av1"}


def _av():
    import av  # local import keeps CLI help fast and independent of codecs

    return av


@dataclass
class DecodedPicture:
    unit_index: int
    width: int
    height: int
    key: bool
    y: np.ndarray  # luma, float32, shape (h, w)

    @property
    def luma_mean(self) -> float:
        return float(self.y.mean())

    @property
    def nbytes(self) -> int:
        """Resident size of the retained luma plane, used by the frame cache."""
        return int(self.y.nbytes)


def decoder_name(codec: str) -> str:
    try:
        return _DECODERS[codec]
    except KeyError as exc:
        raise ValueError(f"unsupported codec {codec!r}") from exc


def new_decoder(codec: str, *, strict: bool = False):
    """Create a fresh single-threaded decoder context.

    ``strict`` asks libavcodec to raise on data errors instead of concealing
    them; the final-AU check uses it to distinguish a truncated picture from a
    picture that merely happened to be the last one in the file.  Note that
    H.265's software decoder conceals more aggressively than H.264's, so the
    check is deliberately conservative: clear evidence is acted on, silence is
    not treated as proof of integrity.
    """
    av = _av()
    context = av.CodecContext.create(decoder_name(codec), "r")
    context.thread_count = 1
    if strict:
        context.options = {"err_detect": "explode"}
    context.flags |= av.codec.context.Flags.OUTPUT_CORRUPT
    return context


def _picture_from_frame(frame, unit_index: int) -> DecodedPicture:
    # Keep the raw luma plane: "gray" would apply a range conversion, so PSNR
    # against the encoder input would be dominated by 16..235 vs 0..255 scaling.
    planes = frame.to_ndarray(format="yuv420p")
    luma = planes[: frame.height, : frame.width]
    return DecodedPicture(
        unit_index=unit_index, width=frame.width, height=frame.height,
        key=bool(frame.key_frame),
        y=np.asarray(luma, dtype=np.float32))


def decode_tail_probe(units: Sequence[AccessUnit], codec: str,
                      *, window: int = 32) -> tuple[bool, str, int | None]:
    """Decode the last GOP strictly to decide whether the tail was cut.

    Returns ``(truncated, reason, unit_index)``.  Only the final ``window`` units
    are decoded, starting from the last keyframe, so the check costs one GOP of
    work regardless of fixture length.  A decoder exception is treated as
    evidence of truncation; quiet concealment is not.
    """
    av = _av()
    if not units:
        return False, "", None
    start = max((i for i, u in enumerate(units) if u.keyframe), default=0)
    window_units = units[start:][-window:]
    context = new_decoder(codec, strict=True)
    for unit in window_units:
        if unit.truncated or not unit.complete:
            return True, "structurally incomplete final access unit", unit.index
        packet = av.Packet(unit.payload())
        packet.pts = unit.index
        packet.dts = unit.index
        try:
            context.decode(packet)
        except Exception as exc:
            return (True,
                    f"final access unit {unit.index} failed strict decode: "
                    f"{type(exc).__name__}", unit.index)
    try:
        context.decode(None)
    except Exception as exc:
        return (True, f"decoder flush failed: {type(exc).__name__}", units[-1].index)
    return False, "", None


def decode_keyframe(unit: AccessUnit, codec: str) -> DecodedPicture | None:
    """Decode one access unit in isolation and return its first picture.

    Returns ``None`` when the unit yields no picture (e.g. a parameter-set-only
    tail).  A decode failure is raised as :class:`MediaCorruptError` carrying the
    unit's byte offset so the manifest can locate it.
    """
    av = _av()
    context = new_decoder(codec)
    try:
        packet = av.Packet(unit.payload())
        packet.pts = unit.index
        packet.dts = unit.index
        frames = list(context.decode(packet))
        frames += list(context.decode(None))
    except Exception as exc:  # PyAV raises bare FFmpeg errors
        raise MediaCorruptError(
            f"cannot decode access unit {unit.index}: {exc}",
            offset=unit.offset, code="decode_failed") from exc
    if not frames:
        return None
    return _picture_from_frame(frames[0], unit.index)


def decode_prefix(
    units: Sequence[AccessUnit], codec: str, *, limit: int,
) -> Iterable[DecodedPicture]:
    """Yield decoded pictures from the first ``limit`` units, one at a time."""
    av = _av()
    context = new_decoder(codec)
    produced = 0
    for unit in units:
        if produced >= limit:
            return
        if unit.truncated or not unit.complete:
            # A truncated tail is not decoded; its defect is already reported.
            return
        packet = av.Packet(unit.payload())
        packet.pts = unit.index
        packet.dts = unit.index
        try:
            frames = context.decode(packet)
        except Exception as exc:
            raise MediaCorruptError(
                f"decode failed at unit {unit.index}: {exc}",
                offset=unit.offset, code="decode_failed") from exc
        for frame in frames:
            if produced >= limit:
                return
            produced += 1
            yield _picture_from_frame(frame, unit.index)
    for frame in context.decode(None):
        if produced >= limit:
            return
        produced += 1
        yield _picture_from_frame(frame, unit.index)


def decode_prefix_cached(
    units: Sequence[AccessUnit], codec: str, *, limit: int,
    cache_bytes: int = 0,
) -> tuple[Iterable[DecodedPicture], Any]:
    """Decode the prefix while retaining recent pictures in a byte-bounded LRU.

    Returns the picture iterator and the cache object, so the caller can report
    the cache's eviction statistics in the manifest.  Memory still has a hard
    ceiling (``cache_bytes``) regardless of clip length; the cache exists so a
    consumer that needs to revisit recently decoded pictures does not re-decode.
    """
    if cache_bytes <= 0:
        return decode_prefix(units, codec, limit=limit), None
    from .queues import FrameCache

    cache: FrameCache[DecodedPicture] = FrameCache(cache_bytes, name="quality")

    def generator() -> Iterable[DecodedPicture]:
        for position, picture in enumerate(decode_prefix(units, codec, limit=limit)):
            cache.put(position, picture)
            yield picture

    return generator(), cache


def _psnr(reference: np.ndarray, decoded: np.ndarray) -> float:
    ref = reference.astype(np.float64)
    dec = decoded.astype(np.float64)
    mse = float(np.mean((ref - dec) ** 2))
    if mse <= 1e-12:
        return 99.0
    return float(min(99.0, 10.0 * np.log10(255.0 ** 2 / mse)))


@dataclass
class QualityStats:
    frames: int = 0
    cache_stats: dict[str, Any] | None = None
    psnr_sum: float = 0.0
    psnr_min: float = 99.0
    psnr_max: float = 0.0
    luma_mse_sum: float = 0.0

    def add(self, psnr: float, mse: float) -> None:
        self.frames += 1
        self.psnr_sum += psnr
        self.psnr_min = min(self.psnr_min, psnr)
        self.psnr_max = max(self.psnr_max, psnr)
        self.luma_mse_sum += mse

    def to_dict(self) -> dict[str, Any]:
        if not self.frames:
            return {"frames": 0, "frame_cache": self.cache_stats,
                    "psnr_db": 0.0, "psnr_min_db": 0.0,
                    "psnr_max_db": 0.0, "luma_mse": 0.0, "finite": False}
        psnr = self.psnr_sum / self.frames
        return {
            "frames": self.frames,
            "frame_cache": self.cache_stats,
            "psnr_db": round(psnr, 3),
            "psnr_min_db": round(self.psnr_min, 3),
            "psnr_max_db": round(self.psnr_max, 3),
            "luma_mse": round(self.luma_mse_sum / self.frames, 4),
            "finite": bool(np.isfinite(psnr)),
        }


def score_quality(
    units: Sequence[AccessUnit],
    codec: str,
    reference_of: Sequence[np.ndarray] | Callable[[int], np.ndarray],
    *,
    limit: int,
    cache_bytes: int = 0,
) -> QualityStats:
    """Compare decoded luma with the deterministic reference frames.

    ``reference_of`` is either a sequence of luma planes indexed by unit index or
    a callable that generates one, so the caller can keep only the references it
    needs in memory.  The loop pulls one decoded picture at a time, so memory
    stays bounded regardless of stream length.
    """
    stats = QualityStats()
    lookup = reference_of if callable(reference_of) else reference_of.__getitem__
    pictures, cache = decode_prefix_cached(
        units, codec, limit=limit, cache_bytes=cache_bytes)
    for picture in pictures:
        try:
            reference = lookup(picture.unit_index)
        except (IndexError, KeyError):
            continue
        if reference.shape != picture.y.shape:
            continue
        mse = float(np.mean((reference.astype(np.float64) - picture.y) ** 2))
        stats.add(_psnr(reference, picture.y), mse)
    if cache is not None:
        stats.cache_stats = cache.stats.to_dict()
    return stats


def frame_seconds(pts: int, time_base: Fraction) -> float:
    return float(Fraction(pts) * time_base)
