"""Recompute fragment boundaries from moof metadata and stbl samples."""
from __future__ import annotations

from dataclasses import dataclass, field

from .boxes import Box
from .reader import ByteCursor, StructureError
from .stbl import Sample

TFHD_FLAG_BASE_DATA_OFFSET = 0x000001
TFHD_FLAG_DEFAULT_BASE_IS_MOOF = 0x020000
TRUN_FLAG_DATA_OFFSET = 0x000001
TRUN_FLAG_SAMPLE_DURATION = 0x000100
TRUN_FLAG_SAMPLE_SIZE = 0x000200


@dataclass
class FragmentInfo:
    sequence: int
    moof_offset: int
    moof_size: int
    mdat_offset: int           # absolute offset of the mdat box header
    mdat_size: int             # total mdat box size including header
    base_decode_time: int
    sample_count: int
    data_offset: int           # trun data offset resolved to absolute file offset

    @property
    def reference_size(self) -> int:
        return self.moof_size + self.mdat_size


@dataclass
class RebuiltEntry:
    byte_start: int
    reference_size: int
    subsegment_duration: int


@dataclass
class RebuiltIndex:
    first_offset: int          # sidx end -> first fragment start
    entries: list[RebuiltEntry] = field(default_factory=list)

    @property
    def boundaries(self) -> list[int]:
        return [entry.byte_start for entry in self.entries]


def _parse_tfhd(cursor: ByteCursor, box: Box) -> tuple[int, int | None, bool]:
    pos = box.payload_offset
    flags = cursor.u24(pos + 1)
    track_id = cursor.u32(pos + 4)
    pos += 8
    base_data_offset = None
    if flags & TFHD_FLAG_BASE_DATA_OFFSET:
        base_data_offset = cursor.u64(pos)
    default_base_is_moof = bool(flags & TFHD_FLAG_DEFAULT_BASE_IS_MOOF)
    return track_id, base_data_offset, default_base_is_moof


def _parse_tfdt(cursor: ByteCursor, box: Box) -> int:
    version = cursor.u8(box.payload_offset)
    if version == 1:
        return cursor.u64(box.payload_offset + 4)
    return cursor.u32(box.payload_offset + 4)


def _parse_trun(cursor: ByteCursor, box: Box) -> tuple[int, int | None]:
    """Return (sample_count, data_offset)."""
    pos = box.payload_offset
    flags = cursor.u24(pos + 1)
    sample_count = cursor.u32(pos + 4)
    pos += 8
    data_offset = None
    if flags & TRUN_FLAG_DATA_OFFSET:
        data_offset = cursor.i32(pos)
        pos += 4
    if flags & 0x000004:  # first-sample-flags present
        pos += 4
    entry_size = (4 * bool(flags & TRUN_FLAG_SAMPLE_DURATION)
                  + 4 * bool(flags & TRUN_FLAG_SAMPLE_SIZE)
                  + 4 * bool(flags & 0x000400)
                  + 4 * bool(flags & 0x000800))
    if pos + entry_size * sample_count > box.end:
        raise StructureError("trun entries overrun box", box.offset)
    return sample_count, data_offset


def parse_fragment(cursor: ByteCursor, moof: Box, mdat: Box) -> FragmentInfo:
    """Extract boundary-relevant fields from one moof/mdat pair."""
    mfhd = moof.find_one("mfhd")
    traf = moof.find_one("traf")
    if mfhd is None or traf is None:
        raise StructureError("moof missing mfhd/traf", moof.offset)
    tfhd = traf.find_one("tfhd")
    tfdt = traf.find_one("tfdt")
    trun = traf.find_one("trun")
    if tfhd is None or tfdt is None or trun is None:
        raise StructureError("traf missing tfhd/tfdt/trun", moof.offset)
    sequence = cursor.u32(mfhd.payload_offset + 4)
    _, base_data_offset, default_is_moof = _parse_tfhd(cursor, tfhd)
    base_decode_time = _parse_tfdt(cursor, tfdt)
    sample_count, trun_data_offset = _parse_trun(cursor, trun)
    if trun_data_offset is None:
        raise StructureError("trun missing data_offset", moof.offset)
    if base_data_offset is not None:
        base = base_data_offset
    elif default_is_moof:
        base = moof.offset
    else:
        base = moof.offset
    return FragmentInfo(
        sequence=sequence,
        moof_offset=moof.offset,
        moof_size=moof.size,
        mdat_offset=mdat.offset,
        mdat_size=mdat.size,
        base_decode_time=base_decode_time,
        sample_count=sample_count,
        data_offset=base + trun_data_offset,
    )


def collect_fragments(cursor: ByteCursor, root: Box) -> list[FragmentInfo]:
    """Pair each top-level moof with the immediately following mdat."""
    fragments: list[FragmentInfo] = []
    children = root.children
    for idx, box in enumerate(children):
        if box.type != "moof":
            continue
        if idx + 1 >= len(children) or children[idx + 1].type != "mdat":
            raise StructureError("moof not followed by mdat", box.offset)
        fragments.append(parse_fragment(cursor, box, children[idx + 1]))
    return fragments


def _sample_index_for_dts(samples: list[Sample], dts: int) -> int:
    for sample in samples:
        if sample.dts == dts:
            return sample.index
    raise StructureError(f"no stbl sample at decode time {dts}", 0)


def _deltas(samples: list[Sample]) -> list[int]:
    """Per-sample decode deltas; the last sample reuses the previous delta."""
    deltas = [samples[i + 1].dts - samples[i].dts
              for i in range(len(samples) - 1)]
    deltas.append(deltas[-1] if deltas else 0)
    return deltas


def rebuild_index(fragments: list[FragmentInfo], samples: list[Sample],
                  sidx_end: int) -> RebuiltIndex:
    """Recompute first_offset, reference_size and subsegment_duration.

    Fragment start derives from moof position (tfhd/tfdt/trun + stbl);
    durations come from stbl decode deltas for the fragment's samples.
    """
    deltas = _deltas(samples)
    entries: list[RebuiltEntry] = []
    for frag in fragments:
        start = _sample_index_for_dts(samples, frag.base_decode_time)
        end = start + frag.sample_count
        if end > len(samples):
            raise StructureError("fragment sample range exceeds stbl",
                                 frag.moof_offset)
        duration = sum(deltas[start:end])
        entries.append(RebuiltEntry(
            byte_start=frag.moof_offset,
            reference_size=frag.reference_size,
            subsegment_duration=duration,
        ))
    first_offset = entries[0].byte_start - sidx_end if entries else 0
    return RebuiltIndex(first_offset=first_offset, entries=entries)
