"""Keyframe index generation and validation.

The index maps every keyframe's presentation time to the byte offset the
container reports for it, and is then *validated against the file*:

* **hit rate** - seeking to each indexed timestamp must land on a keyframe at or
  before that timestamp.  A healthy index hits 100% of the time; the acceptance
  threshold is 99% to tolerate one rounding edge.
* **duplicates** - two entries with the same ``(stream, timestamp)``.
* **out of order** - timestamps must ascend per stream.
* **out of range** - an offset at or beyond EOF, or a negative offset.

Checks run against the container as it is, so a file whose Cues table points
past EOF is reported rather than silently trusted.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path

import av

from .probe import ProbeResult
from .timebase import InputError, NoIndexError, to_ticks


@dataclass(frozen=True)
class KeyframeEntry:
    stream_index: int
    pts: int
    time_base: tuple[int, int]
    pts_ticks: int
    pos: int | None
    size: int

    def to_dict(self) -> dict[str, object]:
        return {
            "stream_index": self.stream_index,
            "pts": self.pts,
            "time_base": list(self.time_base),
            "pts_ticks": self.pts_ticks,
            "pos": self.pos,
            "size": self.size,
        }


@dataclass
class KeyframeIndex:
    stream_index: int
    entries: list[KeyframeEntry] = field(default_factory=list)
    seekable: bool = True

    def to_dict(self) -> dict[str, object]:
        return {
            "stream_index": self.stream_index,
            "seekable": self.seekable,
            "count": len(self.entries),
            "entries": [entry.to_dict() for entry in self.entries],
        }


@dataclass
class IndexProblem:
    kind: str
    detail: str
    stream_index: int | None = None
    position: int | None = None

    def to_dict(self) -> dict[str, object]:
        return {
            "kind": self.kind,
            "detail": self.detail,
            "stream_index": self.stream_index,
            "position": self.position,
        }


@dataclass
class IndexReport:
    file_size: int
    indexes: list[KeyframeIndex]
    problems: list[IndexProblem] = field(default_factory=list)
    seeks_attempted: int = 0
    seeks_hit: int = 0

    @property
    def hit_rate(self) -> float:
        if self.seeks_attempted == 0:
            return 1.0
        return self.seeks_hit / self.seeks_attempted

    @property
    def keyframe_count(self) -> int:
        return sum(len(index.entries) for index in self.indexes)

    @property
    def ok(self) -> bool:
        return not self.problems and self.hit_rate >= 0.99

    def to_dict(self) -> dict[str, object]:
        return {
            "file_size": self.file_size,
            "keyframe_count": self.keyframe_count,
            "hit_rate": self.hit_rate,
            "seeks_attempted": self.seeks_attempted,
            "seeks_hit": self.seeks_hit,
            "problems": [problem.to_dict() for problem in self.problems],
            "indexes": [index.to_dict() for index in self.indexes],
            "ok": self.ok,
        }


def container_index_problems(
    report: IndexReport, probe_result: ProbeResult
) -> list[IndexProblem]:
    """Validate the container's *own* cue table, not just the packet scan.

    A Matroska file can carry a Cues element that is duplicated, disordered or
    points past EOF while the packets themselves are perfectly demuxable.  The
    packet scan cannot see that damage, so the container index is checked here
    and its findings are merged into the same report.
    """
    if not probe_result.index_info:
        return []
    entries = probe_result.index_info.get("cue_entries") or []
    if not entries:
        return []
    scale_ns = int(probe_result.index_info.get("cue_scale_ns") or 1_000_000)
    segment_start = int(probe_result.index_info.get("segment_start") or 0)
    file_size = report.file_size
    problems: list[IndexProblem] = []
    seen_times: set[int] = set()
    previous: int | None = None
    for entry in entries:
        time_value = entry.get("time")
        position = entry.get("cluster_position")
        if isinstance(time_value, int):
            ticks = time_value * scale_ns
            if time_value in seen_times:
                problems.append(
                    IndexProblem(
                        "container_duplicate",
                        f"container cue duplicate at {ticks} ticks",
                        position=position if isinstance(position, int) else None,
                    )
                )
            seen_times.add(time_value)
            if previous is not None and time_value < previous:
                problems.append(
                    IndexProblem(
                        "container_out_of_order",
                        f"container cue {ticks} ticks follows a later entry",
                        position=position if isinstance(position, int) else None,
                    )
                )
            previous = time_value
        if isinstance(position, int):
            absolute = segment_start + position
            if position < 0 or absolute >= file_size:
                problems.append(
                    IndexProblem(
                        "container_out_of_range",
                        (
                            f"container cue offset {absolute} lies outside the "
                            f"{file_size}-byte file"
                        ),
                        position=absolute,
                    )
                )
    return problems


def build_index(path: str | Path, probe_result: ProbeResult | None = None) -> list[KeyframeIndex]:
    """Collect keyframe entries for every video stream in ``path``."""
    source = Path(path)
    if not source.exists():
        raise InputError("input file does not exist", path=str(source))
    try:
        container = av.open(str(source))
    except av.error.FFmpegError as exc:
        raise InputError(
            "container could not be opened for indexing",
            path=str(source),
            error=str(exc),
        ) from exc
    try:
        video_streams = [stream for stream in container.streams if stream.type == "video"]
        if not video_streams:
            # Audio-only assets have no keyframes; the container's cue table can
            # still be validated by the caller, so this is not an error.
            return []
        indexes: dict[int, KeyframeIndex] = {
            stream.index: KeyframeIndex(stream_index=stream.index) for stream in video_streams
        }
        try:
            for packet in container.demux():
                if packet.size == 0 or packet.stream.index not in indexes:
                    continue
                if not packet.is_keyframe or packet.pts is None:
                    continue
                stream = next(
                    stream for stream in video_streams if stream.index == packet.stream.index
                )
                time_base = stream.time_base
                ticks = to_ticks(packet.pts, time_base)
                if ticks is None or ticks < 0:
                    continue
                indexes[packet.stream.index].entries.append(
                    KeyframeEntry(
                        stream_index=packet.stream.index,
                        pts=packet.pts,
                        time_base=(time_base.numerator, time_base.denominator),
                        pts_ticks=ticks,
                        pos=packet.pos,
                        size=packet.size,
                    )
                )
        except av.error.FFmpegError as exc:
            # A container that fails mid-scan still yields a partial index; the
            # caller sees the truncation separately from probing.
            raise NoIndexError(
                "keyframe scan failed",
                path=str(source),
                error=str(exc),
            ) from exc
        return [indexes[stream.index] for stream in video_streams]
    finally:
        container.close()


def validate_index(
    path: str | Path,
    indexes: list[KeyframeIndex],
    *,
    probe_result: ProbeResult | None = None,
    probe_seeks: bool = True,
    max_seeks: int = 64,
) -> IndexReport:
    """Check the index against the file and measure its seek hit rate.

    ``probe_result``, when supplied, enables validation of the container's own
    cue table in addition to the scan-derived index.
    """
    source = Path(path)
    file_size = source.stat().st_size
    report = IndexReport(file_size=file_size, indexes=indexes)
    if probe_result is not None:
        report.problems.extend(container_index_problems(report, probe_result))
    seen: dict[int, set[int]] = {}
    last_ticks: dict[int, int] = {}

    for index in indexes:
        previous = None
        seen.setdefault(index.stream_index, set())
        for entry in index.entries:
            if entry.pos is not None and (entry.pos < 0 or entry.pos >= file_size):
                report.problems.append(
                    IndexProblem(
                        "out_of_range",
                        f"offset {entry.pos} lies outside the {file_size}-byte file",
                        stream_index=entry.stream_index,
                        position=entry.pos,
                    )
                )
            if entry.pts_ticks in seen[index.stream_index]:
                report.problems.append(
                    IndexProblem(
                        "duplicate",
                        f"duplicate keyframe at {entry.pts_ticks} ticks",
                        stream_index=entry.stream_index,
                        position=entry.pos,
                    )
                )
            seen[index.stream_index].add(entry.pts_ticks)
            if previous is not None and entry.pts_ticks < previous:
                report.problems.append(
                    IndexProblem(
                        "out_of_order",
                        f"keyframe {entry.pts_ticks} follows {previous}",
                        stream_index=entry.stream_index,
                        position=entry.pos,
                    )
                )
            previous = entry.pts_ticks
            last_ticks[index.stream_index] = max(
                last_ticks.get(index.stream_index, entry.pts_ticks), entry.pts_ticks
            )

    if probe_seeks:
        report.seeks_attempted, report.seeks_hit = _measure_seek_hits(
            source, indexes, max_seeks=max_seeks, report=report
        )
    return report


def _measure_seek_hits(
    path: Path,
    indexes: list[KeyframeIndex],
    *,
    max_seeks: int,
    report: IndexReport,
) -> tuple[int, int]:
    """Seek to a sample of indexed timestamps and count keyframe landings."""
    try:
        container = av.open(str(path))
    except av.error.FFmpegError:
        return 0, 0
    attempts = 0
    hits = 0
    try:
        streams = {stream.index: stream for stream in container.streams}
        for index in indexes:
            stream = streams.get(index.stream_index)
            if stream is None:
                continue
            # Evenly sample up to ``max_seeks`` entries across all streams.
            step = max(1, len(index.entries) // max(1, max_seeks // max(1, len(indexes))))
            for entry in index.entries[::step][:max_seeks]:
                attempts += 1
                try:
                    container.seek(entry.pts, stream=stream, backward=True)
                except (av.error.FFmpegError, ValueError):
                    continue
                landing = None
                for packet in container.demux():
                    if packet.size == 0 or packet.stream.index != index.stream_index:
                        continue
                    landing = packet
                    break
                if landing is None:
                    continue
                if landing.is_keyframe and landing.pts is not None:
                    # A backward seek may land on an earlier keyframe than the
                    # requested one; both count as a hit so long as the landing
                    # frame is a keyframe at or before the target.
                    if landing.pts <= entry.pts:
                        hits += 1
                    else:
                        report.problems.append(
                            IndexProblem(
                                "seek_overshoot",
                                f"seek to {entry.pts} landed ahead at {landing.pts}",
                                stream_index=index.stream_index,
                                position=landing.pos,
                            )
                        )
                else:
                    report.problems.append(
                        IndexProblem(
                            "seek_miss",
                            f"seek to {entry.pts} landed on a non-keyframe",
                            stream_index=index.stream_index,
                            position=landing.pos,
                        )
                    )
    finally:
        container.close()
    return attempts, hits


def index_for_stream(report: IndexReport, stream_index: int) -> KeyframeIndex | None:
    for index in report.indexes:
        if index.stream_index == stream_index:
            return index
    return None


def nearest_keyframe(index: KeyframeIndex, ticks: int) -> KeyframeEntry | None:
    """Return the last keyframe at or before ``ticks`` (binary search)."""
    low, high = 0, len(index.entries) - 1
    best: KeyframeEntry | None = None
    while low <= high:
        middle = (low + high) // 2
        entry = index.entries[middle]
        if entry.pts_ticks <= ticks:
            best = entry
            low = middle + 1
        else:
            high = middle - 1
    return best


__all__ = [
    "IndexProblem",
    "IndexReport",
    "KeyframeEntry",
    "KeyframeIndex",
    "build_index",
    "container_index_problems",
    "index_for_stream",
    "nearest_keyframe",
    "validate_index",
]
