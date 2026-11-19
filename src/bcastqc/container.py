"""Minimal ISO-BMFF / QuickTime box inspection for MP4/MOV assets.

Two jobs:

* expose the sample tables (``stco``/``stsz``/``stsc``/``stts``/``stss``) so
  the recovery stage can tell a *complete* index from a short one, and can
  compute exactly which byte ranges of ``mdat`` the index claims;
* provide the box-level facts the report needs (mdat extent, declared media
  duration, per-track sample counts).

libavformat is deliberately not used here: it hides the tables and, for some
inconsistent files, silently demuxes only part of the payload without saying
so.  This parser never writes to the input.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

CONTAINER_BOXES = frozenset(
    {"moov", "trak", "mdia", "minf", "stbl", "edts", "dinf", "udta", "mvex"}
)

MAX_INDEX_ENTRIES = 2_000_000  # hard stop for absurd / hostile tables


@dataclass(slots=True)
class Box:
    type: str
    pos: int
    size: int
    header: int
    truncated: bool = False

    @property
    def body_start(self) -> int:
        return self.pos + self.header

    @property
    def body_end(self) -> int:
        return self.pos + self.size


@dataclass(slots=True)
class TrackIndex:
    track_id: int
    handler: str
    timescale: int
    sample_sizes: list[int] = field(default_factory=list)
    chunk_offsets: list[int] = field(default_factory=list)
    chunk_sample_counts: list[int] = field(default_factory=list)
    sync_samples: list[int] = field(default_factory=list)
    has_stss: bool = False
    stts_samples: int = 0
    stts_duration: int = 0
    media_duration: int = 0
    declared_duration: int = 0
    handler_type: bytes = b""

    @property
    def sample_count(self) -> int:
        return len(self.sample_sizes)

    @property
    def chunk_count(self) -> int:
        return len(self.chunk_offsets)

    @property
    def declared_seconds(self) -> Fraction | None:
        if not self.timescale:
            return None
        ticks = max(self.media_duration, self.stts_duration)
        return Fraction(ticks, self.timescale) if ticks else None

    def sample_ranges(self) -> list[tuple[int, int]]:
        """Byte ranges ``(offset, size)`` of every sample, in chunk order."""
        ranges: list[tuple[int, int]] = []
        cursor = 0
        for chunk_start, per_chunk in zip(self.chunk_offsets, self.chunk_sample_counts):
            offset = chunk_start
            for _ in range(per_chunk):
                if cursor >= len(self.sample_sizes):
                    break
                size = self.sample_sizes[cursor]
                ranges.append((offset, size))
                offset += size
                cursor += 1
            if cursor >= len(self.sample_sizes):
                break
        return ranges

    def to_dict(self) -> dict[str, object]:
        return {
            "track_id": self.track_id,
            "handler": self.handler,
            "timescale": self.timescale,
            "samples": self.sample_count,
            "chunks": self.chunk_count,
            "sync_samples": len(self.sync_samples) if self.has_stss else None,
            "stts_samples": self.stts_samples,
            "media_duration_s": _s(Fraction(self.media_duration, self.timescale))
            if self.timescale
            else None,
            "declared_duration_s": _s(Fraction(self.declared_duration, self.timescale))
            if self.timescale
            else None,
        }


@dataclass(slots=True)
class IndexReport:
    tracks: list[TrackIndex]
    mdat_start: int | None
    mdat_end: int | None
    mdat_declared_end: int | None
    file_size: int
    moov_pos: int | None
    issues: list[str]
    warnings: list[str] = field(default_factory=list)

    @property
    def truncated_tail(self) -> bool:
        return (
            self.mdat_end is not None
            and self.mdat_declared_end is not None
            and self.mdat_end < self.mdat_declared_end
        )

    def indexed_end(self) -> int | None:
        end = None
        for track in self.tracks:
            for offset, size in track.sample_ranges():
                stop = offset + size
                end = stop if end is None else max(end, stop)
        return end

    def unindexed_bytes(self) -> int:
        end = self.indexed_end()
        if end is None or self.mdat_end is None:
            return 0
        return max(0, self.mdat_end - end)

    @property
    def incomplete(self) -> bool:
        return bool(self.issues)

    def to_dict(self) -> dict[str, object]:
        return {
            "mdat_start": self.mdat_start,
            "mdat_end": self.mdat_end,
            "mdat_declared_end": self.mdat_declared_end,
            "file_size": self.file_size,
            "moov_pos": self.moov_pos,
            "truncated_tail": self.truncated_tail,
            "indexed_end": self.indexed_end(),
            "unindexed_bytes": self.unindexed_bytes(),
            "incomplete": self.incomplete,
            "issues": list(self.issues),
            "warnings": list(self.warnings),
            "tracks": [track.to_dict() for track in self.tracks],
        }


def _s(value: Fraction) -> float:
    return round(float(value), 6)


# --------------------------------------------------------------------------
# box walking
# --------------------------------------------------------------------------


def parse_boxes(data: bytes, start: int = 0, end: int | None = None) -> list[Box]:
    """Parse a flat run of sibling boxes."""
    limit = len(data) if end is None else min(end, len(data))
    boxes: list[Box] = []
    pos = start
    while pos + 8 <= limit:
        size = int.from_bytes(data[pos : pos + 4], "big")
        kind = data[pos + 4 : pos + 8].decode("latin1")
        header = 8
        if size == 1:
            if pos + 16 > limit:
                boxes.append(Box(kind, pos, 0, 8, True))
                break
            size = int.from_bytes(data[pos + 8 : pos + 16], "big")
            header = 16
        elif size == 0:
            size = limit - pos
        if size < header:
            boxes.append(Box(kind, pos, 0, header, True))
            break
        boxes.append(Box(kind, pos, size, header, pos + size > limit))
        if pos + size > limit:
            break
        pos += size
    return boxes


def find_child(data: bytes, box: Box, kind: str) -> Box | None:
    return next(
        (b for b in parse_boxes(data, box.body_start, box.body_end) if b.type == kind),
        None,
    )


def find_path(data: bytes, *kinds: str) -> Box | None:
    top = parse_boxes(data, 0, len(data))
    current: Box | None = None
    for kind in kinds:
        pool = top if current is None else parse_boxes(data, current.body_start, current.body_end)
        current = next((b for b in pool if b.type == kind), None)
        if current is None:
            return None
    return current


def payload(data: bytes, box: Box) -> bytes:
    return data[box.body_start : box.body_end]


def _u32(buf: bytes, offset: int) -> int:
    return int.from_bytes(buf[offset : offset + 4], "big")


def _u64(buf: bytes, offset: int) -> int:
    return int.from_bytes(buf[offset : offset + 8], "big")


def _pack(kind: str, body: bytes) -> bytes:
    return (8 + len(body)).to_bytes(4, "big") + kind.encode("latin1") + body


# --------------------------------------------------------------------------
# index reading
# --------------------------------------------------------------------------


def _read_stbl(data: bytes, trak: Box) -> dict[str, Box]:
    mdia = find_child(data, trak, "mdia")
    minf = find_child(data, mdia, "minf") if mdia else None
    stbl = find_child(data, minf, "stbl") if minf else None
    if stbl is None:
        return {}
    return {b.type: b for b in parse_boxes(data, stbl.body_start, stbl.body_end)}


def _movie_timescale(data: bytes) -> int:
    mvhd = find_path(data, "moov", "mvhd")
    if mvhd is None:
        return 0
    body = payload(data, mvhd)
    version = body[0] if body else 0
    if version == 0 and len(body) >= 16:
        return _u32(body, 12)
    if len(body) >= 24:
        return _u32(body, 20)
    return 0


def _read_trak(data: bytes, trak: Box, movie_timescale: int = 0) -> TrackIndex | None:
    stbl = _read_stbl(data, trak)
    if not stbl:
        return None
    tkhd = find_child(data, trak, "tkhd")
    mdia = find_child(data, trak, "mdia")
    mdhd = find_child(data, mdia, "mdhd") if mdia else None
    hdlr = find_child(data, mdia, "hdlr") if mdia else None

    track_id = 0
    declared = 0
    if tkhd is not None:
        body = payload(data, tkhd)
        version = body[0] if body else 0
        if version == 0 and len(body) >= 24:
            track_id, declared = _u32(body, 12), _u32(body, 20)
        elif len(body) >= 36:
            track_id, declared = _u32(body, 20), _u64(body, 28)

    timescale = 1
    media_duration = 0
    if mdhd is not None:
        body = payload(data, mdhd)
        version = body[0] if body else 0
        if version == 0 and len(body) >= 20:
            timescale, media_duration = _u32(body, 12) or 1, _u32(body, 16)
        elif len(body) >= 32:
            timescale, media_duration = _u32(body, 20) or 1, _u64(body, 24)

    handler = ""
    handler_type = b""
    if hdlr is not None:
        body = payload(data, hdlr)
        if len(body) >= 12:
            handler_type = body[8:12]
            handler = handler_type.decode("latin1", "replace")

    sizes: list[int] = []
    stsz = stbl.get("stsz")
    if stsz is not None:
        body = payload(data, stsz)
        uniform = _u32(body, 4)
        count = min(_u32(body, 8), MAX_INDEX_ENTRIES)
        if uniform:
            sizes = [uniform] * count
        else:
            available = max(0, (len(body) - 12) // 4)
            sizes = [_u32(body, 12 + 4 * i) for i in range(min(count, available))]

    offsets: list[int] = []
    co = stbl.get("stco") or stbl.get("co64")
    if co is not None:
        body = payload(data, co)
        count = min(_u32(body, 4), MAX_INDEX_ENTRIES)
        width = 4 if co.type == "stco" else 8
        available = max(0, (len(body) - 8) // width)
        for i in range(min(count, available)):
            base = 8 + width * i
            offsets.append(_u32(body, base) if width == 4 else _u64(body, base))

    per_chunk: list[int] = [0] * len(offsets)
    stsc = stbl.get("stsc")
    if stsc is not None and offsets:
        body = payload(data, stsc)
        count = min(_u32(body, 4), MAX_INDEX_ENTRIES)
        entries: list[tuple[int, int]] = []
        for i in range(count):
            base = 8 + 12 * i
            if base + 12 > len(body):
                break
            entries.append((_u32(body, base), _u32(body, base + 4)))
        for chunk in range(1, len(offsets) + 1):
            samples = 0
            for first, count_in_chunk in entries:
                if first <= chunk:
                    samples = count_in_chunk
                else:
                    break
            per_chunk[chunk - 1] = samples

    stts_samples = 0
    stts_duration = 0
    stts = stbl.get("stts")
    if stts is not None:
        body = payload(data, stts)
        count = min(_u32(body, 4), MAX_INDEX_ENTRIES)
        for i in range(count):
            base = 8 + 8 * i
            if base + 8 > len(body):
                break
            stts_samples += _u32(body, base)
            stts_duration += _u32(body, base) * _u32(body, base + 4)

    sync: list[int] = []
    stss = stbl.get("stss")
    if stss is not None:
        body = payload(data, stss)
        count = min(_u32(body, 4), MAX_INDEX_ENTRIES)
        for i in range(count):
            base = 8 + 4 * i
            if base + 4 > len(body):
                break
            sync.append(_u32(body, base))

    declared_media = 0
    if movie_timescale and timescale:
        declared_media = declared * timescale // movie_timescale
    return TrackIndex(
        track_id=track_id,
        handler=handler,
        handler_type=handler_type,
        timescale=timescale,
        sample_sizes=sizes,
        chunk_offsets=offsets,
        chunk_sample_counts=per_chunk,
        sync_samples=sync,
        has_stss=stss is not None,
        stts_samples=stts_samples,
        stts_duration=stts_duration,
        media_duration=media_duration,
        declared_duration=declared_media,
    )


def track_indices(data: bytes) -> list[TrackIndex]:
    moov = find_path(data, "moov")
    if moov is None or moov.truncated:
        return []
    movie_ts = _movie_timescale(data)
    tracks: list[TrackIndex] = []
    for trak in parse_boxes(data, moov.body_start, moov.body_end):
        if trak.type != "trak":
            continue
        index = _read_trak(data, trak, movie_ts)
        if index is not None:
            tracks.append(index)
    return tracks


def inspect_index(data: bytes) -> IndexReport:
    """Summarize the sample index and flag structural inconsistencies."""
    top = parse_boxes(data, 0, len(data))
    moov = next((b for b in top if b.type == "moov"), None)
    mdat = next((b for b in top if b.type == "mdat"), None)
    issues: list[str] = []
    warnings: list[str] = []

    mdat_start = None
    mdat_end = None
    declared_end = None
    if mdat is None:
        issues.append("missing mdat box")
    else:
        mdat_start = mdat.body_start
        declared_end = mdat.body_end
        mdat_end = min(declared_end, len(data))

    if moov is None:
        issues.append("missing moov box")
    elif moov.truncated:
        issues.append("moov box is truncated")

    tracks = track_indices(data)
    if moov is not None and not tracks:
        issues.append("moov contains no readable track index")

    for track in tracks:
        label = f"track {track.track_id} ({track.handler or 'unknown'})"
        if not track.sample_count:
            issues.append(f"{label} has no indexed samples")
            continue
        if track.stts_samples and track.stts_samples != track.sample_count:
            issues.append(
                f"{label} stts covers {track.stts_samples} samples, "
                f"stsz declares {track.sample_count}"
            )
        missing_chunks = sum(track.chunk_sample_counts) - track.sample_count
        if track.chunk_count and missing_chunks < 0:
            issues.append(
                f"{label} stsc describes {-missing_chunks} samples more than stco chunks hold"
            )
        if track.has_stss and not track.sync_samples:
            warnings.append(f"{label} has an empty sync sample table")
        if track.declared_seconds and track.timescale:
            indexed_seconds = Fraction(track.stts_duration, track.timescale)
            if indexed_seconds and indexed_seconds + Fraction(1, 100) < track.declared_seconds:
                issues.append(
                    f"{label} index covers {float(indexed_seconds):.3f}s of "
                    f"{float(track.declared_seconds):.3f}s declared"
                )

    report = IndexReport(
        tracks=tracks,
        mdat_start=mdat_start,
        mdat_end=mdat_end,
        mdat_declared_end=declared_end,
        file_size=len(data),
        moov_pos=moov.pos if moov is not None else None,
        issues=issues,
        warnings=warnings,
    )

    if mdat_start is not None and mdat_end is not None:
        for track in tracks:
            label = f"track {track.track_id} ({track.handler or 'unknown'})"
            ranges = track.sample_ranges()
            if not ranges:
                continue
            first_offset = ranges[0][0]
            last_end = ranges[-1][0] + ranges[-1][1]
            if first_offset < mdat_start:
                issues.append(f"{label} first sample starts before mdat")
            if last_end > mdat_end:
                issues.append(
                    f"{label} last sample ends at {last_end}, beyond available mdat end {mdat_end}"
                )
        tail = report.unindexed_bytes()
        if tail > 0 and not report.truncated_tail:
            issues.append(
                f"index leaves {tail} bytes of mdat unreferenced"
            )
    return report


def read_index(path: Path) -> tuple[bytes, IndexReport]:
    data = path.read_bytes()
    return data, inspect_index(data)


def _read_exact(stream, offset: int, size: int) -> bytes:
    stream.seek(offset)
    block = stream.read(size)
    if len(block) != size:
        raise OSError(f"short read at {offset}: wanted {size}, got {len(block)}")
    return block


def scan_file_header(path: Path) -> tuple[int, IndexReport | None]:
    """Read only the box headers and the ``moov`` payload of a file.

    Memory use is bounded by the index size, not by the media size: a 2 GiB
    asset with a 50 MB ``moov`` costs 50 MB here, and a camera-recorded file
    whose ``moov`` sits at the end only costs the ``moov`` itself.  Returns the
    byte index where ``moov`` ends (useful as a scan floor) and the report.
    """
    file_size = path.stat().st_size
    top: list[Box] = []
    with path.open("rb") as stream:
        pos = 0
        while pos + 8 <= file_size:
            header = _read_exact(stream, pos, 16 if pos + 16 <= file_size else 8)
            size = int.from_bytes(header[0:4], "big")
            kind = header[4:8].decode("latin1")
            head = 8
            if size == 1:
                size = int.from_bytes(header[8:16], "big")
                head = 16
            elif size == 0:
                size = file_size - pos
            if size < head:
                top.append(Box(kind, pos, 0, head, True))
                break
            top.append(Box(kind, pos, size, head, pos + size > file_size))
            if pos + size > file_size:
                break
            pos += size

    moov = next((b for b in top if b.type == "moov"), None)
    moov_data = b""
    if moov is not None and moov.size >= moov.header:
        with path.open("rb") as stream:
            moov_data = _read_exact(stream, moov.pos, min(moov.size, file_size - moov.pos))
    report = inspect_index_from_boxes(top, moov_data, moov, file_size)
    return (moov.body_end if moov is not None else 0), report


def inspect_index_from_boxes(
    top: list[Box], moov_data: bytes, moov: Box | None, file_size: int
) -> IndexReport:
    """Finish :func:`inspect_index` using only the ``moov`` bytes."""
    mdat = next((b for b in top if b.type == "mdat"), None)
    mdat_start = mdat.body_start if mdat is not None else None
    declared_end = mdat.body_end if mdat is not None else None
    mdat_end = min(declared_end, file_size) if declared_end is not None else None

    tracks: list[TrackIndex] = []
    issues: list[str] = []
    warnings: list[str] = []
    if moov is None:
        issues.append("missing moov box")
    elif moov.truncated or len(moov_data) < moov.size:
        issues.append("moov box is truncated")
    elif mdat is None:
        issues.append("missing mdat box")
    else:
        tracks = _tracks_from_moov(moov_data, moov)
        if not tracks:
            issues.append("moov contains no readable track index")
        _collect_issues(tracks, issues, warnings, mdat_start, mdat_end)

    report = IndexReport(
        tracks=tracks,
        mdat_start=mdat_start,
        mdat_end=mdat_end,
        mdat_declared_end=declared_end,
        file_size=file_size,
        moov_pos=moov.pos if moov is not None else None,
        issues=issues,
        warnings=warnings,
    )
    if mdat_start is not None and mdat_end is not None and not report.truncated_tail:
        tail = report.unindexed_bytes()
        if tail > 0:
            issues.append(f"index leaves {tail} bytes of mdat unreferenced")
    return report


def _tracks_from_moov(moov_data: bytes, moov: Box) -> list[TrackIndex]:
    """Read track tables from a standalone ``moov`` slice (offsets are local)."""
    shifted = Box("moov", 0, len(moov_data), moov.header, moov.truncated)
    movie_ts = 0
    mvhd = find_child(moov_data, shifted, "mvhd")
    if mvhd is not None:
        body = payload(moov_data, mvhd)
        version = body[0] if body else 0
        if version == 0 and len(body) >= 16:
            movie_ts = _u32(body, 12)
        elif len(body) >= 24:
            movie_ts = _u32(body, 20)
    tracks: list[TrackIndex] = []
    for trak in parse_boxes(moov_data, shifted.body_start, shifted.body_end):
        if trak.type != "trak":
            continue
        index = _read_trak(moov_data, trak, movie_ts)
        if index is not None:
            tracks.append(index)
    return tracks


def _collect_issues(
    tracks: list[TrackIndex],
    issues: list[str],
    warnings: list[str],
    mdat_start: int | None,
    mdat_end: int | None,
) -> None:
    for track in tracks:
        label = f"track {track.track_id} ({track.handler or 'unknown'})"
        if not track.sample_count:
            issues.append(f"{label} has no indexed samples")
            continue
        if track.stts_samples and track.stts_samples != track.sample_count:
            issues.append(
                f"{label} stts covers {track.stts_samples} samples, "
                f"stsz declares {track.sample_count}"
            )
        missing_chunks = sum(track.chunk_sample_counts) - track.sample_count
        if track.chunk_count and missing_chunks < 0:
            issues.append(
                f"{label} stsc describes {-missing_chunks} samples more than stco chunks hold"
            )
        if track.has_stss and not track.sync_samples:
            warnings.append(f"{label} has an empty sync sample table")
        if track.declared_seconds and track.timescale:
            indexed_seconds = Fraction(track.stts_duration, track.timescale)
            if indexed_seconds and indexed_seconds + Fraction(1, 100) < track.declared_seconds:
                issues.append(
                    f"{label} index covers {float(indexed_seconds):.3f}s of "
                    f"{float(track.declared_seconds):.3f}s declared"
                )
        if mdat_start is None or mdat_end is None:
            continue
        ranges = track.sample_ranges()
        if not ranges:
            continue
        if ranges[0][0] < mdat_start:
            issues.append(f"{label} first sample starts before mdat")
        last_end = ranges[-1][0] + ranges[-1][1]
        if last_end > mdat_end:
            issues.append(
                f"{label} last sample ends at {last_end}, beyond available mdat end {mdat_end}"
            )


def has_valid_boxes(data: bytes) -> bool:
    """Cheap sanity check used by the resume cache."""
    if len(data) < 16:
        return False
    top = parse_boxes(data, 0, len(data))
    return bool(top) and not top[0].truncated and any(b.type == "moov" for b in top)
