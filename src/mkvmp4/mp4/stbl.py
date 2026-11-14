"""Generate sample table boxes from track samples and chunk layout."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Iterable, Sequence

from .boxes import BoxBuffer, u16, u32, u64, full_box


@dataclass(slots=True)
class SampleMeta:
    offset: int
    size: int
    dts: int
    cts: int
    duration: int
    keyframe: bool
    cluster_index: int

    @property
    def cts_offset(self) -> int:
        return self.cts - self.dts


def _runs(values: Iterable[int]) -> list[tuple[int, int]]:
    runs: list[tuple[int, int]] = []
    for value in values:
        if runs and runs[-1][0] == value:
            runs[-1] = (value, runs[-1][1] + 1)
        else:
            runs.append((value, 1))
    return runs


def stts_box(samples: Sequence[SampleMeta]) -> bytes:
    runs = _runs(sample.duration for sample in samples)
    return full_box("stts", 0, 0, u32(len(runs)) + b"".join(
        u32(count) + u32(duration) for duration, count in runs))


def ctts_box(samples: Sequence[SampleMeta]) -> bytes | None:
    offsets = [sample.cts_offset for sample in samples]
    if not any(offsets):
        return None
    version = 1 if any(offset < 0 for offset in offsets) else 0
    runs = _runs(offsets)
    payload = u32(len(runs))
    for offset, count in runs:
        payload += u32(count) + (struct_i32(offset) if version else u32(offset))
    return full_box("ctts", version, 0, payload)


def struct_i32(value: int) -> bytes:
    return value.to_bytes(4, "big", signed=True)


def stsc_box(samples: Sequence[SampleMeta]) -> bytes:
    counts: list[int] = []
    last_cluster: int | None = None
    for sample in samples:
        if counts and last_cluster == sample.cluster_index:
            counts[-1] += 1
        else:
            counts.append(1)
        last_cluster = sample.cluster_index
    entries: list[tuple[int, int, int]] = []
    for index, count in enumerate(counts, start=1):
        if entries and entries[-1][1] == count:
            continue
        entries.append((index, count, 1))
    return full_box("stsc", 0, 0, u32(len(entries)) + b"".join(
        u32(first) + u32(count) + u32(desc) for first, count, desc in entries))


def stsz_box(samples: Sequence[SampleMeta]) -> bytes:
    sizes = [sample.size for sample in samples]
    if sizes and all(size == sizes[0] for size in sizes):
        return full_box("stsz", 0, 0, u32(sizes[0]) + u32(len(sizes)))
    return full_box("stsz", 0, 0, u32(0) + u32(len(sizes)) +
                    b"".join(u32(size) for size in sizes))


def chunk_offsets(samples: Sequence[SampleMeta], cluster_chunks: dict[int, int]) -> list[int]:
    offsets: dict[int, int] = {}
    for sample in samples:
        offsets.setdefault(sample.cluster_index, sample.offset)
    return [offsets[index] for index in sorted(offsets, key=lambda item: cluster_chunks[item])]


def stco_box(offsets: Sequence[int]) -> bytes:
    if not offsets or max(offsets) <= 0xFFFFFFFF:
        return full_box("stco", 0, 0, u32(len(offsets)) +
                        b"".join(u32(offset) for offset in offsets))
    return full_box("co64", 0, 0, u32(len(offsets)) +
                    b"".join(u64(offset) for offset in offsets))


def stss_box(samples: Sequence[SampleMeta]) -> bytes | None:
    if all(sample.keyframe for sample in samples):
        return None
    sync = [index for index, sample in enumerate(samples, start=1) if sample.keyframe]
    return full_box("stss", 0, 0, u32(len(sync)) + b"".join(u32(index) for index in sync))


def build_stbl(samples: Sequence[SampleMeta], entry: bytes,
               cluster_chunks: dict[int, int]) -> bytes:
    out = BoxBuffer()
    from .boxes import sample_description

    out.write(sample_description(entry))
    out.write(stts_box(samples))
    ctts = ctts_box(samples)
    if ctts is not None:
        out.write(ctts)
    out.write(stsc_box(samples))
    out.write(stsz_box(samples))
    out.write(stco_box(chunk_offsets(samples, cluster_chunks)))
    stss = stss_box(samples)
    if stss is not None:
        out.write(stss)
    from .boxes import box

    return box("stbl", out.getvalue())

