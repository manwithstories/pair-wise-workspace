"""Sample table and index reconstruction.

This is where Matroska's block stream becomes an MP4 sample table:

  * **stsz** — every sample's byte size.
  * **stts** (decoding time table) — run-length DTS deltas.
  * **ctts** (composition time table) — PTS − DTS offsets, signed when the
    source stored frames out of presentation order.
  * **stsc** (sample-to-chunk) — which chunk owns which sample run.
  * **stco/co64** — absolute chunk offsets inside the output file.
  * **stss** (sync sample table) — keyframe indices, omitted when every sample
    is a keyframe (all-intra) and when a track has no keyframes at all.

Matroska stores blocks in decode order and puts the *presentation* time on each
block, so PTS comes straight from the timeline and DTS is the storage-order
clock. When the source already presents in order the two collapse and ctts is
omitted entirely.
"""

from __future__ import annotations

import numpy as np

from ._index_records import (
    DEFAULT_CHUNK_SAMPLES,
    INT32_MAX,
    Chunk,
    Sample,
    TrackIndex,
    _run_length,
)
from ._timing import TrackTimeline
from .matroska_model import Block


class IndexError_(ValueError):
    """Raised when a sample table cannot be built consistently."""

    def __init__(self, code: str, message: str, track_number: int | None = None) -> None:
        self.code = code
        self.track_number = track_number
        super().__init__(message)

    def as_dict(self) -> dict[str, object]:
        return {"code": self.code, "message": str(self), "track_number": self.track_number}


def build_track_index(
    timeline: TrackTimeline,
    blocks: list[Block],
    *,
    timescale: int | None = None,
    chunk_samples: int = DEFAULT_CHUNK_SAMPLES,
) -> TrackIndex:
    """Turn one track's blocks + rewrite plan into samples, DTS and tables."""
    if len(blocks) != timeline.sample_count:
        raise IndexError_(
            "sample_count_mismatch",
            f"track {timeline.track_number}: {len(blocks)} blocks vs "
            f"{timeline.sample_count} timeline samples",
            timeline.track_number,
        )
    if not blocks:
        raise IndexError_("track_empty", f"track {timeline.track_number} has no samples", timeline.track_number)

    scale = timescale or timeline.timescale
    if scale != timeline.timescale:
        raise IndexError_(
            "timescale_mismatch",
            f"track {timeline.track_number}: index timescale {scale} != timeline {timeline.timescale}",
            timeline.track_number,
        )

    pts = np.asarray(timeline.rewritten_pts, dtype=np.int64)
    if pts.size and int(pts[0]) < 0:
        raise IndexError_(
            "negative_pts",
            f"track {timeline.track_number} starts at negative PTS {int(pts[0])}",
            timeline.track_number,
        )

    notes: list[str] = []
    # DTS is decided by the timestamp layer (storage order, strictly
    # increasing); the composition offset is whatever makes PTS come out right.
    dts = np.asarray(timeline.rewritten_dts, dtype=np.int64)
    if dts.size != pts.size:
        raise IndexError_(
            "dts_count_mismatch",
            f"track {timeline.track_number}: {dts.size} DTS vs {pts.size} PTS",
            timeline.track_number,
        )
    if int(dts[0]) < 0:
        raise IndexError_(
            "negative_dts",
            f"track {timeline.track_number} starts at negative DTS {int(dts[0])}",
            timeline.track_number,
        )
    ctts_offsets = pts - dts
    if bool(np.any(np.abs(ctts_offsets) > INT32_MAX)):
        raise IndexError_(
            "ctts_overflow",
            f"track {timeline.track_number} composition offset exceeds 32 bits",
            timeline.track_number,
        )
    if bool(np.any(ctts_offsets != 0)):
        notes.append("presentation order differs from storage order; composition offsets carry PTS")

    if dts.size >= 2 and not bool(np.all(np.diff(dts) > 0)):
        raise IndexError_(
            "non_monotonic_dts",
            f"track {timeline.track_number} DTS is not strictly increasing",
            timeline.track_number,
        )

    durations = np.asarray(timeline.durations, dtype=np.int64)
    if durations.size != pts.size:
        raise IndexError_("duration_count_mismatch", "timeline durations do not match samples", timeline.track_number)
    if bool(np.any(durations <= 0)):
        bad = int(np.count_nonzero(durations <= 0))
        raise IndexError_(
            "non_positive_duration",
            f"track {timeline.track_number} has {bad} non-positive sample duration(s)",
            timeline.track_number,
        )

    samples: list[Sample] = []
    for index, block in enumerate(blocks):
        if len(block.frames) != 1:
            raise IndexError_(
                "laced_block_unsupported",
                f"track {timeline.track_number} block {index} has {len(block.frames)} laced frames; "
                "split the block or disable lacing",
                timeline.track_number,
            )
        frame = block.frames[0]
        samples.append(
            Sample(
                track_number=timeline.track_number,
                index=index,
                size=frame.size,
                source_offset=frame.offset,
                pts=int(pts[index]),
                dts=int(dts[index]),
                ctts=int(ctts_offsets[index]),
                sync=block.keyframe,
            )
        )
    if any(sample.size <= 0 for sample in samples):
        raise IndexError_(
            "empty_sample", f"track {timeline.track_number} has a zero-length sample", timeline.track_number
        )

    # stts: delta of consecutive DTS, last sample uses its own duration.
    deltas: list[int] = []
    for index in range(len(samples)):
        if index + 1 < len(samples):
            deltas.append(samples[index + 1].dts - samples[index].dts)
        else:
            deltas.append(int(durations[index]))
    if any(delta <= 0 for delta in deltas):
        raise IndexError_("non_positive_stts", "decoding delta of zero", timeline.track_number)
    stts = _run_length(deltas)

    ctts: list[tuple[int, int]] = []
    ctts_version = 0
    if any(sample.ctts != 0 for sample in samples):
        ctts = _run_length([sample.ctts for sample in samples])
        ctts_version = 1 if any(offset < 0 for _, offset in ctts) else 0

    media_duration = int(samples[-1].dts + deltas[-1] - samples[0].dts)
    sync_indices = [s.index + 1 for s in samples if s.sync]  # 1-based
    if not sync_indices:
        notes.append("track has no keyframes; stss omitted so all samples decode independently")
    elif len(sync_indices) == len(samples):
        sync_indices = []  # all-intra: omit stss per spec
        notes.append("all samples are sync samples; stss omitted")

    chunks = _plan_chunks(samples, max_samples=max(1, chunk_samples))
    return TrackIndex(
        track_number=timeline.track_number,
        timescale=scale,
        samples=samples,
        chunks=chunks,
        stts=stts,
        ctts=ctts,
        stss=sync_indices,
        media_duration=media_duration,
        has_ctts=bool(ctts),
        ctts_version=ctts_version,
        notes=notes,
    )


def _plan_chunks(samples: list[Sample], *, max_samples: int = DEFAULT_CHUNK_SAMPLES) -> list[Chunk]:
    chunks: list[Chunk] = []
    for start in range(0, len(samples), max_samples):
        group = list(range(start, min(start + max_samples, len(samples))))
        chunks.append(Chunk(track_number=samples[0].track_number, sample_indices=group))
    return chunks


def interleave_chunks(indexes: list[TrackIndex]) -> list[tuple[TrackIndex, Chunk]]:
    """Order chunks round-robin across tracks for a playback-friendly mdat."""
    order: list[tuple[TrackIndex, Chunk]] = []
    longest = max((len(index.chunks) for index in indexes), default=0)
    for round_index in range(longest):
        for index in indexes:
            if round_index < len(index.chunks):
                order.append((index, index.chunks[round_index]))
    return order


def lay_out_media(
    indexes: list[TrackIndex], *, mdat_data_offset: int
) -> list[tuple[TrackIndex, Chunk]]:
    """Assign each sample's output offset following the interleaved plan."""
    order = interleave_chunks(indexes)
    cursor = mdat_data_offset
    for index, chunk in order:
        chunk.offset = cursor
        for sample_index in chunk.sample_indices:
            sample = index.samples[sample_index]
            sample.output_offset = cursor
            cursor += sample.size
    return order


def total_media_bytes(indexes: list[TrackIndex]) -> int:
    return sum(sample.size for index in indexes for sample in index.samples)


def sample_source_ranges(index: TrackIndex) -> list[tuple[int, int, int]]:
    """``(source_offset, output_offset, size)`` per sample — the copy plan."""
    return [(s.source_offset, s.output_offset, s.size) for s in index.samples]


__all__ = [
    "Chunk",
    "DEFAULT_CHUNK_SAMPLES",
    "IndexError_",
    "Sample",
    "TrackIndex",
    "build_track_index",
    "interleave_chunks",
    "lay_out_media",
    "sample_source_ranges",
    "total_media_bytes",
]
