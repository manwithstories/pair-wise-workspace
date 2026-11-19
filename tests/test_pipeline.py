"""End-to-end and stage-level tests for the restoration pipeline.

Fixtures are generated once per session with the real FFmpeg CLI, so tests run
against decodable media and exercise the actual PyAV/soundfile read paths.
"""

from __future__ import annotations

import json
import shutil
from pathlib import Path

import pytest

from archive_restore.cli import main as cli_main
from archive_restore.errors import (
    EXIT_METRICS,
    EXIT_OK,
    EXIT_INVALID,
    ErrorCollector,
    ErrorKind,
)
from archive_restore.fixtures import build_all
from archive_restore.manifest import ManifestError, Representation, SegmentRef, parse_manifest, safe_resolve
from archive_restore.metrics import av_sync_error, packet_recovery_rate, percentile_latency, TrackSpan
from archive_restore.pipeline import process_asset
from archive_restore.recovery import CheckpointStore, digest_inputs
from archive_restore.stream import BoundedQueue, CancellationToken, Pipeline, StreamItem
from archive_restore.timeline import build_timelines


@pytest.fixture(scope="session")
def archive(tmp_path_factory) -> Path:
    root = tmp_path_factory.mktemp("archive")
    build_all(root)
    return root


@pytest.fixture()
def output(tmp_path) -> Path:
    out = tmp_path / "result"
    yield out
    shutil.rmtree(out, ignore_errors=True)


def _run(archive: Path, output: Path, scenario: str) -> dict:
    return process_asset(
        archive / scenario / "master.m3u8", output / scenario,
        queue_bytes=1_048_576, consumer_delay_ms=25)


# ---------------------------------------------------------------- manifest

def test_safe_resolve_rejects_unsafe_references(tmp_path: Path) -> None:
    for bad in ("../escape.ts", "/etc/passwd", "file:///host/x.ts", "a/../../b.ts"):
        with pytest.raises(ManifestError):
            safe_resolve(tmp_path, tmp_path, bad)
    inside = safe_resolve(tmp_path, tmp_path, "v0/seg0.ts")
    assert tmp_path.resolve() in inside.parents


def test_manifest_parses_three_representations(archive: Path) -> None:
    parsed = parse_manifest(archive / "normal" / "master.m3u8")
    kinds = {rep.kind for rep in parsed.representations}
    assert kinds == {"video", "audio"}
    assert len(parsed.representations) == 3
    assert all(rep.segments for rep in parsed.representations)
    assert not parsed.has_fatal()


def test_dash_segment_timeline_maps_to_microseconds(tmp_path: Path) -> None:
    mpd = (
        '<MPD mediaPresentationDuration="PT6S">'
        '<AdaptationSet mimeType="video/mp4">'
        '<Representation id="v0" codecs="avc1.64" bandwidth="100000">'
        '<SegmentTemplate timescale="1000" media="$RepresentationID$_$Number$.m4s" startNumber="1">'
        '<SegmentTimeline><S t="0" d="2000" r="2"/></SegmentTimeline>'
        '</SegmentTemplate></Representation></AdaptationSet></MPD>'
    )
    path = tmp_path / "a.mpd"
    path.write_text(mpd)
    parsed = parse_manifest(path)
    rep = parsed.representations[0]
    assert [(s.start_us, s.duration_us) for s in rep.segments] == [
        (0, 2_000_000), (2_000_000, 2_000_000), (4_000_000, 2_000_000)]
    result = build_timelines(parsed.representations)
    assert result.timelines["v0"].duration_us == 6_000_000


# ---------------------------------------------------------------- timeline

def test_rollback_is_classified_not_silently_overwritten(archive: Path) -> None:
    parsed = parse_manifest(archive / "rollback" / "master.m3u8")
    errors = ErrorCollector()
    result = build_timelines(parsed.representations, errors)
    v0 = next(tl for rid, tl in result.timelines.items() if "v0" in rid)
    notes = " ".join(seg.note for seg in v0.segments)
    assert "rollback parked" in notes
    assert any("rollback" in e.message for e in errors.records)


def test_wraparound_is_compensated_with_offset(archive: Path) -> None:
    parsed = parse_manifest(archive / "wraparound" / "master.m3u8")
    errors = ErrorCollector()
    result = build_timelines(parsed.representations, errors)
    v0 = next(tl for rid, tl in result.timelines.items() if "v0" in rid)
    last = v0.segments[-1]
    assert "wraparound" in last.note
    assert last.start_us >= 6_000_000  # restart shifted past the high-water mark


def test_duplicate_and_disorder_are_distinguished(archive: Path) -> None:
    dup = parse_manifest(archive / "duplicate" / "master.m3u8")
    dup_res = build_timelines(dup.representations, ErrorCollector())
    dup_v0 = next(tl for rid, tl in dup_res.timelines.items() if "v0" in rid)
    assert any(seg.note == "duplicate" for seg in dup_v0.segments)

    ooo = parse_manifest(archive / "outoforder" / "master.m3u8")
    ooo_res = build_timelines(ooo.representations, ErrorCollector())
    ooo_v0 = next(tl for rid, tl in ooo_res.timelines.items() if "v0" in rid)
    assert any(seg.note == "out-of-order" for seg in ooo_v0.segments)


def test_truncated_playlist_emits_retryable_manifest_error(archive: Path) -> None:
    parsed = parse_manifest(archive / "truncated" / "master.m3u8")
    assert parsed.errors.count(ErrorKind.RETRYABLE) >= 1


def test_zero_duration_and_overlong_segments_are_flagged(tmp_path: Path) -> None:
    refs = [
        SegmentRef(0, tmp_path / "s0.ts", 2_000_000, 0),
        SegmentRef(1, tmp_path / "s1.ts", 0, 2_000_000),            # zero duration
        SegmentRef(2, tmp_path / "s2.ts", 9_000_000, 2_000_000),    # overlong (>3x target)
    ]
    rep = Representation("edge", "video", 100, "avc1", tmp_path, refs, 2_000_000)
    result = build_timelines([rep], ErrorCollector())
    tl = result.timelines["edge"]
    messages = " ".join(e.message for e in tl.errors.records)
    assert "zero-duration" in messages
    assert "overlong" in messages
    zero_seg = next(s for s in tl.segments if s.duration_us == 0)
    assert zero_seg.valid is False


# ------------------------------------------------------------- bounded stream

def test_bounded_queue_enforces_byte_budget() -> None:
    q = BoundedQueue(max_items=100, max_bytes=1000, name="t")
    token = CancellationToken()
    for i in range(5):
        q.put(StreamItem(f"k{i}", i, 150), token)
    assert q.peak_bytes <= 1000
    seen = []
    for _ in range(5):
        item = q.get(token)
        seen.append(item.key)
    assert seen == [f"k{i}" for i in range(5)]


def test_backpressure_blocks_slow_consumer() -> None:
    import time
    collected = []

    def slow(item, token):
        time.sleep(0.02)
        collected.append(item.key)
        return item

    pipe = Pipeline(max_items=4, max_bytes=800).add_stage("s", slow)
    pipe.run()
    for i in range(6):
        pipe.submit(StreamItem(f"k{i}", i, 200))
    pipe.finish()
    assert collected == [f"k{i}" for i in range(6)]
    assert pipe.peak_queue_bytes <= 800


def test_cancellation_propagates_through_stages() -> None:
    pipe = Pipeline(max_items=4, max_bytes=1000).add_stage("a", lambda item, t: item)
    pipe.run()
    pipe.cancel()
    with pytest.raises(Exception):
        pipe.submit(StreamItem("x", 1, 10))
    pipe.finish()  # must drain without hanging


# ------------------------------------------------------------- recovery / cp

def test_missing_segment_is_recovered_from_peer(archive: Path, output: Path) -> None:
    result = _run(archive, output, "missing")
    recovery = json.loads((output / "missing" / "recovery.json").read_text())
    v0 = next(r for r in recovery["representations"] if "v0" in r["rep_id"])
    assert v0["recovered_from_peers"], "gap should be filled from the v1 peer"
    assert result["metrics"]["packet_recovery_rate"] >= 0.95


def test_corrupt_segments_leave_explicit_gaps(archive: Path, output: Path) -> None:
    result = _run(archive, output, "truncated")
    metrics = result["metrics"]
    assert metrics["gaps"], "an unrecoverable tail must remain an explicit gap"
    assert metrics["packet_recovery_rate"] < 0.95
    assert all(gap["recoverable"] is False for gap in metrics["gaps"])


def test_checkpoint_rolls_back_on_digest_change(archive: Path, tmp_path: Path) -> None:
    out = tmp_path / "cp"
    digest = digest_inputs(archive / "normal")
    store = CheckpointStore(out, digest)
    store.mark_done("k1", 10)
    store.mark_done("k2", 20)
    # A changed input digest invalidates the checkpoint on resume.
    changed = CheckpointStore(out, digest + "deadbeef")
    with pytest.raises(Exception):
        changed.load()


def test_checkpoint_truncates_partial_output(tmp_path: Path) -> None:
    store = CheckpointStore(tmp_path, "digest")
    store.mark_done("k1", 10)
    data = tmp_path / "data.bin"
    data.write_bytes(b"ab")  # torn: 2 bytes vs boundary 10
    safe = store.load() and store.verify_output(data)
    assert safe == 2
    assert store.safe_boundary == 2
    assert "k1" not in store.completed


# ------------------------------------------------------------------- metrics

def test_av_sync_none_when_track_missing() -> None:
    spans = {"video": TrackSpan("video", None, None, False, "no video"),
             "audio": TrackSpan("audio", 0, 3_000_000)}
    value, status = av_sync_error(spans)
    assert value is None and "missing" in status


def test_percentile_and_recovery_helpers() -> None:
    assert percentile_latency([1, 2, 3, 4, 5, 6, 7, 8, 9, 100]) > 9
    assert packet_recovery_rate([]) == 1.0


# --------------------------------------------------------------------- e2e cli

def test_normal_asset_meets_all_thresholds(archive: Path, output: Path) -> None:
    result = _run(archive, output, "normal")
    metrics = result["metrics"]
    assert metrics["av_sync_error_ms"] <= 40
    assert metrics["packet_recovery_rate"] >= 0.95
    assert 25 <= metrics["p95_latency_ms"] <= 500
    assert metrics["peak_memory_bytes"] <= 1_048_576
    assert metrics["aligned_representations"] >= 2
    assert "gaps" in metrics


def test_verify_exit_codes(archive: Path, output: Path) -> None:
    _run(archive, output, "normal")
    _run(archive, output, "truncated")
    assert cli_main(["verify", "--input", str(archive / "normal"),
                     "--result", str(output / "normal"), "--json"]) == EXIT_OK
    assert cli_main(["verify", "--input", str(archive),
                     "--result", str(output), "--json"]) == EXIT_METRICS


def test_invalid_input_returns_exit_two(tmp_path: Path) -> None:
    assert cli_main(["restore", "--input", str(tmp_path / "missing"),
                     "--output", str(tmp_path / "out"), "--json"]) == EXIT_INVALID


def test_peak_memory_respects_queue_budget(archive: Path, output: Path) -> None:
    result = _run(archive, output, "normal")
    assert result["metrics"]["peak_memory_bytes"] <= 1_048_576
