"""End-to-end and unit tests for the lossless broadcast remux QC pipeline.

The suite drives the real synthetic fixtures (no mocks around PyAV), so every
assertion is made against bytes that a demuxer actually produced.
"""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

import av
import pytest

from bcastqc import container
from bcastqc.cli import main
from bcastqc.demux import probe_asset
from bcastqc.errors import LosslessCopyRefusedError
from bcastqc.trackmap import (
    DuplicateMappingError,
    IncompatibleTrackError,
    MissingTrackError,
)
from bcastqc.fixtures import MARKERS, build_fixtures, strip_audio
from bcastqc.pipeline import PipelineOptions, process_asset
from bcastqc.recover import recover_asset, validate_packet
from bcastqc.remux import remux_asset
from bcastqc.report import THRESHOLDS
from bcastqc.sync import measure_sync
from bcastqc.timeline import (
    DEFAULT_DRIFT_TOLERANCE,
    PacketSpan,
    analyse_timelines,
    build_stream_timeline,
)
from bcastqc.trackmap import TrackRule, build_track_graph, stable_automatic_rules

from fractions import Fraction


@pytest.fixture(scope="session")
def demo_dir(tmp_path_factory: pytest.TempPathFactory) -> Path:
    workdir = tmp_path_factory.mktemp("bcastqc-demo")
    build_fixtures(workdir)
    return workdir


@pytest.fixture(scope="session")
def fixtures(demo_dir: Path):
    from bcastqc.fixtures import FixtureSet

    inputs = demo_dir / "inputs"
    return FixtureSet(
        normal=inputs / "normal_master.mp4",
        truncated=inputs / "truncated_tail.mp4",
        corrupt=inputs / "corrupt_packets.mp4",
        incomplete_index=inputs / "incomplete_index.mp4",
        refused=inputs / "refused_vp9_opus.mp4",
    )


def _packets(path: Path) -> dict[int, list[bytes]]:
    out: dict[int, list[bytes]] = {}
    with av.open(str(path)) as inp:
        for packet in inp.demux():
            if packet.size == 0:
                continue
            out.setdefault(packet.stream.index, []).append(bytes(packet))
    return out


# --------------------------------------------------------------------------
# timeline
# --------------------------------------------------------------------------


def test_timeline_keeps_vfr_and_flags_regression_and_gap():
    tb = Fraction(1, 1000)
    spans = [
        PacketSpan(0, Fraction(0), Fraction(0), Fraction(1, 25), 0, 10, True),
        PacketSpan(0, Fraction(1, 25), Fraction(1, 25), Fraction(1, 10), 10, 10, False),
        PacketSpan(0, Fraction(9, 25), Fraction(9, 25), Fraction(1, 25), 20, 10, False),
        PacketSpan(0, Fraction(5, 25), Fraction(5, 25), Fraction(1, 25), 30, 10, False),
    ]
    timeline = build_stream_timeline(0, "video", tb, spans)
    assert timeline.packet_count == 4
    # A 100 ms frame followed by a 300 ms hole is a gap; the 100 ms frame alone
    # is a legal VFR hold, not an issue.
    assert any(issue.kind == "timestamp_gap" for issue in timeline.gaps)
    assert timeline.regressions, "backwards pts must be reported"


def test_av_drift_is_a_timeline_issue():
    video = build_stream_timeline(
        0,
        "video",
        Fraction(1, 1000),
        [
            PacketSpan(0, Fraction(0), Fraction(0), Fraction(1, 25), 0, 1, True),
            PacketSpan(0, Fraction(5), Fraction(5), Fraction(1, 25), 1, 1, True),
        ],
    )
    audio = build_stream_timeline(
        1,
        "audio",
        Fraction(1, 1000),
        [
            PacketSpan(1, Fraction(1, 10), Fraction(1, 10), Fraction(1, 25), 2, 1, True),
            PacketSpan(1, Fraction(7), Fraction(7), Fraction(1, 25), 3, 1, True),
        ],
    )
    report = analyse_timelines({0: video, 1: audio})
    assert report.av_drift is not None
    assert report.av_drift > DEFAULT_DRIFT_TOLERANCE
    assert any(issue.kind == "av_drift" for issue in report.issues)


# --------------------------------------------------------------------------
# probing and mapping
# --------------------------------------------------------------------------


def test_probe_reports_exact_time_bases_and_extradata(fixtures):
    probe = probe_asset(fixtures.normal)
    assert {s.type for s in probe.streams} == {"video", "audio"}
    video = next(s for s in probe.streams if s.type == "video")
    audio = next(s for s in probe.streams if s.type == "audio")
    assert video.codec == "h264" and audio.codec == "aac"
    assert video.time_base > 0 and audio.time_base > 0
    assert len(video.extradata) > 0
    assert len(video.extradata_sha256) == 64
    assert video.copyable and audio.copyable


def test_duplicate_output_slot_is_rejected(fixtures):
    probe = probe_asset(fixtures.normal)
    rules = [
        TrackRule(output=0, source=0, kind="video"),
        TrackRule(output=0, source=1, kind="audio"),
    ]
    with pytest.raises(DuplicateMappingError) as excinfo:
        build_track_graph(probe, rules)
    assert excinfo.value.location.rule == 0


def test_duplicate_source_is_rejected(fixtures):
    probe = probe_asset(fixtures.normal)
    rules = [
        TrackRule(output=0, source=0, kind="video"),
        TrackRule(output=1, source=0, kind="video"),
    ]
    with pytest.raises(DuplicateMappingError) as excinfo:
        build_track_graph(probe, rules)
    assert excinfo.value.location.stream == 0


def test_wrong_kind_is_incompatible(fixtures):
    probe = probe_asset(fixtures.normal)
    rules = [TrackRule(output=0, source=0, kind="audio")]
    with pytest.raises(IncompatibleTrackError) as excinfo:
        build_track_graph(probe, rules)
    assert excinfo.value.location.stream == 0


def test_missing_required_track_is_partial_not_fatal(tmp_path, demo_dir):
    probe = probe_asset(demo_dir / "inputs" / "normal_master.mp4")
    rules = [
        TrackRule(output=0, source=0, kind="video"),
        TrackRule(output=1, source=5, kind="audio"),
    ]
    graph = build_track_graph(probe, rules)
    assert graph.partial
    assert graph.missing_required == [1]


def test_empty_mapping_is_fatal(tmp_path, demo_dir):
    probe = probe_asset(demo_dir / "inputs" / "normal_master.mp4")
    with pytest.raises(MissingTrackError):
        build_track_graph(probe, [TrackRule(output=0, source=9, kind="video", required=False)])


# --------------------------------------------------------------------------
# recovery
# --------------------------------------------------------------------------


def test_normal_asset_needs_no_recovery(fixtures):
    probe = probe_asset(fixtures.normal)
    outcome = recover_asset(probe)
    assert outcome.plan.strategies == ["none"]
    assert outcome.recovery_rate == 1.0
    assert outcome.skipped_count == 0


@pytest.mark.parametrize(
    "name, strategies",
    [
        ("truncated", {"boundary_scan"}),
        ("corrupt", {"skip_corrupt_packets"}),
        ("incomplete_index", {"rebuild_index"}),
    ],
)
def test_damaged_assets_are_recognised(fixtures, name, strategies):
    probe = probe_asset(getattr(fixtures, name))
    outcome = recover_asset(probe)
    assert strategies.issubset(set(outcome.plan.strategies))
    assert outcome.recovery_rate >= 0.80
    assert outcome.plan.issues, "a recoverable defect must carry located evidence"


def test_truncated_asset_keeps_only_present_packets(fixtures):
    probe = probe_asset(fixtures.truncated)
    outcome = recover_asset(probe)
    report = container.inspect_index(fixtures.truncated.read_bytes())
    assert report.truncated_tail
    for packet in outcome.packets:
        assert packet.pos is not None
        assert packet.pos + packet.size <= report.mdat_end


def test_corrupt_packets_are_skipped_not_repaired(fixtures):
    probe = probe_asset(fixtures.corrupt)
    outcome = recover_asset(probe)
    assert outcome.skipped_count >= 1
    video = next(s for s in probe.streams if s.type == "video")
    for decision in outcome.skipped:
        assert decision.packet.stream == video.index
        assert not validate_packet(video, decision.packet.data)[0]


def test_incomplete_index_leaves_unreferenced_bytes(fixtures):
    report = container.inspect_index(fixtures.incomplete_index.read_bytes())
    assert report.incomplete
    assert report.unindexed_bytes() > 0
    outcome = recover_asset(probe_asset(fixtures.incomplete_index))
    assert "rebuild_index" in outcome.plan.strategies


def test_recovery_never_decodes_or_invents_payloads(fixtures):
    source_packets = {packet["pos"] for packet in _with_positions(fixtures.corrupt)}
    probe = probe_asset(fixtures.corrupt)
    outcome = recover_asset(probe)
    for packet in outcome.packets:
        assert packet.pos in source_packets
        assert packet.data == _literal_bytes(fixtures.corrupt, packet.pos, packet.size)


def _with_positions(path: Path):
    with av.open(str(path)) as inp:
        for packet in inp.demux():
            if packet.size == 0:
                continue
            yield {"pos": packet.pos, "size": packet.size, "stream": packet.stream.index}


def _literal_bytes(path: Path, pos: int, size: int) -> bytes:
    with path.open("rb") as stream:
        stream.seek(pos)
        return stream.read(size)


# --------------------------------------------------------------------------
# remux
# --------------------------------------------------------------------------


def test_lossless_remux_keeps_every_packet_byte_identical(fixtures, tmp_path):
    probe = probe_asset(fixtures.normal)
    graph = build_track_graph(probe, stable_automatic_rules(probe))
    outcome = recover_asset(probe)
    target = tmp_path / "out.mp4"
    result = remux_asset(probe, graph, outcome.packets, target)
    assert result.packets_written == len(outcome.packets)
    source = _packets(fixtures.normal)
    written = _packets(target)
    for stream, payloads in source.items():
        assert written[stream] == payloads, f"stream {stream} was not copied byte-for-byte"


def test_remuxed_output_has_a_fresh_complete_index(fixtures, tmp_path):
    probe = probe_asset(fixtures.truncated)
    graph = build_track_graph(probe, stable_automatic_rules(probe))
    outcome = recover_asset(probe)
    target = tmp_path / "recovered.mp4"
    remux_asset(probe, graph, outcome.packets, target)
    report = container.inspect_index(target.read_bytes())
    assert not report.incomplete
    assert report.unindexed_bytes() == 0


def test_sync_is_preserved_end_to_end(fixtures, tmp_path):
    probe = probe_asset(fixtures.normal)
    graph = build_track_graph(probe, stable_automatic_rules(probe))
    outcome = recover_asset(probe)
    target = tmp_path / "out.mp4"
    remux_asset(probe, graph, outcome.packets, target)
    measurement = measure_sync(target, MARKERS)
    assert measurement.measured, measurement.error
    assert measurement.sync_error_ms is not None
    assert measurement.sync_error_ms <= 5.0


def test_refusal_names_the_offending_codec(fixtures, tmp_path):
    probe = probe_asset(fixtures.refused)
    graph = build_track_graph(probe, stable_automatic_rules(probe))
    outcome = recover_asset(probe)
    with pytest.raises(LosslessCopyRefusedError) as excinfo:
        remux_asset(probe, graph, outcome.packets, tmp_path / "x.mp4")
    assert excinfo.value.location.stream == 0
    assert "vp9" in excinfo.value.message


# --------------------------------------------------------------------------
# pipeline, cache and CLI
# --------------------------------------------------------------------------


def _options(tmp_path: Path) -> PipelineOptions:
    return PipelineOptions(
        workdir=tmp_path,
        output_dir=tmp_path / "outputs",
        cache_dir=tmp_path / "cache",
        timeout_s=30.0,
        sync_markers=MARKERS,
    )


def test_pipeline_writes_output_atomically_and_reports(fixtures, tmp_path):
    result = process_asset(fixtures.normal, _options(tmp_path))
    assert result.result.status == "ok"
    assert result.result.output is not None and result.result.output.exists()
    assert result.result.remux_packet_match_rate == 1.0
    assert result.result.sync_error_ms is not None
    assert result.result.sync_error_ms <= THRESHOLDS["sync_error_ms"]
    assert not list((tmp_path / "outputs").glob("*.part"))


def test_cache_reuses_only_identical_input_and_strategy(fixtures, tmp_path):
    first = process_asset(fixtures.normal, _options(tmp_path))
    assert first.reused is False
    second = process_asset(fixtures.normal, _options(tmp_path))
    assert second.reused is True
    assert second.result.cache == "hit"
    # A reuse must publish the bytes under the same stable output path.
    assert second.result.output == first.result.output
    assert second.result.output is not None and second.result.output.exists()
    assert second.result.output == tmp_path / "outputs" / "normal_master.remux.mp4"

    changed = _options(tmp_path)
    changed.timeout_s = 31.0
    third = process_asset(fixtures.normal, changed)
    assert third.reused is False


def test_partial_output_is_non_zero(tmp_path, demo_dir):
    video_only = strip_audio(
        demo_dir / "inputs" / "normal_master.mp4", tmp_path / "video_only.mp4"
    )
    options = _options(tmp_path)
    options.track_map = [
        {"output": 0, "source": 0, "kind": "video", "required": True},
        {"output": 1, "source": 1, "kind": "audio", "required": True},
    ]
    result = process_asset(video_only, options)
    assert result.result.status == "partial"
    assert result.result.output is not None
    assert "partial_output" in [warning.code for warning in result.result.warnings]


def test_cli_demo_and_verify_pass(tmp_path):
    workdir = tmp_path / "demo"
    report = tmp_path / "report.json"
    code = main(
        [
            "demo",
            "--workdir",
            str(workdir),
            "--json",
            str(report),
            "--fresh",
        ]
    )
    assert code == 0, "demo must meet every acceptance threshold"
    payload = json.loads(report.read_text())
    assert payload["passed"] is True
    assert payload["summary"]["damaged_samples"] >= 3
    assert payload["summary"]["sync_error_ms"] <= 20.0
    assert payload["summary"]["recovery_rate"] >= 0.80
    assert payload["summary"]["p95_latency_ms"] <= 3000.0
    assert payload["summary"]["remux_packet_match_rate"] >= 0.99
    assert payload["exit_code"] == 0
    assert main(["verify", "--report", str(report)]) == 0


def test_process_without_markers_skips_sync_threshold(fixtures, tmp_path):
    inputs = tmp_path / "delivery"
    inputs.mkdir()
    report = tmp_path / "process.json"
    code = main(
        [
            "process",
            "--inputs",
            str(inputs),
            "--workdir",
            str(tmp_path),
        ]
    )
    assert code == 2, "an empty delivery folder is a usage error"

    # A real folder without sync markers: sync cannot be measured, so the
    # threshold is reported as not applicable rather than failed.
    import shutil

    shutil.copyfile(fixtures.normal, inputs / fixtures.normal.name)
    report_path = tmp_path / "batch.json"
    assert (
        main(
            [
                "process",
                "--inputs",
                str(inputs),
                "--workdir",
                str(tmp_path / "work"),
                "--json",
                str(report_path),
            ]
        )
        == 0
    )
    payload = json.loads(report_path.read_text())
    assert payload["thresholds"]["sync_error_ms"]["applicable"] is False
    assert payload["thresholds"]["damaged_samples"]["applicable"] is False
    assert payload["passed"] is True


def test_cli_usage_errors_use_exit_two(tmp_path):
    assert main(["process", "--inputs", str(tmp_path / "nope"), "--workdir", str(tmp_path)]) == 2
    assert main(["verify", "--report", str(tmp_path / "missing.json")]) == 2
    assert main(["process", "--inputs", str(tmp_path), "--workdir", str(tmp_path)]) == 2


def test_inputs_are_never_modified(fixtures):
    before = fixtures.normal.read_bytes()
    process_asset(fixtures.normal, _options(fixtures.normal.parent))
    assert fixtures.normal.read_bytes() == before


def test_module_entry_point_runs(tmp_path):
    completed = subprocess.run(
        [
            sys.executable,
            "-m",
            "bcastqc",
            "demo",
            "--workdir",
            str(tmp_path / "demo"),
            "--json",
            str(tmp_path / "demo" / "report.json"),
            "--fresh",
        ],
        cwd=Path(__file__).resolve().parents[1],
        capture_output=True,
        text=True,
    )
    assert completed.returncode == 0, completed.stderr[-2000:]
    assert "sync_error_ms" in completed.stdout
    assert "recovery_rate" in completed.stdout
    assert "p95_latency_ms" in completed.stdout
    assert "remux_packet_match_rate" in completed.stdout
    assert "damaged_samples" in completed.stdout
