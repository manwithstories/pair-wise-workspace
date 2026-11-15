"""Expand stbl sub-boxes into per-sample durations and byte ranges."""
from __future__ import annotations

from dataclasses import dataclass, field

from .boxio import BoxError

__all__ = ["SampleTable", "SampleTableError", "parse_sample_table",
           "iter_traks", "stbl_of", "read_codec"]

ENCRYPTED_SAMPLE_ENTRIES = frozenset({"encv", "enca", "encr", "encs"})


class SampleTableError(BoxError):
    """Inconsistent sample table (e.g. stsc runs past stsz entries)."""


@dataclass
class SampleTable:
    track_id: int
    codec: str | None
    sample_count: int
    stts_entries: int
    stsc_entries: int
    stsz_entries: int
    chunk_count: int
    durations: list = field(default_factory=list)
    sizes: list = field(default_factory=list)
    chunk_offsets: list = field(default_factory=list)
    samples_per_chunk: list = field(default_factory=list)
    sync_samples: list | None = None
    ctts_offsets: list | None = None
    warnings: list = field(default_factory=list)

    def sample_ranges(self):
        """Yield (offset, size) per sample; mdat is never read."""
        idx = 0
        for chunk_offset, spc in zip(self.chunk_offsets, self.samples_per_chunk):
            pos = chunk_offset
            for _ in range(spc):
                if idx >= self.sample_count:
                    return
                size = self.sizes[idx]
                yield pos, size
                pos += size
                idx += 1


def _find(reader, box, type4):
    if box is None:
        return None
    for child in reader.children(box):
        if child.type == type4:
            return child
    return None


def _track_id(reader, trak):
    tkhd = _find(reader, trak, "tkhd")
    if tkhd is None:
        return None
    version, _flags, off = reader.fullbox(tkhd)
    return reader.unpack(">I", off + (16 if version == 1 else 8))[0]


def iter_traks(reader, moov):
    """Yield (trak_box, track_id) for each trak in moov."""
    for trak in reader.children(moov):
        if trak.type == "trak":
            yield trak, _track_id(reader, trak)


def stbl_of(reader, trak):
    return _find(reader, _find(reader, _find(reader, trak, "mdia"), "minf"), "stbl")


def read_codec(reader, stbl):
    stsd = _find(reader, stbl, "stsd")
    if stsd is None:
        return None
    _version, _flags, off = reader.fullbox(stsd)
    (count,) = reader.unpack(">I", off)
    if count < 1:
        return None
    return reader.read(off + 8, 4).decode("latin-1")


def _parse_stts(reader, box):
    _version, _flags, off = reader.fullbox(box)
    (count,) = reader.unpack(">I", off)
    off += 4
    durations = []
    for _ in range(count):
        n, delta = reader.unpack(">II", off)
        off += 8
        durations.extend([delta] * n)
    return durations, count


def _parse_ctts(reader, box):
    version, _flags, off = reader.fullbox(box)
    (count,) = reader.unpack(">I", off)
    off += 4
    fmt = ">i" if version == 1 else ">I"  # v1: signed composition offsets
    offsets = []
    for _ in range(count):
        n = reader.unpack(">I", off)[0]
        value = reader.unpack(fmt, off + 4)[0]
        off += 8
        offsets.extend([value] * n)
    return offsets


def check_decode_order(durations, offsets):
    """With negative composition offsets, validate decode-order sanity."""
    issues = []
    if not any(o < 0 for o in offsets):
        return issues
    dts = 0
    cts = []
    for dur, off in zip(durations, offsets):
        cts.append(dts + off)
        dts += dur
    if any(c < 0 for c in cts):
        issues.append("negative composition time with signed ctts")
    if len(set(cts)) != len(cts):
        issues.append("duplicate composition timestamps break decode-order monotonicity")
    return issues


def _parse_stsc(reader, box, chunk_count, sample_count):
    _version, _flags, off = reader.fullbox(box)
    (count,) = reader.unpack(">I", off)
    off += 4
    runs = []
    for _ in range(count):
        first, spc, desc = reader.unpack(">III", off)
        off += 12
        runs.append((first, spc, desc))
    per_chunk = []
    total = 0
    for i, (first, spc, _desc) in enumerate(runs):
        nxt = runs[i + 1][0] if i + 1 < len(runs) else chunk_count + 1
        for _chunk in range(first, min(nxt, chunk_count + 1)):
            per_chunk.append(spc)
            total += spc
            if total > sample_count:
                raise SampleTableError(
                    "stsc run %d expands past stsz entries (%d samples)"
                    % (i, sample_count))
    return per_chunk, len(runs), total


def _parse_stsz(reader, box):
    _version, _flags, off = reader.fullbox(box)
    sample_size, count = reader.unpack(">II", off)
    off += 8
    if sample_size:
        return [sample_size] * count, count
    sizes = [reader.unpack(">I", off + 4 * i)[0] for i in range(count)]
    return sizes, count


def _parse_stz2(reader, box):
    _version, _flags, off = reader.fullbox(box)
    field_size = reader.unpack(">B", off + 3)[0]
    (count,) = reader.unpack(">I", off + 4)
    data_off = off + 8
    sizes = []
    if field_size == 4:
        for i in range(count):
            byte = reader.unpack(">B", data_off + i // 2)[0]
            sizes.append((byte >> 4) if i % 2 == 0 else (byte & 0x0F))
    elif field_size == 8:
        for i in range(count):
            sizes.append(reader.unpack(">B", data_off + i)[0])
    elif field_size == 16:
        for i in range(count):
            sizes.append(reader.unpack(">H", data_off + 2 * i)[0])
    else:
        raise SampleTableError("unsupported stz2 field_size %d" % field_size)
    return sizes, count


def parse_sample_table(reader, trak, track_id):
    stbl = stbl_of(reader, trak)
    if stbl is None:
        raise SampleTableError("track %s has no stbl box" % track_id)
    codec = read_codec(reader, stbl)
    warnings = []
    if codec in ENCRYPTED_SAMPLE_ENTRIES:
        warnings.append(
            "encrypted sample entry %r: metadata only, media not decoded" % codec)
    durations, stts_entries = [], 0
    ctts_offsets = None
    sizes, stsz_entries = [], 0
    chunk_offsets = []
    per_chunk, stsc_entries, covered = [], 0, 0
    sync = None
    for box in reader.children(stbl):
        if box.type == "stts":
            durations, stts_entries = _parse_stts(reader, box)
        elif box.type == "ctts":
            ctts_offsets = _parse_ctts(reader, box)
        elif box.type == "stsz":
            sizes, stsz_entries = _parse_stsz(reader, box)
        elif box.type == "stz2":
            sizes, stsz_entries = _parse_stz2(reader, box)
        elif box.type in ("stco", "co64"):
            _v, _f, off = reader.fullbox(box)
            (n,) = reader.unpack(">I", off)
            fmt = ">Q" if box.type == "co64" else ">I"
            width = 8 if box.type == "co64" else 4
            chunk_offsets = [reader.unpack(fmt, off + 4 + width * i)[0]
                             for i in range(n)]
        elif box.type == "stss":
            _v, _f, off = reader.fullbox(box)
            (n,) = reader.unpack(">I", off)
            sync = [reader.unpack(">I", off + 4 + 4 * i)[0] for i in range(n)]
    sample_count = stsz_entries
    stsc_box = _find(reader, stbl, "stsc")
    if stsc_box is not None:
        per_chunk, stsc_entries, covered = _parse_stsc(
            reader, stsc_box, len(chunk_offsets), sample_count)
        if covered < sample_count:
            warnings.append(
                "stsc covers %d of %d stsz samples" % (covered, sample_count))
    if ctts_offsets is not None:
        warnings.extend(check_decode_order(durations, ctts_offsets))
    return SampleTable(
        track_id=track_id, codec=codec, sample_count=sample_count,
        stts_entries=stts_entries, stsc_entries=stsc_entries,
        stsz_entries=stsz_entries, chunk_count=len(chunk_offsets),
        durations=durations, sizes=sizes, chunk_offsets=chunk_offsets,
        samples_per_chunk=per_chunk, sync_samples=sync,
        ctts_offsets=ctts_offsets, warnings=warnings)

