"""Remux driver: Matroska in, MP4 out, with an auditable report.

The write path is transactional: media bytes are streamed into a temporary file
next to the destination, the header is written only after every sample has been
copied and verified against its source range, and the temporary file is renamed
on success. Any failure removes the temporary file, so a partial MP4 is never
left behind.
"""

from __future__ import annotations

import hashlib
import os
import tempfile
from dataclasses import dataclass, field
from pathlib import Path
from typing import BinaryIO

from .index_rebuilder import TrackIndex, build_track_index
from .matroska_model import MatroskaDocument
from .mp4_boxes import (
    MovieLayout,
    MovieSpec,
    build_layout,
    mdat_header,
    serialize,
)
from .timestamp import RewritePlan, TimingConfig, TimestampError, plan_rewrite
from .track_mapper import TrackMapping, TrackMappingError, build_track_mapping


class RemuxError(Exception):
    """A remux failed; ``code`` maps to the CLI exit-code table."""

    def __init__(self, code: str, message: str, *, details: dict | None = None) -> None:
        self.code = code
        self.details = details or {}
        super().__init__(message)

    def as_dict(self) -> dict[str, object]:
        return {"code": self.code, "message": str(self), "details": self.details}


@dataclass
class RemuxOptions:
    timing: TimingConfig = field(default_factory=TimingConfig)
    include_subtitles: bool = True
    #: Default is to stop rather than write a partial film: an unmappable
    #: CodecID means the caller's intent (all tracks carried over) cannot be met.
    strict_codecs: bool = True
    verify_copy: bool = True
    chunk_samples: int = 10


@dataclass
class RemuxResult:
    output_path: Path
    size: int
    sha256: str
    mapping: TrackMapping
    plan: RewritePlan
    indexes: list[TrackIndex]
    layout: MovieLayout
    copied_bytes: int
    warnings: list[dict[str, object]] = field(default_factory=list)

    def as_dict(self) -> dict[str, object]:
        return {
            "output": str(self.output_path),
            "size": self.size,
            "sha256": self.sha256,
            "copied_bytes": self.copied_bytes,
            "track_mapping": self.mapping.summary(),
            "timestamps": self.plan.summary(),
            "indexes": [index.as_dict() for index in self.indexes],
            "box_count": self.layout.box_count,
            "mdat_offset": self.layout.mdat_offset,
            "mdat_size": self.layout.mdat_size,
            "warnings": list(self.warnings),
        }


def _collect_indexes(
    document: MatroskaDocument,
    plan: RewritePlan,
    mapping: TrackMapping,
    *,
    chunk_samples: int,
) -> tuple[list[TrackIndex], list[object]]:
    indexes: list[TrackIndex] = []
    mapped = []
    for item in mapping.remuxable:
        track = document.track_by_number(item.source_number)
        timeline = plan.timeline(item.source_number)
        blocks = document.blocks_for_track(track)
        try:
            index = build_track_index(timeline, blocks, chunk_samples=chunk_samples)
        except Exception as exc:  # IndexError_ and friends
            code = getattr(exc, "code", "index_build_failed")
            raise RemuxError(
                "timestamp_index_failed",
                f"track {item.source_number}: {exc}",
                details={"code": code, "track_number": item.source_number},
            ) from exc
        indexes.append(index)
        mapped.append(item)
    return indexes, mapped


def _copy_samples(
    source: BinaryIO,
    sink: BinaryIO,
    indexes: list[TrackIndex],
    *,
    verify_copy: bool,
) -> int:
    """Copy every sample's bytes to its assigned output offset, in chunk order.

    With ``verify_copy`` each sample is read, written, and then read back from
    the sink at the exact offset it was written to; a mismatch means the layout
    and the write path disagree and the whole remux is abandoned.
    """
    copied = 0
    samples = [sample for index in indexes for sample in index.samples]
    ordered = sorted(samples, key=lambda s: s.output_offset)
    for sample in ordered:
        source.seek(sample.source_offset)
        payload = source.read(sample.size)
        if len(payload) != sample.size:
            raise RemuxError(
                "source_truncated",
                f"sample {sample.index} of track {sample.track_number} needs "
                f"{sample.size} bytes at {sample.source_offset}, got {len(payload)}",
                details={"track_number": sample.track_number, "index": sample.index},
            )
        sink.write(payload)
        copied += len(payload)
        if verify_copy:
            _verify_sample_write(sink, sample.output_offset, payload, sample)
    return copied


def _verify_sample_write(sink: BinaryIO, offset: int, expected: bytes, sample) -> None:
    """Read the sample back from the output at its declared offset."""
    position = sink.tell()
    sink.flush()
    sink.seek(offset)
    actual = sink.read(len(expected))
    sink.seek(position)
    if actual != expected:
        raise RemuxError(
            "copy_mismatch",
            f"track {sample.track_number} sample {sample.index} did not land at "
            f"offset {offset}",
            details={"track_number": sample.track_number, "index": sample.index},
        )


def remux(
    document: MatroskaDocument,
    source_path: Path,
    output_path: Path,
    *,
    options: RemuxOptions | None = None,
) -> RemuxResult:
    """Convert ``document`` to an MP4 at ``output_path``.

    ``document.path`` (or ``source_path``) must be readable: samples are copied
    from the source byte ranges recorded during parsing, never re-decoded.
    """
    options = options or RemuxOptions()
    output_path = Path(output_path)
    source_path = Path(source_path)
    if not source_path.is_file():
        raise RemuxError("input_missing", f"source file {source_path} does not exist")

    try:
        mapping = build_track_mapping(
            document,
            include_subtitles=options.include_subtitles,
            strict_codecs=options.strict_codecs,
        )
    except TrackMappingError as exc:
        raise RemuxError(
            "track_mapping_failed",
            str(exc),
            details=exc.as_dict(),
        ) from exc

    try:
        plan = plan_rewrite(document, config=options.timing)
    except TimestampError as exc:
        raise RemuxError(
            "timestamp_rewrite_failed",
            str(exc),
            details=exc.as_dict(),
        ) from exc

    indexes, mapped = _collect_indexes(
        document, plan, mapping, chunk_samples=options.chunk_samples
    )
    spec = MovieSpec(indexes=indexes, mapped=mapped)  # type: ignore[arg-type]
    layout = build_layout(spec)

    output_path.parent.mkdir(parents=True, exist_ok=True)
    temp_fd, temp_name = tempfile.mkstemp(
        prefix=f".{output_path.name}.", suffix=".tmp", dir=output_path.parent
    )
    temp_path = Path(temp_name)
    try:
        with os.fdopen(temp_fd, "w+b") as sink, source_path.open("rb") as source:
            header = serialize(layout.ftyp) + serialize(layout.moov)
            sink.write(header)
            sink.write(mdat_header(layout.mdat_size))
            copied = _copy_samples(source, sink, indexes, verify_copy=options.verify_copy)
            if copied != layout.mdat_size:
                raise RemuxError(
                    "copy_size_mismatch",
                    f"copied {copied} bytes but the mdat declares {layout.mdat_size}",
                )
            sink.flush()
            os.fsync(sink.fileno())
        digest = hashlib.sha256()
        with temp_path.open("rb") as handle:
            for chunk in iter(lambda: handle.read(1 << 20), b""):
                digest.update(chunk)
        os.replace(temp_path, output_path)
    except BaseException:
        temp_path.unlink(missing_ok=True)
        raise

    size = output_path.stat().st_size
    expected = layout.total_size
    if size != expected:
        output_path.unlink(missing_ok=True)
        raise RemuxError(
            "output_size_mismatch",
            f"wrote {size} bytes, layout predicted {expected}",
        )
    return RemuxResult(
        output_path=output_path,
        size=size,
        sha256=digest.hexdigest(),
        mapping=mapping,
        plan=plan,
        indexes=indexes,
        layout=layout,
        copied_bytes=copied,
        warnings=list(mapping.warnings) + list(plan.warnings),
    )


__all__ = ["RemuxError", "RemuxOptions", "RemuxResult", "remux"]
