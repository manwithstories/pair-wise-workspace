"""Per-track structure checks over an MP4 sample table."""

from __future__ import annotations

from ._mp4_core import BoxRecord, Mp4Error
from ._verify_records import CheckResult
from ._verify_tables import (
    _read_chunk_offsets,
    _read_ctts,
    _read_stss,
    _read_stsz,
    _read_stts,
    _u32,
)


def _track_id(data: bytes, trak: BoxRecord) -> str:
    tkhd = trak.child(b"tkhd")
    if tkhd is None or tkhd.payload_offset + 16 > len(data):
        return "?"
    return str(_u32(data, tkhd.payload_offset + 12))


def _verify_track(
    data: bytes, trak: BoxRecord, checks: list[CheckResult], chunk_spans: list[tuple[int, int]]
) -> dict[str, object] | None:
    tkhd = trak.child(b"tkhd")
    mdia = trak.child(b"mdia")
    if tkhd is None or mdia is None:
        checks.append(CheckResult("trak", False, "trak missing tkhd/mdia"))
        return None
    track_id = _u32(data, tkhd.payload_offset + 4 + 8)
    mdhd = mdia.child(b"mdhd")
    hdlr = mdia.child(b"hdlr")
    minf = mdia.child(b"minf")
    stbl = minf.child(b"stbl") if minf is not None else None
    if mdhd is None or stbl is None or hdlr is None:
        checks.append(CheckResult(f"track[{track_id}]", False, "mdhd/hdlr/stbl missing"))
        return None
    timescale = _u32(data, mdhd.payload_offset + 4 + 8)
    duration = _u32(data, mdhd.payload_offset + 4 + 12)
    handler = data[hdlr.payload_offset + 4 + 4 : hdlr.payload_offset + 4 + 8]

    ok = True
    detail: list[str] = []
    stsz = stbl.child(b"stsz")
    stts = stbl.child(b"stts")
    stsc = stbl.child(b"stsc")
    stco = stbl.child(b"stco") or stbl.child(b"co64")
    stss = stbl.child(b"stss")
    ctts = stbl.child(b"ctts")
    if stsz is None or stts is None or stsc is None or stco is None:
        checks.append(CheckResult(f"track[{track_id}]", False, "stbl missing a required table"))
        return None
    sizes = _read_stsz(data, stsz)
    stts_entries = _read_stts(data, stts)
    offsets = _read_chunk_offsets(data, stco)
    stsc_payload = stsc.payload_offset + 4
    stsc_count = _u32(data, stsc_payload)
    stsc_entries = [
        (
            _u32(data, stsc_payload + 4 + i * 12),
            _u32(data, stsc_payload + 8 + i * 12),
            _u32(data, stsc_payload + 12 + i * 12),
        )
        for i in range(stsc_count)
    ]
    decoded = sum(run for run, _ in stts_entries)
    if decoded != len(sizes):
        ok = False
        detail.append(f"stts covers {decoded} of {len(sizes)} samples")
    if duration <= 0:
        ok = False
        detail.append("mdhd duration is zero")

    expected_chunks = 0
    for sample_index, (first_chunk, per_chunk, _desc) in enumerate(stsc_entries):
        next_first = stsc_entries[sample_index + 1][0] if sample_index + 1 < len(stsc_entries) else len(offsets) + 1
        expected_chunks += (next_first - first_chunk) * per_chunk
    if expected_chunks != len(sizes):
        ok = False
        detail.append(f"stsc covers {expected_chunks} of {len(sizes)} samples")

    sample_cursor = 0
    dts: list[int] = []
    for run, delta in stts_entries:
        for _ in range(run):
            dts.append(sample_cursor)
            sample_cursor += delta
    if any(b <= a for a, b in zip(dts, dts[1:])):
        ok = False
        detail.append("stts produces a non-increasing decode time")

    if stss is not None:
        sync_indices = _read_stss(data, stss)
        if sync_indices != sorted(sync_indices) or any(i < 1 or i > len(sizes) for i in sync_indices):
            ok = False
            detail.append("stss indices are unsorted or out of range")

    if ctts is not None:
        version, ctts_entries = _read_ctts(data, ctts)
        composed = [0] * len(dts)
        position = 0
        for run, offset in ctts_entries:
            for _ in range(run):
                if position >= len(composed):
                    break
                composed[position] = dts[position] + offset
                position += 1
        if position != len(dts):
            ok = False
            detail.append(f"ctts covers {position} of {len(dts)} samples")
        if any(value < 0 for value in composed[:position]):
            ok = False
            detail.append("a composition time is negative")

    starts: list[int] = []
    step = 0
    if not stsc_entries:
        checks.append(CheckResult(f"track[{track_id}]", False, "stsc has no entries"))
        return None
    for chunk_index, chunk_offset in enumerate(offsets):
        while step + 1 < len(stsc_entries) and stsc_entries[step + 1][0] <= chunk_index + 1:
            step += 1
        per_chunk = stsc_entries[step][1]
        chunk_size = sum(sizes[chunk_index * per_chunk : (chunk_index + 1) * per_chunk])
        starts.append(chunk_offset)
        chunk_spans.append((chunk_offset, chunk_offset + chunk_size))

    if starts != sorted(starts):
        ok = False
        detail.append("chunk offsets are not ascending")

    checks.append(
        CheckResult(
            f"track[{track_id}]",
            ok,
            "; ".join(detail) if detail else f"{len(sizes)} samples, {len(offsets)} chunks",
        )
    )
    return {
        "track_id": track_id,
        "handler": handler.decode("latin-1").strip(),
        "timescale": timescale,
        "duration": duration,
        "sample_count": len(sizes),
        "chunk_count": len(offsets),
        "sync_sample_count": len(_read_stss(data, stss)) if stss is not None else len(sizes),
        "has_ctts": ctts is not None,
        "media_bytes": sum(sizes),
    }


__all__ = ["_track_id", "_verify_track"]
