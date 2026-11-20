"""Low-level container access: PyAV handles and ISO-BMFF box parsing.

This layer knows how to open a file, pick a stream, unwrap a counter, decode
packets until the first rejection and read the ``stts`` sample-time table
directly.  It raises no :class:`~mediacore.errors.MediaError` of its own; the
scan layer above decides what a failure *means*.
"""

from __future__ import annotations

import struct
from pathlib import Path
from typing import Any

from .errors import ParameterError
from .timebase import Timebase, Us, round_half_away

def _av():
    import av  # local import keeps module import cheap and testable

    return av


def _open(path: str):
    av = _av()
    # NB: do *not* pass err_detect=explode on the demuxer.  It also promotes
    # container warnings (e.g. a recovered edit list) into hard errors, which
    # makes libavformat abandon a perfectly healthy stream at packet 0.  The
    # decoder is configured separately in _first_corrupt_packet.
    return av.open(path)


def _pick_stream(container, *, kind: str, stream_index: int | None):
    candidates = list(container.streams.video if kind == "video" else container.streams.audio)
    if not candidates:
        raise ProbeError(
            f"no {kind} stream in file",
            code="probe.missing_stream",
            path=container.name,
            kind=kind,
            recoverable=False,
        )
    if stream_index is not None:
        for stream in candidates:
            if stream.index == stream_index:
                return stream
        raise ProbeError(
            "requested stream index not found",
            code="probe.missing_stream",
            path=container.name,
            stream_index=stream_index,
            recoverable=False,
        )
    return candidates[0]


def _stream_by_index(container, stream_index: int):
    """Look up a stream by its index.

    ``container.streams.get(i)`` returns a *list* in PyAV 12 (it implements
    the generic ``StreamContainer.get`` API), so it cannot be used to fetch a
    single stream.  Iterate instead.
    """
    for stream in container.streams:
        if stream.index == stream_index:
            return stream
    return None


def _short(exc: BaseException) -> str:
    text = str(exc).strip().replace("\n", " ")
    return text[:240] if len(text) > 240 else text


def _nominal_step(stream, tb: Timebase) -> int | None:
    rate = getattr(stream, "average_rate", None)
    if rate:
        return max(1, tb.from_us(int(1_000_000 / float(rate))))
    if getattr(stream, "type", "") == "audio":
        sr = stream.codec_context.sample_rate
        if sr:
            return max(1, tb.from_us(int(1_000_000 * 1024 / sr)))
    return None


class _Unwrapper:
    """On-demand counter-wrap correction for a tick sequence.

    Only used when the caller supplies ``wrap_at``; otherwise it is the
    identity.  RTP-style audio counters wrap at 2**32, so a timestamp that
    moves backwards by more than half the modulus is a wrap, not a reorder.
    """

    __slots__ = ("modulus", "prev", "carried")

    def __init__(self, modulus: int | None) -> None:
        self.modulus = modulus
        self.prev: int | None = None
        self.carried = 0

    def feed(self, ticks: int) -> int:
        if self.modulus is None:
            return ticks
        value = ticks + self.carried
        if self.prev is not None:
            delta = value - self.prev
            if delta < -self.modulus // 2:
                self.carried += self.modulus
                value += self.modulus
            elif delta > self.modulus // 2:
                self.carried -= self.modulus
                value -= self.modulus
        self.prev = value
        return value


def _is_demux_fault(exc: BaseException) -> bool:
    """True for libavformat/decoder errors, False for our own bugs.

    PyAV raises ``av.error.FFmpegError`` subclasses (``InvalidDataError``,
    ``EOFError``, ...) for media faults.  Anything else — ``NameError``,
    ``TypeError``, ... — is a defect in this module and must propagate.
    """
    av = _av()
    return isinstance(exc, av.error.FFmpegError)


def _first_corrupt_packet(path: str, stream_index: int, limit: int) -> tuple[int, Us] | None:
    """Decode packets from the stream head and report the first rejection.

    Decoding must start at the beginning of the stream: an MP4 with dropped
    sample-table entries stores absolute byte offsets, so seeking into the
    middle lands on a byte that no longer begins a packet and produces a
    spurious decode error at the first packet read.  Sequential decoding also
    mirrors what the pipeline will actually do.

    Search is bounded: most corruption appears quickly, and a 10-minute file
    should not pay a full decode during probing.  ``limit`` is raised by the
    caller when the media is short enough to scan exhaustively.
    """
    av = _av()
    try:
        container = av.open(path)
    except Exception:
        return None
    try:
        stream = _stream_by_index(container, stream_index)
        if stream is None:
            return None
        try:
            stream.codec_context.options = {"err_detect": "explode"}
        except Exception:
            pass
        tb = Timebase.parse(stream.time_base)
        ordinal = 0
        # Once the first keyframe has been decoded, every packet is decodable
        # in principle; before that, a reference frame is legitimately needed.
        for packet in container.demux(stream):
            if packet.size == 0 and packet.pts is None:
                continue
            if not packet.is_keyframe and ordinal == 0:
                ordinal += 1
                continue
            pts = packet.pts
            pts_us = round_half_away(tb.to_us_fraction(pts)) if pts is not None else 0
            try:
                frames = packet.decode()
            except Exception as exc:
                if not _is_demux_fault(exc):
                    raise
                return ordinal, pts_us
            if any(getattr(frame, "is_corrupt", False) for frame in frames):
                return ordinal, pts_us
            ordinal += 1
            if limit and ordinal > limit:
                break
        return None
    except Exception:
        return None
    finally:
        container.close()


def _declared_sample_total(path: str, stream_index: int) -> int:
    """Sum the ``stts`` sample counts for one MP4 track.

    The stream header's ``frames`` field is not trustworthy after sample-table
    damage (libavformat reports the value it can *demux*), whereas ``stts``
    describes every sample the container claims to hold.  We read it directly
    so a lost ``stco`` entry is detectable as ``declared > demuxed``.

    Returns 0 for non-ISO-BMFF files or when anything is unexpected; callers
    fall back to ``declared_frames``.
    """
    try:
        with open(path, "rb") as fh:
            data = fh.read()
        tracks = _child_boxes(data, 0, len(data), b"moov")
        if not tracks:
            return 0
        moov = tracks[0]
        track_boxes = _child_boxes(data, *moov, b"trak")
        for position, trak in enumerate(track_boxes):
            # Track order in moov matches stream index order for MP4.
            if position != stream_index:
                continue
            for mdia in _child_boxes(data, *trak, b"mdia"):
                for minf in _child_boxes(data, *mdia, b"minf"):
                    for stbl in _child_boxes(data, *minf, b"stbl"):
                        for stts in _child_boxes(data, *stbl, b"stts"):
                            return _stts_total(data, stts)
        return 0
    except (OSError, ValueError, IndexError, struct.error):
        return 0


def _stts_total(data: bytes, box: tuple[int, int]) -> int:
    """``box`` is ``(payload_start, payload_end)``; returns sum of counts."""
    start, end = box
    if end - start < 8:
        return 0
    entries = struct.unpack(">I", data[start + 4 : start + 8])[0]
    total = 0
    cursor = start + 8
    for _ in range(entries):
        if cursor + 8 > end:
            break
        count = struct.unpack(">I", data[cursor : cursor + 4])[0]
        total += count
        cursor += 8
    return total


def _child_boxes(data: bytes, start: int, end: int, wanted: bytes) -> list[tuple[int, int]]:
    """Return payload ranges of direct children named ``wanted``.

    Accepts every ``wanted`` child; for the single-child containers we walk
    (moov/trak/mdia/minf/stbl) the first match is always the right one.
    """
    out: list[tuple[int, int]] = []
    offset = start
    while offset + 8 <= end:
        size = struct.unpack(">I", data[offset : offset + 4])[0]
        header = 8
        if size == 1:
            if offset + 16 > end:
                break
            size = struct.unpack(">Q", data[offset + 8 : offset + 16])[0]
            header = 16
        elif size == 0:
            size = end - offset
        if size < header or offset + size > end:
            break
        if data[offset + 4 : offset + 8] == wanted:
            out.append((offset + header, offset + size))
        offset += size
    return out

