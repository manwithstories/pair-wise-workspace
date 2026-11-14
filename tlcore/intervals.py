"""Half-open integer interval operations."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Iterable, Sequence


@dataclass(frozen=True, order=True)
class Interval:
    start: int
    end: int

    @property
    def duration(self) -> int:
        return self.end - self.start

    @property
    def is_valid(self) -> bool:
        return self.end >= self.start


def overlap(a: Interval, b: Interval) -> int:
    return max(0, min(a.end, b.end) - max(a.start, b.start))


def intersects(a: Interval, b: Interval) -> bool:
    return overlap(a, b) > 0


def contains(container: Interval, inner: Interval) -> bool:
    return container.start <= inner.start and inner.end <= container.end


def adjacent(a: Interval, b: Interval) -> bool:
    return a.end == b.start or b.end == a.start


def merge_intervals(
    intervals: Iterable[Interval], *, merge_adjacent: bool = True
) -> list[Interval]:
    values = sorted(intervals)
    if not values:
        return []
    result = [values[0]]
    for current in values[1:]:
        previous = result[-1]
        touches = previous.end > current.start or (
            merge_adjacent and previous.end == current.start
        )
        if touches:
            result[-1] = Interval(previous.start, max(previous.end, current.end))
        else:
            result.append(current)
    return result


def cluster_overlaps(intervals: Sequence[Interval]) -> list[list[Interval]]:
    """Return connected overlap components; adjacency alone does not connect."""
    if not intervals:
        return []
    ordered = sorted(intervals)
    clusters: list[list[Interval]] = [[ordered[0]]]
    cluster_end = ordered[0].end
    for current in ordered[1:]:
        if current.start < cluster_end:
            clusters[-1].append(current)
            cluster_end = max(cluster_end, current.end)
        else:
            clusters.append([current])
            cluster_end = current.end
    return clusters


def subtract(interval: Interval, cut: Interval) -> list[Interval]:
    if not intersects(interval, cut):
        return [interval]
    result: list[Interval] = []
    if interval.start < cut.start:
        result.append(Interval(interval.start, cut.start))
    if cut.end < interval.end:
        result.append(Interval(cut.end, interval.end))
    return result
