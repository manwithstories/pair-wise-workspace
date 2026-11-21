"""Tests for shared-timeline segmentation and nearest-frame rules."""

from archive_media.segments import (
    FrameMark,
    Segmenter,
    audio_sample_window,
)


def _marks(track, times):
    return [FrameMark(track, i, t, raw_pts=i) for i, t in enumerate(times)]


def test_uniform_segment_boundaries():
    seg = Segmenter(target_duration_s=0.5)
    for mark in _marks("video", [i / 12.0 for i in range(24)]):
        seg.add(mark)
    out = seg.build()
    video_segments = [s for s in out if s.track == "video"]
    assert len(video_segments) >= 3
    # Segments cover every frame exactly once (contiguous index ranges).
    for prev, nxt in zip(video_segments, video_segments[1:]):
        assert nxt.start_index == prev.end_index + 1
    covered = sum(s.frame_count for s in video_segments)
    assert covered == 24
    assert video_segments[0].start_index == 0
    assert video_segments[-1].end_index == 23


def test_tie_resolves_to_earlier_frame():
    seg = Segmenter(target_duration_s=1.0)
    # Frames at 0.9 and 1.1 are equidistant (0.1) from the 1.0 boundary.
    for mark in _marks("video", [0.0, 0.9, 1.1, 2.0]):
        seg.add(mark)
    out = seg.build()
    first = out[0]
    # Ties resolve earlier, so the boundary at 1.0 picks the 0.9 frame.
    assert first.end_s == 0.9


def test_shared_grid_keeps_audio_video_aligned():
    seg = Segmenter(target_duration_s=0.5)
    for mark in _marks("video", [i / 30.0 for i in range(60)]):
        seg.add(mark)
    for mark in _marks("audio", [i * 1024 / 24000.0 for i in range(47)]):
        seg.add(mark)
    out = seg.build()
    by_track = {}
    for s in out:
        by_track.setdefault(s.track, {})[s.segment_id] = s
    common = set(by_track["video"]) & set(by_track["audio"])
    assert common, "audio and video must share segment ordinals"
    for seg_id in common:
        gap = abs(
            by_track["video"][seg_id].start_s - by_track["audio"][seg_id].start_s
        )
        assert gap <= 0.04  # within the 40 ms alignment tolerance


def test_audio_sample_window_clamps():
    start, end = audio_sample_window(0.25, 0.5, sample_rate=24000)
    assert start == 6000
    assert end == 12000
    # Negative start must clamp to zero.
    assert audio_sample_window(-1.0, 0.5, sample_rate=24000)[0] == 0
