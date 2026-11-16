"""Sample-table record types: samples, chunks, per-track index tables."""

from __future__ import annotations

from dataclasses import dataclass, field

DEFAULT_CHUNK_SAMPLES = 10
INT32_MAX = (1 << 31) - 1


@dataclass
class Sample:
    """One MP4 sample, pointing at its bytes in the output mdat."""

    track_number: int
    index: int
    size: int
    source_offset: int
    pts: int
    dts: int
    ctts: int
    sync: bool
    output_offset: int = 0

    def as_dict(self) -> dict[str, object]:
        return {
            "index": self.index,
            "size": self.size,
            "pts": self.pts,
            "dts": self.dts,
            "ctts": self.ctts,
            "sync": self.sync,
            "source_offset": self.source_offset,
            "output_offset": self.output_offset,
        }


@dataclass
class Chunk:
    """A contiguous run of samples of one track in the output file."""

    track_number: int
    sample_indices: list[int]
    offset: int = 0

    @property
    def sample_count(self) -> int:
        return len(self.sample_indices)


@dataclass
class TrackIndex:
    """Everything the moov builder needs for one track's stbl."""

    track_number: int
    timescale: int
    samples: list[Sample]
    chunks: list[Chunk]
    stts: list[tuple[int, int]] = field(default_factory=list)
    ctts: list[tuple[int, int]] = field(default_factory=list)
    stss: list[int] = field(default_factory=list)
    media_duration: int = 0
    has_ctts: bool = False
    ctts_version: int = 0
    notes: list[str] = field(default_factory=list)

    @property
    def sample_count(self) -> int:
        return len(self.samples)

    def as_dict(self) -> dict[str, object]:
        return {
            "track_number": self.track_number,
            "timescale": self.timescale,
            "sample_count": len(self.samples),
            "chunk_count": len(self.chunks),
            "media_duration": self.media_duration,
            "has_ctts": self.has_ctts,
            "ctts_version": self.ctts_version if self.has_ctts else None,
            "sync_samples": len(self.stss) if self.stss else len(self.samples),
            "stts_entries": len(self.stts),
            "notes": list(self.notes),
        }


def _run_length(pairs: list[tuple[int, int]]) -> list[tuple[int, int]]:
    """Collapse ``(delta, ...)`` sequences into ``(count, value)`` runs."""
    if not pairs:
        return []
    out: list[tuple[int, int]] = []
    count, value = 1, pairs[0]
    for item in pairs[1:]:
        if item == value:
            count += 1
        else:
            out.append((count, value))
            count, value = 1, item
    out.append((count, value))
    return out


__all__ = ["Chunk", "DEFAULT_CHUNK_SAMPLES", "INT32_MAX", "Sample", "TrackIndex"]
