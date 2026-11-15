"""Movie fragment parsing: mfhd, tfhd, tfdt, trun."""
from __future__ import annotations

from dataclasses import dataclass, field

TFHD_BASE_DATA_OFFSET = 0x000001
TFHD_SAMPLE_DESCRIPTION = 0x000002
TFHD_DEFAULT_DURATION = 0x000008
TFHD_DEFAULT_SIZE = 0x000010
TFHD_DEFAULT_FLAGS = 0x000020
TFHD_DEFAULT_BASE_IS_MOOF = 0x020000

TRUN_DATA_OFFSET = 0x000001
TRUN_FIRST_FLAGS = 0x000004
TRUN_SAMPLE_DURATION = 0x000100
TRUN_SAMPLE_SIZE = 0x000200
TRUN_SAMPLE_FLAGS = 0x000400
TRUN_SAMPLE_CTO = 0x000800


@dataclass
class TrunSample:
    duration: int
    size: int
    flags: int
    cto: int


@dataclass
class Trun:
    data_offset: int | None
    first_sample_flags: int | None
    samples: list = field(default_factory=list)


@dataclass
class Traf:
    track_id: int
    base_data_offset: int | None = None
    default_sample_duration: int = 0
    default_sample_size: int = 0
    default_sample_flags: int = 0
    default_base_is_moof: bool = False
    base_media_decode_time: int | None = None
    truns: list = field(default_factory=list)

    @property
    def total_duration(self):
        return sum(s.duration for t in self.truns for s in t.samples)

    def data_extent(self, moof_start):
        """Absolute (offset, length) of this traf's sample data."""
        base = self.base_data_offset
        if base is None:
            base = moof_start  # default-base-is-moof or implicit
        start = None
        end = 0
        for trun in self.truns:
            off = base + (trun.data_offset or 0)
            size = sum(s.size for s in trun.samples)
            start = off if start is None else min(start, off)
            end = max(end, off + size)
        if start is None:
            return 0, 0
        return start, end - start


@dataclass
class Moof:
    start: int
    size: int
    sequence_number: int
    trafs: list = field(default_factory=list)

    def by_track(self):
        """Group trafs by track_id (multi-track fragments)."""
        grouped = {}
        for traf in self.trafs:
            grouped.setdefault(traf.track_id, traf)
        return grouped


def _parse_tfhd(reader, box):
    _version, flags, off = reader.fullbox(box)
    (track_id,) = reader.unpack(">I", off)
    off += 4
    traf = Traf(track_id)
    if flags & TFHD_BASE_DATA_OFFSET:
        traf.base_data_offset = reader.unpack(">Q", off)[0]
        off += 8
    if flags & TFHD_SAMPLE_DESCRIPTION:
        off += 4
    if flags & TFHD_DEFAULT_DURATION:
        traf.default_sample_duration = reader.unpack(">I", off)[0]
        off += 4
    if flags & TFHD_DEFAULT_SIZE:
        traf.default_sample_size = reader.unpack(">I", off)[0]
        off += 4
    if flags & TFHD_DEFAULT_FLAGS:
        traf.default_sample_flags = reader.unpack(">I", off)[0]
        off += 4
    traf.default_base_is_moof = bool(flags & TFHD_DEFAULT_BASE_IS_MOOF)
    return traf


def _parse_trun(reader, box, traf):
    version, flags, off = reader.fullbox(box)
    (count,) = reader.unpack(">I", off)
    off += 4
    data_offset = None
    first_flags = None
    if flags & TRUN_DATA_OFFSET:
        (data_offset,) = reader.unpack(">i", off)  # signed
        off += 4
    if flags & TRUN_FIRST_FLAGS:
        (first_flags,) = reader.unpack(">I", off)
        off += 4
    samples = []
    for _ in range(count):
        dur = traf.default_sample_duration
        size = traf.default_sample_size
        flg = traf.default_sample_flags
        cto = 0
        if flags & TRUN_SAMPLE_DURATION:
            dur = reader.unpack(">I", off)[0]
            off += 4
        if flags & TRUN_SAMPLE_SIZE:
            size = reader.unpack(">I", off)[0]
            off += 4
        if flags & TRUN_SAMPLE_FLAGS:
            flg = reader.unpack(">I", off)[0]
            off += 4
        if flags & TRUN_SAMPLE_CTO:
            cto = reader.unpack(">i" if version == 1 else ">I", off)[0]
            off += 4
        samples.append(TrunSample(dur, size, flg, cto))
    traf.truns.append(Trun(data_offset, first_flags, samples))


def parse_moof(reader, box):
    seq = 0
    trafs = []
    for child in reader.children(box):
        if child.type == "mfhd":
            _v, _f, off = reader.fullbox(child)
            seq = reader.unpack(">I", off)[0]
        elif child.type == "traf":
            traf = None
            for sub in reader.children(child):
                if sub.type == "tfhd":
                    traf = _parse_tfhd(reader, sub)
                elif sub.type == "tfdt" and traf is not None:
                    version, _f, off = reader.fullbox(sub)
                    fmt = ">Q" if version == 1 else ">I"
                    traf.base_media_decode_time = reader.unpack(fmt, off)[0]
                elif sub.type == "trun" and traf is not None:
                    _parse_trun(reader, sub, traf)
            if traf is not None:
                trafs.append(traf)
    return Moof(box.start, box.size, seq, trafs)

