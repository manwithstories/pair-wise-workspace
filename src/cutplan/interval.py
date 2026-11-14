from __future__ import annotations

from collections.abc import Iterable, Sequence

from .timebase import FrameInterval


def intersection(left: FrameInterval, right: FrameInterval) -> FrameInterval | None:
    start = max(left.start, right.start)
    end = min(left.end, right.end)
    if start >= end:
        return None
    return FrameInterval(start, end)


def overlaps(left: FrameInterval, right: FrameInterval) -> bool:
    return max(left.start, right.start) < min(left.end, right.end)


def contains(outer: FrameInterval, inner: FrameInterval) -> bool:
    return outer.start <= inner.start and inner.end <= outer.end


def adjacent(left: FrameInterval, right: FrameInterval) -> bool:
    return left.end == right.start or right.end == left.start


def subtract(left: FrameInterval, right: FrameInterval) -> list[FrameInterval]:
    cut = intersection(left, right)
    if cut is None:
        return [left]
    pieces: list[FrameInterval] = []
    if left.start < cut.start:
        pieces.append(FrameInterval(left.start, cut.start))
    if cut.end < left.end:
        pieces.append(FrameInterval(cut.end, left.end))
    return pieces


def merge_intervals(intervals: Iterable[FrameInterval], *, join_adjacent: bool = True) -> list[FrameInterval]:
    ordered = sorted((item for item in intervals if not item.empty), key=lambda item: (item.start, item.end))
    merged: list[FrameInterval] = []
    for item in ordered:
        if not merged:
            merged.append(item)
            continue
        previous = merged[-1]
        touching = item.start <= previous.end if join_adjacent else item.start < previous.end
        if touching:
            merged[-1] = FrameInterval(previous.start, max(previous.end, item.end))
        else:
            merged.append(item)
    return merged


def covered_frames(intervals: Iterable[FrameInterval]) -> int:
    return sum(item.duration for item in merge_intervals(intervals))


def scan_clusters(intervals: Sequence[FrameInterval]) -> list[list[int]]:
    ordered = sorted(
        ((item.start, item.end, index) for index, item in enumerate(intervals) if not item.empty),
        key=lambda row: (row[0], row[1], row[2]),
    )
    clusters: list[list[int]] = []
    cluster: list[int] = []
    cluster_end = -1
    for start, end, index in ordered:
        if not cluster or start < cluster_end:
            cluster.append(index)
            cluster_end = max(cluster_end, end)
        else:
            if len(cluster) > 1:
                clusters.append(cluster)
            cluster = [index]
            cluster_end = end
    if len(cluster) > 1:
        clusters.append(cluster)
    return clusters


def overlap_pairs(intervals: Sequence[FrameInterval]) -> list[tuple[int, int]]:
    ordered = sorted(
        ((item.start, item.end, index) for index, item in enumerate(intervals) if not item.empty),
        key=lambda row: (row[0], row[1], row[2]),
    )
    pairs: list[tuple[int, int]] = []
    for position, (_, end, index) in enumerate(ordered):
        for other_start, _, other_index in ordered[position + 1 :]:
            if other_start >= end:
                break
            pairs.append((min(index, other_index), max(index, other_index)))
    return sorted(pairs)


def cluster_stats(intervals: Sequence[FrameInterval]) -> dict[str, object]:
    clusters = scan_clusters(intervals)
    pairs = overlap_pairs(intervals)
    return {
        "clusters": len(clusters),
        "pairs": len(pairs),
        "cluster_indices": clusters,
        "pairs_detail": pairs,
    }
