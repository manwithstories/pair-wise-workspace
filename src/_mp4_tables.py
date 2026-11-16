"""Sample-table boxes (stbl children) shared by the movie builder."""

from __future__ import annotations

import struct

from ._mp4_core import Box, Mp4Error, box, full_box
from ._mp4_samples import (
    AudioEntry,
    VisualEntry,
    avc1_entry,
    hvc1_entry,
    mp4a_entry,
    opus_entry,
    stsd_box,
    tx3g_entry,
)
from .index_rebuilder import TrackIndex
from .track_mapper import MappedTrack

MAX_32BIT_OFFSET = 0xFFFFFFFF


def _stts_from_table(index: TrackIndex) -> Box:
    payload = len(index.stts).to_bytes(4, "big")
    for count, delta in index.stts:
        payload += struct.pack(">II", count, delta)
    return full_box(b"stts", 0, 0, payload)


def _ctts_from_table(index: TrackIndex) -> Box:
    entries = (len(index.ctts)).to_bytes(4, "big")
    for count, offset in index.ctts:
        entries += struct.pack(">Ii", count, offset)
    return full_box(b"ctts", index.ctts_version, 0, entries)


def _stsc_from_chunks(index: TrackIndex) -> Box:
    entries: list[tuple[int, int, int]] = []
    for chunk_number, chunk in enumerate(index.chunks, start=1):
        signature = (len(chunk.sample_indices), 1)
        if entries and entries[-1][1:] == signature:
            continue
        entries.append((chunk_number, *signature))
    payload = len(entries).to_bytes(4, "big")
    for first_chunk, per_chunk, desc_index in entries:
        payload += struct.pack(">III", first_chunk, per_chunk, desc_index)
    return full_box(b"stsc", 0, 0, payload)


def _stsz(index: TrackIndex) -> Box:
    sizes = b"".join(struct.pack(">I", sample.size) for sample in index.samples)
    payload = struct.pack(">II", 0, len(index.samples)) + sizes
    return full_box(b"stsz", 0, 0, payload)


def _stco_or_co64(index: TrackIndex, *, offsets: list[int] | None = None) -> Box:
    values = offsets if offsets is not None else [chunk.offset for chunk in index.chunks]
    if all(value <= MAX_32BIT_OFFSET for value in values):
        payload = len(values).to_bytes(4, "big") + b"".join(struct.pack(">I", v) for v in values)
        return full_box(b"stco", 0, 0, payload)
    payload = len(values).to_bytes(4, "big") + b"".join(struct.pack(">Q", v) for v in values)
    return full_box(b"co64", 0, 0, payload)


def _stss(index: TrackIndex) -> Box | None:
    if not index.stss:
        return None
    payload = len(index.stss).to_bytes(4, "big") + b"".join(struct.pack(">I", i) for i in index.stss)
    return full_box(b"stss", 0, 0, payload)


def _visual_entry(mapped: MappedTrack) -> Box:
    if not mapped.width or not mapped.height:
        raise Mp4Error("missing_dimensions", f"track {mapped.source_number} video has no dimensions")
    entry = VisualEntry(
        width=mapped.width,
        height=mapped.height,
        codec_private=mapped.codec_private or b"",
    )
    if mapped.codec_id.startswith("V_MPEGH"):
        return hvc1_entry(entry)
    return avc1_entry(entry)


def _audio_entry(mapped: MappedTrack) -> Box:
    audio = AudioEntry(
        channel_count=mapped.channels or 2,
        sample_rate=int(mapped.sample_rate or 48_000),
        codec_private=mapped.codec_private or b"",
    )
    if mapped.codec_id.upper().startswith("A_OPUS"):
        return opus_entry(audio)
    if mapped.codec_id.upper().startswith("A_FLAC"):
        # FLAC in MP4 wraps dfLa; we only have the fLaC stream marker, so this
        # is rejected upstream rather than written as a bogus sample entry.
        raise Mp4Error("unsupported_codec", "A_FLAC is mapped in CODEC_MAP but not writable yet")
    return mp4a_entry(audio)


def sample_entry_for(mapped: MappedTrack) -> Box:
    if mapped.track_type == "video":
        return _visual_entry(mapped)
    if mapped.track_type == "audio":
        return _audio_entry(mapped)
    return tx3g_entry()


def build_stbl(index: TrackIndex, mapped: MappedTrack, *, chunk_offsets: list[int] | None = None) -> Box:
    children = [stsd_box([sample_entry_for(mapped)])]
    children.append(_stts_from_table(index))
    if index.has_ctts:
        children.append(_ctts_from_table(index))
    children.append(_stsc_from_chunks(index))
    children.append(_stsz(index))
    children.append(_stco_or_co64(index, offsets=chunk_offsets))
    stss = _stss(index)
    if stss is not None:
        children.append(stss)
    return box(b"stbl", children=children)


def build_stbl_placeholder(index: TrackIndex, mapped: MappedTrack) -> Box:
    """stbl with zero offsets, used to measure the moov size."""
    return build_stbl(index, mapped, chunk_offsets=[0] * len(index.chunks))
