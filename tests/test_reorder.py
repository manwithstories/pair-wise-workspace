from __future__ import annotations

from frameaudit.model import DecodedFrame
from frameaudit.reorder import PtsReorderer


def frame(index: int, pts: int | None, segment: int = 0) -> DecodedFrame:
    return DecodedFrame(
        decode_idx=index,
        pts_raw_us=pts,
        pict_type="P",
        keyframe=False,
        plane_hash=None,
        frame=None,
        segment=segment,
    )


def run(frames: list[DecodedFrame], depth: int = 16, step: int = 40_000):
    reorder = PtsReorderer(depth=depth, default_step_us=step)
    output = []
    for item in frames:
        output.extend(reorder.push(item))
    output.extend(reorder.flush())
    return output, reorder


def test_b_pyramid_is_fully_reordered_without_loss() -> None:
    decoded = [frame(0, 0), frame(1, 120_000), frame(2, 40_000), frame(3, 80_000)]
    output, reorder = run(decoded)

    assert [item.pts_raw_us for item in output] == [0, 40_000, 80_000, 120_000]
    assert {item.decode_idx for item in output} == {0, 1, 2, 3}
    assert reorder.max_buffered <= 16


def test_equal_pts_keeps_decode_order() -> None:
    output, _ = run([frame(0, 0), frame(1, 40_000), frame(2, 40_000), frame(3, 80_000)])
    assert [item.decode_idx for item in output] == [0, 1, 2, 3]
    assert output[1].pts_raw_us == output[2].pts_raw_us == 40_000


def test_missing_pts_is_interpolated_between_neighbors() -> None:
    output, reorder = run([frame(0, 0), frame(1, None), frame(2, None), frame(3, 120_000)])
    assert [item.pts_raw_us for item in output] == [0, 40_000, 80_000, 120_000]
    assert [item.missing_pts for item in output] == [False, True, True, False]
    assert reorder.interpolated_count == 2


def test_missing_pts_at_end_uses_default_step() -> None:
    output, _ = run([frame(0, 0), frame(1, None), frame(2, None)])
    assert [item.pts_raw_us for item in output] == [0, 40_000, 80_000]
    assert all(item.missing_pts for item in output[1:])


def test_buffer_is_bounded_and_out_of_order_frames_survive() -> None:
    decoded = [frame(0, 0), frame(1, 160_000), frame(2, 40_000), frame(3, 80_000), frame(4, 120_000)]
    output, reorder = run(decoded, depth=3)
    assert [item.pts_raw_us for item in output] == [0, 40_000, 80_000, 120_000, 160_000]
    assert reorder.max_buffered <= 3


def test_segment_change_flushes_before_next_segment() -> None:
    output, _ = run([frame(0, 100_000, 0), frame(1, 200_000, 0), frame(2, 0, 1), frame(3, 50_000, 1)])
    assert [(item.segment, item.pts_raw_us) for item in output] == [
        (0, 100_000),
        (0, 200_000),
        (1, 0),
        (1, 50_000),
    ]

