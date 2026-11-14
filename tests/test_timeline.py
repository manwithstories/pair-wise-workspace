from types import SimpleNamespace

from mkvtool.mkv.demux import DemuxResult, Sample, Track
from mkvtool.mkv.timeline import build_timeline


def _demux(samples, track_type=1, default_ns=40_000_000, gap=False):
    track = Track(1, 1, track_type, "V_MPEG4/ISO/AVC", default_duration_ns=default_ns)
    if gap:
        for index, sample in enumerate(samples):
            if index >= 6:
                sample.timestamp_ticks += 1001
    return DemuxResult(
        parse=SimpleNamespace(issues=[]),
        segment=SimpleNamespace(data_offset=0),
        tracks=[track],
        samples={1: samples},
        timecode_scale_ns=1_000_000,
        duration_ns=None,
    )


def _samples(count=10):
    return [Sample(1, -80 + index * 40, None, index % 5 == 0, bytes([index]), 0, index)
            for index in range(count)]


def test_negative_dts_is_shifted_to_zero():
    result = build_timeline(_demux(_samples()))
    assert result.shift_ns == 80_000_000
    rewritten = result.track(1)
    assert rewritten.source_first_dts_ns == -80_000_000
    assert rewritten.rewritten_first_dts_ns == 0
    assert rewritten.samples[-1].dts_ns == rewritten.samples[-2].dts_ns + 40_000_000


def test_gap_detected_and_optional_fill():
    demux = _demux(_samples(), gap=True)
    plain = build_timeline(demux)
    assert len(plain.track(1).gaps) == 1
    assert plain.track(1).gaps[0].duration_ns == 1001_000_000
    filled = build_timeline(demux, fill_gaps=True)
    assert filled.track(1).gaps[0].filled_frames > 0
    assert len(filled.track(1).samples) > len(plain.track(1).samples)


def test_duplicate_timestamp_is_nudged_monotonic():
    samples = _samples(5)
    samples[2].timestamp_ticks = samples[1].timestamp_ticks
    result = build_timeline(_demux(samples))
    rewritten = result.track(1).samples
    assert all(a.dts_ns < b.dts_ns for a, b in zip(rewritten, rewritten[1:]))
