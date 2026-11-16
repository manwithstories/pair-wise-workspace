"""Generate self-contained fMP4 fixture directories."""

from __future__ import annotations

import struct
from pathlib import Path

from .sidx import SidxEntry, encode_sidx

TIMESCALE = 128_000
SEGMENT_SECONDS = 4
SAMPLES_PER_SEGMENT = 320
SAMPLE_DURATION = TIMESCALE * SEGMENT_SECONDS // SAMPLES_PER_SEGMENT
SAMPLE_SIZE = 64
WRITERS = {"normal", "missing", "bad_counts", "truncated", "mixed"}


def generate_fixture(out_dir: str | Path, *, segments: int = 12, writer: str = "normal") -> Path:
    if segments <= 0:
        raise ValueError("segments must be positive")
    if segments > 0xFFFF:
        raise ValueError("sidx supports at most 65535 references")
    if writer not in WRITERS:
        raise ValueError(f"unknown writer {writer!r}; choose from {', '.join(sorted(WRITERS))}")
    target = Path(out_dir)
    target.mkdir(parents=True, exist_ok=True)
    sample_count = segments * SAMPLES_PER_SEGMENT
    ftyp = _box(b"ftyp", b"iso6\x00\x00\x00\x01iso6iso5dash")
    moov_stub = _moov([0] * segments, segments, sample_count)
    sidx_stub = _build_sidx(segments, [1] * segments, 0) if writer != "missing" else b""

    offsets: list[int] = []
    mdats: list[int] = []
    cursor = len(ftyp) + len(moov_stub) + len(sidx_stub)
    truncate_after = 5 if writer in {"truncated", "mixed"} else -1
    for index in range(segments):
        if index == truncate_after:
            cursor += 8
        offsets.append(cursor)
        moof = _moof(index + 1, index * SEGMENT_SECONDS * TIMESCALE)
        mdat = _box(b"mdat", bytes(SAMPLE_SIZE * SAMPLES_PER_SEGMENT))
        mdats.append(cursor + len(moof) + 8)
        cursor += len(moof) + len(mdat)
    file_size = cursor

    sizes = []
    for index, start in enumerate(offsets):
        end = offsets[index + 1] if index + 1 < len(offsets) else file_size
        sizes.append(end - start)
    earliest = 400 if writer == "mixed" else 0
    if writer == "mixed" and segments >= 7:
        sizes[5] -= 128
        sizes[6] += 128
    declared = segments - 1 if writer == "bad_counts" else None
    sidx = _build_sidx(segments, sizes, earliest, declared) if writer != "missing" else b""
    assert len(sidx) == len(sidx_stub)

    moov = _moov(mdats, segments, sample_count)
    assert len(moov) == len(moov_stub)
    chunks = [ftyp, moov, sidx]
    for index in range(segments):
        if index == truncate_after:
            chunks.append(struct.pack(">I4s", 4, b"moof"))
        chunks.append(_moof(index + 1, index * SEGMENT_SECONDS * TIMESCALE))
        chunks.append(_box(b"mdat", bytes(SAMPLE_SIZE * SAMPLES_PER_SEGMENT)))
    media = target / "media.mp4"
    media.write_bytes(b"".join(chunks))
    return media


def _build_sidx(
    segments: int,
    sizes: list[int],
    earliest: int,
    declared: int | None = None,
) -> bytes:
    entries = [
        SidxEntry(
            index=index,
            reference_type=0,
            referenced_size=sizes[index],
            subsegment_duration=SEGMENT_SECONDS * TIMESCALE,
        )
        for index in range(segments)
    ]
    return encode_sidx(
        reference_id=1,
        timescale=TIMESCALE,
        earliest_presentation_time=earliest,
        first_offset=0,
        entries=entries,
        declared_reference_count=declared,
    )


def _moov(chunk_offsets: list[int], chunks: int, sample_count: int) -> bytes:
    stts = _full_box(b"stts", 0, 0, struct.pack(">I", 1) + struct.pack(">II", sample_count, SAMPLE_DURATION))
    stsc = _full_box(b"stsc", 0, 0, struct.pack(">I", 1) + struct.pack(">III", 1, SAMPLES_PER_SEGMENT, 1))
    stsz = _full_box(b"stsz", 0, 0, struct.pack(">II", SAMPLE_SIZE, sample_count))
    stco = _full_box(b"stco", 0, 0, struct.pack(">I", chunks) + b"".join(struct.pack(">I", value) for value in chunk_offsets))
    sync = [index * SAMPLES_PER_SEGMENT + 1 for index in range(chunks)]
    stss = _full_box(b"stss", 0, 0, struct.pack(">I", len(sync)) + b"".join(struct.pack(">I", value) for value in sync))
    stbl = _box(b"stbl", stts + stsc + stsz + stco + stss)
    minf = _box(b"minf", stbl)
    mdhd = _full_box(
        b"mdhd", 0, 0,
        struct.pack(">IIIIHH", 0, 0, TIMESCALE, sample_count * SAMPLE_DURATION, 0x55C4, 0),
    )
    mdia = _box(b"mdia", mdhd + minf)
    tkhd = _full_box(
        b"tkhd", 0, 7,
        struct.pack(">IIII", 0, 0, 1, 0) + struct.pack(">I", sample_count * SAMPLE_DURATION)
        + bytes(8) + bytes(8) + bytes(36) + struct.pack(">II", 0, 0),
    )
    trak = _box(b"trak", tkhd + mdia)
    mvhd = _full_box(b"mvhd", 0, 0, struct.pack(">IIIII", 0, 0, TIMESCALE, sample_count * SAMPLE_DURATION, 0))
    return _box(b"moov", mvhd + trak)


def _moof(sequence: int, base_time: int) -> bytes:
    mfhd = _full_box(b"mfhd", 0, 0, struct.pack(">I", sequence))
    tfhd = _full_box(
        b"tfhd", 0, 0x020018,
        struct.pack(">III", 1, SAMPLE_DURATION, SAMPLE_SIZE),
    )
    tfdt = _full_box(b"tfdt", 1, 0, struct.pack(">Q", base_time))
    trun_stub = _full_box(b"trun", 0, 0x000001, struct.pack(">Ii", SAMPLES_PER_SEGMENT, 0))
    traf_stub = _box(b"traf", tfhd + tfdt + trun_stub)
    moof_stub = _box(b"moof", mfhd + traf_stub)
    data_offset = len(moof_stub) + 8
    trun = _full_box(b"trun", 0, 0x000001, struct.pack(">Ii", SAMPLES_PER_SEGMENT, data_offset))
    return _box(b"moof", mfhd + _box(b"traf", tfhd + tfdt + trun))


def _box(box_type: bytes, payload: bytes) -> bytes:
    return struct.pack(">I4s", len(payload) + 8, box_type) + payload


def _full_box(box_type: bytes, version: int, flags: int, payload: bytes) -> bytes:
    return _box(box_type, struct.pack(">I", (version << 24) | flags) + payload)
