from tlcore.intervals import (
    Interval,
    adjacent,
    cluster_overlaps,
    contains,
    merge_intervals,
    overlap,
)


def test_half_open_and_adjacency():
    first, second = Interval(0, 10), Interval(10, 20)
    assert overlap(first, second) == 0
    assert adjacent(first, second)
    assert contains(Interval(0, 20), first)


def test_merge_and_cluster():
    merged = merge_intervals([Interval(0, 4), Interval(4, 8), Interval(9, 11)])
    assert merged == [Interval(0, 8), Interval(9, 11)]
    clusters = cluster_overlaps([Interval(0, 5), Interval(4, 8), Interval(8, 9)])
    assert clusters == [[Interval(0, 5), Interval(4, 8)], [Interval(8, 9)]]
