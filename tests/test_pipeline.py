"""Pipeline state and recovery-path tests."""
from __future__ import annotations

import struct
import zlib
from pathlib import Path

import pytest

from fieldpack import fixtures
from fieldpack.cli import safe_resolve
from fieldpack.demux import Demuxer
from fieldpack.mapping import audio_duration_ns, build_plan, summarise_audio
from fieldpack.model import (
    TIMESTAMP_WRAP_NS,
    FieldPackError,
    FrameReady,
    FrameRecord,
    Quarantined,
    SegmentStart,
)
from fieldpack.mux import Muxer
from fieldpack.pipeline import Pipeline
from fieldpack.source import iter_records, probe_source
from fieldpack.validate import check_keyframe_index, validate_output


@pytest.fixture(scope="module")
def sample_dir(tmp_path_factory):
    directory = tmp_path_factory.mktemp("fixtures")
    fixtures.generate_all(directory)
    return directory


# ---- source --------------------------------------------------------------


def test_good_source_parses_all_records(sample_dir):
    items = list(iter_records(sample_dir / "good.fpk"))
    errors = [i for i in items if isinstance(i, FieldPackError)]
    records = [i for i in items if not isinstance(i, FieldPackError)]
    assert errors == []
    assert len(records) == 44
    assert {r.version for r in records} == {1, 2}
    assert {r.device_id for r in records} == {1, 2}
    v2 = next(r for r in records if r.version == 2)
    assert v2.exposure_ns == 20_000_000 and v2.iso == 800


def test_corrupt_source_reports_offsets_and_recovers(sample_dir):
    items = list(iter_records(sample_dir / "corrupt.fpk"))
    errors = [i for i in items if isinstance(i, FieldPackError)]
    records = [i for i in items if not isinstance(i, FieldPackError)]
    codes = [e.code for e in errors]
    assert codes == ["unknown-version", "checksum-mismatch",
                     "field-out-of-bounds", "bad-magic", "truncated-header"]
    assert all(e.offset is not None and e.offset >= 0 for e in errors)
    assert len(records) == 10  # recoverable records after corrupt spots
    offsets = [e.offset for e in errors]
    assert offsets == sorted(offsets)


# ---- demux ---------------------------------------------------------------


def _record(device, seq, raw_ts):
    return FrameRecord(offset=0, version=1, device_id=device, seq=seq,
                       raw_ts_ns=raw_ts, pixfmt="gray", width=2, height=2,
                       keyframe=False, meta=b"", payload=b"\0" * 4)


def test_demux_unwraps_timestamp_wraparound():
    demux = Demuxer()
    base = TIMESTAMP_WRAP_NS - 2
    pts = []
    for raw in (base, base + 1, 0, 1, 2):  # clock wraps mid-sequence
        events = demux.handle(_record(1, len(pts), raw))
        pts.append(next(e.pts_ns for e in events if isinstance(e, FrameReady)))
    assert pts == [base, base + 1, TIMESTAMP_WRAP_NS,
                   TIMESTAMP_WRAP_NS + 1, TIMESTAMP_WRAP_NS + 2]


def test_demux_quarantines_duplicates_and_nonmonotonic():
    demux = Demuxer()
    demux.handle(_record(1, 0, 100))
    dup = demux.handle(_record(1, 0, 100))
    assert isinstance(dup[0], Quarantined)
    demux.handle(_record(1, 1, 200))
    back = demux.handle(_record(1, 2, 50))  # cannot be a wrap: too close
    assert isinstance(back[0], Quarantined)
    assert "non-monotonic" in back[0].reason


def test_demux_sequence_reset_opens_segment_but_clock_continues():
    demux = Demuxer()
    base = TIMESTAMP_WRAP_NS - 10
    segments, pts = [], []
    for i, raw in enumerate((base, base + 5, 3, 8)):  # reset at i=2, clock wrapped
        events = demux.handle(_record(2, 0 if i >= 2 else i, raw))
        for e in events:
            if isinstance(e, SegmentStart):
                segments.append(e.segment.index)
            if isinstance(e, FrameReady):
                pts.append(e.pts_ns)
    assert segments == [0, 1]
    assert pts == sorted(pts)  # unified timeline stays monotonic


# ---- mapping -------------------------------------------------------------


def test_mapping_rejects_unknown_and_overlapping(sample_dir):
    tracks = probe_source(sample_dir / "good.fpk")
    with pytest.raises(FieldPackError, match="unknown-track"):
        build_plan([("camX", 99)], None, tracks)
    with pytest.raises(FieldPackError, match="overlap-mapping"):
        build_plan([("a", 1), ("b", 1)], None, tracks)
    with pytest.raises(FieldPackError, match="overlap-mapping"):
        build_plan([("a", 1), ("a", 2)], None, tracks)


def test_audio_duration_overflow():
    assert audio_duration_ns(48_000, 48_000) == 1_000_000_000
    with pytest.raises(FieldPackError, match="duration-overflow"):
        audio_duration_ns(1 << 40, 1)
    with pytest.raises(FieldPackError, match="invalid-sample-rate"):
        audio_duration_ns(10, 0)


def test_audio_summary_roundtrip(sample_dir):
    track = summarise_audio(sample_dir / "companion.wav")
    assert track.sample_rate == 48_000 and track.channels == 2
    assert track.duration_ns == 500_000_000
    assert summarise_audio(sample_dir / "companion.wav").sha256 == track.sha256


# ---- keyframe index -------------------------------------------------------


def test_keyframe_index_checker_counts_bad_entries():
    check = check_keyframe_index([10, 20, 20, 5, 999], [10, 20, 30])
    assert check.hits == 2
    assert any("duplicate" in e for e in check.errors)
    assert any("out-of-order" in e for e in check.errors)
    assert any("out-of-bounds" in e for e in check.errors)
    assert check_keyframe_index([10, 30], [10, 30]).hit_rate == 1.0


# ---- pipeline --------------------------------------------------------------


def _run(sample_dir, name, queue_size=8, slow_ms=5):
    from fieldpack.mapping import build_plan

    source = sample_dir / f"{name}.fpk"
    tracks = probe_source(source)
    plan = build_plan([(f"cam{d}", d) for d in sorted(tracks)], None, tracks)
    out = sample_dir / f"{name}_test.nut"
    muxer = Muxer(out, plan, tracks)
    stats = Pipeline(queue_size=queue_size, slow_consumer_ms=slow_ms).run(
        source, plan, muxer)
    muxer.close()
    return stats, muxer, out


def test_pipeline_backpressure_bounds_queue(sample_dir):
    stats, _, _ = _run(sample_dir, "good", queue_size=4, slow_ms=2)
    assert 0 < stats.queue_peak_items <= 4
    assert stats.frames_written == 44
    assert stats.producer_wait_ns > 0  # slow consumer forced backpressure
    assert not stats.cancelled


def test_pipeline_recovers_after_corrupt_records(sample_dir):
    stats, _, _ = _run(sample_dir, "corrupt")
    assert stats.corrupt_records == 5
    assert stats.recovered_records == 8
    assert stats.first_error_offset is not None
    assert stats.frames_written == 10


def test_mux_rejects_unsupported_pixfmt_at_setup(tmp_path):
    # a parseable record whose pixel format the container cannot store
    payload = b"\0" * (2 * 2 * 2)
    hdr = struct.Struct("<4sBBHIQBBHHHII")
    meta = b"{}"
    crc = zlib.crc32(meta + payload) & 0xFFFFFFFF
    record = hdr.pack(b"FPR1", 1, 0, 1, 0, 0, 4, 0, 2, 2,
                      len(meta), len(payload), crc) + meta + payload
    source = tmp_path / "bayer.fpk"
    source.write_bytes(record)
    tracks = probe_source(source)
    from fieldpack.mapping import build_plan

    plan = build_plan([("cam1", 1)], None, tracks)
    with pytest.raises(FieldPackError, match="unsupported-pixfmt"):
        Muxer(tmp_path / "bayer.nut", plan, tracks)  # no implicit re-encode


def test_pipeline_cancel_releases_buffers_keeps_segments(sample_dir):
    import threading
    import time

    from fieldpack.mapping import build_plan

    source = sample_dir / "good.fpk"
    tracks = probe_source(source)
    plan = build_plan([(f"cam{d}", d) for d in sorted(tracks)], None, tracks)
    out = sample_dir / "cancel_test.nut"
    muxer = Muxer(out, plan, tracks)
    pipeline = Pipeline(queue_size=2, slow_consumer_ms=50)
    stats_box: dict = {}

    def worker():
        stats_box["stats"] = pipeline.run(source, plan, muxer)

    thread = threading.Thread(target=worker)
    thread.start()
    time.sleep(0.2)
    pipeline.cancel()
    thread.join(timeout=10)
    muxer.close()  # completed segments stay intact in the sidecar index
    stats = stats_box["stats"]
    assert stats.cancelled
    assert stats.frames_written < 44
    assert muxer.index_path.is_file()


# ---- end to end ------------------------------------------------------------


def test_end_to_end_roundtrip_is_lossless(sample_dir):
    stats, muxer, out = _run(sample_dir, "good", slow_ms=0)
    report = validate_output(out, muxer.index_path)
    assert report.consistency == 1.0
    assert report.segment_hash_rate == 1.0
    assert report.keyframes is not None and report.keyframes.hit_rate == 1.0
    assert report.errors == []


# ---- cli helpers ------------------------------------------------------------


def test_safe_resolve_rejects_escape(tmp_path):
    assert safe_resolve(tmp_path, "ok.nut").is_relative_to(tmp_path)
    with pytest.raises(FieldPackError, match="path-escape"):
        safe_resolve(tmp_path, "../escape.nut")
