"""Acceptance and regression tests for the region-media pipeline.

Every test builds its own fixtures through ``scripts/make_fixtures.py`` helpers
so the suite has no external file dependencies.  The acceptance test mirrors
the demo commands in the brief and asserts the documented tolerances.
"""

from __future__ import annotations

import json
import math
import shutil
import struct
import subprocess
import sys
from pathlib import Path

import numpy as np
import pytest

ROOT = Path(__file__).resolve().parents[1]
SRC = ROOT / "src"
SCRIPTS = ROOT / "scripts"
if str(SRC) not in sys.path:
    sys.path.insert(0, str(SRC))
if str(SCRIPTS) not in sys.path:
    sys.path.insert(0, str(SCRIPTS))

import make_fixtures  # noqa: E402

from region_media import assemble, checkpoint, clock, codec, metadata, normalize, packet  # noqa: E402
from region_media import pipeline  # noqa: E402
from region_media.errors import Code  # noqa: E402


# ---------------------------------------------------------------------------
# fixtures
# ---------------------------------------------------------------------------


@pytest.fixture(scope="session")
def fixture_root(tmp_path_factory: pytest.TempPathFactory) -> Path:
    root = tmp_path_factory.mktemp("fixtures")
    make_fixtures.build_good(root)
    make_fixtures.build_damaged(root)
    make_fixtures.build_boundary(root)
    return root


def run_cli(*args: str) -> tuple[int, dict]:
    """Run the real CLI entry point in-process and parse its JSON."""
    from region_media import cli

    captured: list[str] = []

    class _Capture:
        @staticmethod
        def write(text: str) -> int:
            captured.append(text)
            return len(text)

        @staticmethod
        def flush() -> None:
            return None

    original = sys.stdout
    sys.stdout = _Capture()  # type: ignore[assignment]
    try:
        code = cli.main(list(args))
    finally:
        sys.stdout = original
    payload = json.loads("".join(captured))
    return code, payload


def code_counts(payload: dict) -> dict[str, int]:
    return payload.get("errors", {})


# ---------------------------------------------------------------------------
# packet layer
# ---------------------------------------------------------------------------


def test_sequence_tracker_wrap_duplicate_reorder():
    tracker = packet.SequenceTracker(window=8)
    order = [0xFFFE, 0xFFFF, 0x0000, 0x0001]
    accepted = [tracker.observe(s)[0] for s in order]
    assert accepted == [True] * 4
    assert tracker.wraps >= 1
    assert tracker.observe(0x0001)[0] is False  # duplicate
    assert tracker.duplicates == 1
    accepted, reordered, _ = tracker.observe(0x0003)  # in order
    assert accepted and not reordered
    fresh = packet.SequenceTracker(window=8)
    for seq in (10, 11, 12):
        fresh.observe(seq)
    accepted, reordered, _ = fresh.observe(9)  # late arrival
    assert accepted and reordered
    assert fresh.reordered == 1


def test_sequence_tracker_reports_losses():
    tracker = packet.SequenceTracker(window=8)
    for s in range(0, 6):
        tracker.observe(s)
    tracker.observe(10)
    lost = tracker.flush_lost()
    assert lost == [6, 7, 8, 9]


def test_rtp_header_parsing_and_padding():
    payload = struct.pack("!BBHII", 0x80, 96, 7, 1000, 0x1234) + b"abcd"
    header = packet.parse_rtp_header(payload)
    assert header.sequence == 7 and header.timestamp == 1000
    assert header.header_bytes == 12
    with pytest.raises(ValueError):
        packet.parse_rtp_header(b"\x80\x60")


def test_audio_payload_truncation_is_flagged():
    samples = np.arange(960, dtype=np.float32).reshape(480, 2)
    full = packet.encode_audio_payload(samples)
    decoded = packet.decode_audio_payload(full, 2)
    assert decoded.truncated is False
    assert decoded.samples.shape == (480, 2)
    short = packet.decode_audio_payload(full[:-10], 2)
    assert short.truncated is True
    assert short.samples.shape[0] < 480


def test_truncated_record_resynchronises(fixture_root: Path):
    analysis = assemble.analyse_capture(
        fixture_root / "boundary" / "truncated_record"
    )
    codes = {d.code for d in analysis.diagnostics}
    assert Code.TRUNCATED_RECORD in codes
    # packets after the damaged record are still recovered
    assert analysis.accepted_packets == 49
    assert analysis.record_errors >= 1


# ---------------------------------------------------------------------------
# clock layer
# ---------------------------------------------------------------------------


def test_timestamp_unwrap_across_modulus():
    timeline = clock.Timeline(48000)
    start = 0xFFFFFF00
    events = [timeline.push((start + i * 480) & 0xFFFFFFFF) for i in range(4)]
    assert [e.wrap for e in events].count(True) == 1
    offsets = [e.sample_offset for e in events]
    assert offsets == [0, 480, 960, 1440]
    assert not any(e.discontinuity for e in events)


def test_timestamp_backwards_is_discontinuity():
    timeline = clock.Timeline(48000)
    timeline.push(48000 * 10)
    event = timeline.push(48000 * 2)
    assert event.discontinuity is True
    assert any(d.code is Code.TIMESTAMP_DISCONTINUITY for d in timeline.diagnostics)


def test_rtcp_drift_detected(fixture_root: Path):
    analysis = assemble.analyse_capture(fixture_root / "damaged")
    assert analysis.rtcp.available
    assert analysis.rtcp.drift_ppm is not None
    assert abs(analysis.rtcp.drift_ppm) > 500
    assert any(d.code is Code.CLOCK_DRIFT for d in analysis.diagnostics)


def test_rtcp_missing_is_recorded(fixture_root: Path):
    analysis = assemble.analyse_capture(fixture_root / "boundary" / "single")
    assert not analysis.rtcp.available
    assert any(d.code is Code.RTCP_MISSING for d in analysis.diagnostics)


def test_sender_report_roundtrip():
    body = struct.pack("!IIIIII", 1, 2, 3, 4000, 5, 6)
    blob = struct.pack("!BBH", 0x80, clock.RTCP_SR, (len(body) + 4) // 4 - 1) + body
    reports, diags = clock.parse_rtcp_sender_reports(blob)
    assert not diags
    assert reports[0].rtp_timestamp == 4000
    assert reports[0].ntp_seconds == 2


# ---------------------------------------------------------------------------
# assembly layer
# ---------------------------------------------------------------------------


def test_good_capture_is_lossless(fixture_root: Path):
    analysis = assemble.analyse_capture(fixture_root / "good")
    assert analysis.lost_packets == 0
    assert analysis.duplicate_packets == 0
    assert analysis.reordered_packets == 0
    assert analysis.source_samples == 144000


def test_damaged_capture_counts(fixture_root: Path):
    analysis = assemble.analyse_capture(fixture_root / "damaged")
    assert analysis.lost_packets == 5
    assert analysis.duplicate_packets == 3
    assert analysis.reordered_packets >= 1
    assert analysis.sequence_wraps == 1
    assert analysis.timestamp_wraps == 1


def test_reorder_assembler_fills_gap_with_silence(fixture_root: Path):
    stats = assemble.AssemblyStats()
    blocks = list(
        assemble.iter_source_blocks(
            fixture_root / "damaged", block_samples=4800, stats=stats
        )
    )
    assert stats.gap_samples == 2400  # 5 lost packets x 480 samples
    assert sum(b.samples.shape[0] for b in blocks) == 192000
    assert all(np.isfinite(b.samples).all() for b in blocks)


def test_assembler_overlap_is_trimmed():
    def make_event(index, seq, pos, value):
        return assemble.PacketEvent(
            index=index,
            sequence=seq,
            sequence_unwrapped=seq,
            timestamp=pos,
            timestamp_unwrapped=pos,
            wall_seconds=None,
            media_pos=pos,
            samples=np.full((480, 1), value, dtype=np.float32),
        )

    assembler = assemble.BlockAssembler(960, 1)
    blocks = assembler.push(make_event(0, 0, 0, 0.1))
    blocks += assembler.push(make_event(1, 1, 240, 0.9))
    blocks += assembler.flush()
    audio = np.concatenate([b.samples for b in blocks])
    assert audio.shape[0] == 720
    assert assembler.stats.overlap_samples == 240
    # 0..480 keeps the first packet; the overlapping tail is clipped and the
    # second packet's surviving samples begin at 480.
    assert audio[300, 0] == pytest.approx(0.1)
    assert audio[600, 0] == pytest.approx(0.9)


def test_position_window_reorder_is_exact_within_window():
    events = [
        assemble.PacketEvent(
            index=i,
            sequence=i & 0xFFFF,
            sequence_unwrapped=i,
            timestamp=i * 480,
            timestamp_unwrapped=i * 480,
            wall_seconds=None,
            media_pos=i * 480,
            samples=np.full((480, 1), np.float32(i)),
        )
        for i in range(120)
    ]
    rng = __import__("random").Random(3)
    order = list(range(120))
    for start in range(0, 120, 12):
        block = order[start : start + 12]
        rng.shuffle(block)
        order[start : start + 12] = block
    reorder = assemble.Reorderer(window_samples=200 * 480)  # 200 packets
    assembler = assemble.BlockAssembler(4800, 1)
    out: list = []
    for idx in order:
        for event in reorder.push(events[idx]):
            out.extend(assembler.push(event))
    for event in reorder.flush():
        out.extend(assembler.push(event))
    out.extend(assembler.flush())
    audio = np.concatenate([b.samples for b in out])[:, 0]
    expected = np.repeat(np.arange(120, dtype=np.float32), 480)
    assert np.array_equal(audio, expected)
    assert assembler.stats.dropped_packets == 0


def test_empty_capture_produces_no_blocks(fixture_root: Path):
    blocks = list(
        assemble.iter_source_blocks(fixture_root / "boundary" / "empty", block_samples=4800)
    )
    assert blocks == []


# ---------------------------------------------------------------------------
# normalization
# ---------------------------------------------------------------------------


def test_resample_sample_counts_are_exact():
    for src, dst in ((44100, 48000), (96000, 48000), (48000, 96000)):
        normalizer = normalize.ChunkNormalizer(src, dst, "stereo", "stereo")
        normalizer.prepare()
        n = src  # 1 second
        signal = np.zeros((n, 2), dtype=np.float32)
        signal[:, 0] = 0.1 * np.sin(2 * np.pi * 440 * np.arange(n) / src)
        out = [normalizer.process(signal[i : i + 1024]) for i in range(0, n, 1024)]
        out.append(normalizer.flush())
        total = sum(block.shape[0] for block in out)
        assert total == dst


def test_channel_matrix_targets():
    order51 = normalize.CANONICAL_LAYOUTS["5.1"]
    stereo = normalize.channel_matrix("5.1", "stereo")
    # LFE (index 3) must not leak into the stereo down-mix.
    assert np.allclose(stereo.weights[:, order51.index("LFE")], 0.0)
    # Centre is folded equally into both channels at 0.707.
    assert np.allclose(stereo.weights[:, order51.index("FC")], 0.70710678)
    # A 5.1 down-mix folds the back surrounds into the matching front channel.
    assert stereo.weights[0, order51.index("BL")] == pytest.approx(0.70710678)
    assert stereo.weights[1, order51.index("BR")] == pytest.approx(0.70710678)
    order71 = normalize.CANONICAL_LAYOUTS["7.1"]
    down71 = normalize.channel_matrix("7.1", "stereo")
    # Both back and side surrounds must reach the front pair in a 7.1 mix.
    for name, row in (("BL", 0), ("SL", 0), ("BR", 1), ("SR", 1)):
        assert down71.weights[row, order71.index(name)] == pytest.approx(0.70710678)
    assert np.allclose(down71.weights[:, order71.index("LFE")], 0.0)
    mono = normalize.channel_matrix("mono", "stereo")
    assert np.allclose(mono.weights[:, 0], math.sqrt(0.5))
    with pytest.raises(ValueError):
        normalize.channel_matrix("3.0", "stereo")


def test_unknown_source_layout_is_rejected(fixture_root: Path):
    code, payload = run_cli(
        "process",
        "--input",
        str(fixture_root / "boundary" / "unknown_layout"),
        "--out",
        str(Path(fixture_root).parent / "unknown-layout-out"),
    )
    assert code == 1
    assert Code.UNSUPPORTED_LAYOUT.value in payload["errors"]


def test_loudness_matches_reference_tones():
    rate = 48000
    t = np.arange(rate * 2) / rate
    for amplitude, expected in ((0.1, -20.0), (0.5, -6.0)):
        tone = np.stack([amplitude * np.sin(2 * np.pi * 1000 * t)] * 2, axis=1)
        measured = normalize.integrated_loudness(tone.astype(np.float32), rate)
        assert measured == pytest.approx(expected, abs=0.1)


def test_streaming_meter_matches_batch():
    rng = np.random.default_rng(11)
    rate = 48000
    audio = (0.05 * rng.standard_normal((rate * 2, 2))).astype(np.float32)
    batch = normalize.integrated_loudness(audio, rate)
    meter = normalize.LoudnessMeter(rate)
    for start in range(0, audio.shape[0], 777):
        meter.push(audio[start : start + 777])
    streamed, _ = meter.result()
    assert streamed == pytest.approx(batch, abs=1e-9)


def test_silence_and_nonfinite_are_bounded():
    rate = 48000
    silence = np.zeros((rate, 2), dtype=np.float32)
    assert normalize.integrated_loudness(silence, rate) is None
    assert normalize.true_peak_dbfs(silence) is None
    dirty = silence.copy()
    dirty[10, 0] = np.inf
    dirty[20, 1] = np.nan
    meter = normalize.LoudnessMeter(rate)
    meter.push(dirty)
    lufs, peak = meter.result()
    assert meter.nonfinite_samples == 2
    assert lufs is None and peak is None


def test_clipping_is_counted_against_peak_ceiling():
    rate = 48000
    t = np.arange(rate) / rate
    hot = np.stack([0.98 * np.sin(2 * np.pi * 100 * t)] * 2, axis=1).astype(np.float32)
    normalizer = normalize.ChunkNormalizer(
        rate, rate, "stereo", "stereo", gain_db=3.0, peak_ceiling_dbfs=-1.0
    )
    normalizer.prepare()
    out = normalizer.process(hot)
    assert normalizer.take_clipped() > 0
    assert float(np.abs(out).max()) <= 10 ** (-1.0 / 20.0) + 1e-6
    assert normalizer.take_clipped() == 0  # counter resets per chunk


def test_gain_respects_peak_ceiling():
    gain, limited = normalize.compute_gain(-30.0, -1.0, target_lufs=-16.0,
                                           peak_ceiling_dbfs=-1.0)
    assert limited is True
    assert gain == pytest.approx(0.0)


# ---------------------------------------------------------------------------
# codec
# ---------------------------------------------------------------------------


@pytest.mark.parametrize("layout,channels", [("mono", 1), ("stereo", 2), ("5.1", 6), ("7.1", 8)])
def test_codec_roundtrip_is_length_exact(tmp_path: Path, layout: str, channels: int):
    rate = 48000
    n = 48000
    signal = np.stack(
        [0.05 * np.sin(2 * np.pi * (220 * (c + 1)) * np.arange(n) / rate)
         for c in range(channels)],
        axis=1,
    ).astype(np.float32)
    path = tmp_path / f"{layout}.ogg"
    codec.encode_chunk(path, signal, rate, layout, bit_rate=256000)
    decoded = codec.decode_chunk(path, expected_samples=n, expected_layout=layout)
    assert decoded.samples_out == n
    assert decoded.layout == layout
    assert np.isfinite(decoded.samples).all()


def test_codec_fallback_rate_uses_aac(tmp_path: Path):
    assert codec.choose_codec(48000).codec == "libopus"
    assert codec.choose_codec(44100).codec == "aac"
    rate = 44100
    n = rate
    signal = np.stack([0.05 * np.sin(2 * np.pi * 440 * np.arange(n) / rate)] * 2, axis=1)
    path = tmp_path / "chunk.m4a"
    codec.encode_chunk(path, signal.astype(np.float32), rate, "stereo",
                       bit_rate=192000)
    decoded = codec.decode_chunk(path, expected_samples=n)
    assert decoded.samples_out == n


# ---------------------------------------------------------------------------
# metadata
# ---------------------------------------------------------------------------


def test_metadata_conflict_and_missing(fixture_root: Path):
    analysis = assemble.analyse_capture(fixture_root / "damaged")
    report, diags = metadata.inspect_video_metadata(analysis.videos)
    assert report.conflict_count >= 1
    assert any(d.code is Code.METADATA_CONFLICT for d in diags)
    # Original values are preserved rather than converted.
    assert report.preserved["color_space"] == "bt709"


def test_video_provenance_guard_detects_retiming(fixture_root: Path):
    analysis = assemble.analyse_capture(fixture_root / "damaged")
    report, _ = metadata.inspect_video_metadata(analysis.videos)
    copied = metadata.video_output_records(analysis.videos)
    assert metadata.timeline_shift_diagnostics(
        report,
        source_records=analysis.videos,
        output_records=copied,
        clock_rate=48000,
    ) == []
    tampered = [dict(copied[0], pts_samples=999), *copied[1:]]
    diags = metadata.timeline_shift_diagnostics(
        report,
        source_records=analysis.videos,
        output_records=tampered,
        clock_rate=48000,
    )
    assert diags and diags[0].code is Code.VIDEO_TIME_SHIFT


def test_metadata_missing_recorded(fixture_root: Path):
    analysis = assemble.analyse_capture(fixture_root / "boundary" / "single")
    report, diags = metadata.inspect_video_metadata(analysis.videos)
    assert report.records == 0
    assert any(d.code is Code.METADATA_MISSING for d in diags)


# ---------------------------------------------------------------------------
# checkpoint + resume
# ---------------------------------------------------------------------------


def test_checkpoint_hash_and_quarantine(fixture_root: Path, tmp_path: Path):
    out = tmp_path / "out"
    options = pipeline.ProcessOptions(
        input_dir=fixture_root / "good", out_dir=out, chunk_ms=1000
    )
    assert pipeline.process(options).exit_code == 0
    with checkpoint.ChunkIndex(out / checkpoint.INDEX_NAME) as index:
        records = index.records()
        assert len(records) == 3
        victim = records[1]
        with (out / victim.name).open("ab") as handle:
            handle.write(b"corruption")
        outcome = checkpoint.verify_committed(out, index)
        assert outcome.quarantined and outcome.quarantined[0]["index"] == 1
        assert not (out / victim.name).exists()
        assert (out / checkpoint.QUARANTINE_DIR / victim.name).exists()


def test_resume_after_interruption_redoes_only_missing_chunks(
    fixture_root: Path, tmp_path: Path
):
    out = tmp_path / "run"
    first = pipeline.ProcessOptions(
        input_dir=fixture_root / "good",
        out_dir=out,
        simulate_interrupt_after=2,
    )
    with pytest.raises(Exception) as excinfo:
        pipeline.process(first)
    assert "simulated interruption" in str(excinfo.value)
    assert not (out / "manifest.json").exists()

    resumed = pipeline.ProcessOptions(input_dir=fixture_root / "good", out_dir=out,
                                      resume=True)
    result = pipeline.process(resumed)
    assert result.exit_code == 0
    assert result.summary["reused_chunks"] == 2
    manifest = json.loads((out / "manifest.json").read_text())
    assert len(manifest["chunks"]) == 3
    assert manifest["resumed"] is True


def test_resume_detects_changed_input(fixture_root: Path, tmp_path: Path):
    out = tmp_path / "run"
    options = pipeline.ProcessOptions(
        input_dir=fixture_root / "good", out_dir=out, chunk_ms=1000
    )
    pipeline.process(options)
    capture = out / "capture.edg"
    assert not capture.exists()
    changed = tmp_path / "changed"
    shutil.copytree(fixture_root / "good", changed)
    with (changed / "capture.edg").open("ab") as handle:
        handle.write(b"\x00" * 4)
    from region_media.errors import InputError

    with pytest.raises(InputError):
        pipeline.process(
            pipeline.ProcessOptions(input_dir=changed, out_dir=out, resume=True)
        )


# ---------------------------------------------------------------------------
# acceptance (mirrors the demo commands)
# ---------------------------------------------------------------------------


def test_acceptance_good_fixture(fixture_root: Path, tmp_path: Path):
    out = tmp_path / "accept"
    code, payload = run_cli(
        "process",
        "--input",
        str(fixture_root / "good"),
        "--out",
        str(out),
        "--rate",
        "48000",
        "--layout",
        "stereo",
        "--lufs",
        "-16",
        "--chunk-ms",
        "1000",
        "--target-kbps",
        "256",
    )
    assert code == 0, payload
    assert payload["errors"].get(Code.LOST_PACKET.value, 0) == 0

    code, verify = run_cli(
        "verify", "--out", str(out), "--manifest", str(out / "manifest.json")
    )
    assert code == 0, verify
    required = {
        "target_kbps",
        "actual_kbps",
        "bitrate_deviation_pct",
        "chunks",
        "samples",
        "integrated_lufs",
        "true_peak_dbfs",
        "clipped_samples",
        "lost_packets",
        "reordered_packets",
        "metadata_conflicts",
        "manifest_alignment_ms",
    }
    assert required <= set(verify)
    assert abs(verify["bitrate_deviation_pct"]) <= 10.0
    assert verify["manifest_alignment_ms"] <= 2.0
    for key in ("integrated_lufs", "true_peak_dbfs"):
        assert verify[key] is not None and math.isfinite(verify[key])
    for key in ("clipped_samples", "lost_packets", "reordered_packets",
                "metadata_conflicts"):
        assert verify[key] >= 0 and math.isfinite(verify[key])


def test_acceptance_damaged_reports_counts(fixture_root: Path, tmp_path: Path):
    out = tmp_path / "damaged"
    code, payload = run_cli(
        "process",
        "--input",
        str(fixture_root / "damaged"),
        "--out",
        str(out),
        "--target-kbps",
        "256",
    )
    assert code == 0
    assert payload["lost_packets"] == 5  # five-packet burst loss
    assert payload["duplicate_packets"] == 3
    assert payload["reordered_packets"] == 1
    assert payload["metadata_conflicts"] >= 1
    code, verify = run_cli(
        "verify", "--out", str(out), "--manifest", str(out / "manifest.json")
    )
    assert code == 0
    assert verify["lost_packets"] == 5
    assert verify["reordered_packets"] >= 1
    assert abs(verify["bitrate_deviation_pct"]) <= 10.0
    assert verify["manifest_alignment_ms"] <= 2.0


def test_acceptance_boundary_wrap_and_short_tail(fixture_root: Path, tmp_path: Path):
    out = tmp_path / "wrap"
    code, payload = run_cli(
        "process",
        "--input",
        str(fixture_root / "boundary" / "wrap_short"),
        "--out",
        str(out),
        "--chunk-ms",
        "1000",
    )
    assert code == 0
    assert payload["chunks"] == 3  # 2 full seconds + 0.35 s tail
    manifest = json.loads((out / "manifest.json").read_text())
    last = manifest["chunks"][-1]
    assert last["samples"] == int(round(0.35 * 48000))
    assert payload["errors"].get(Code.TIMESTAMP_WRAP.value, 0) == 1
    code, verify = run_cli(
        "verify", "--out", str(out), "--manifest", str(out / "manifest.json")
    )
    assert code == 0
    assert verify["manifest_alignment_ms"] <= 2.0


def test_manifest_records_sequence_ranges(fixture_root: Path, tmp_path: Path):
    out = tmp_path / "seq"
    code, _ = run_cli(
        "process", "--input", str(fixture_root / "good"), "--out", str(out),
        "--chunk-ms", "1000", "--target-kbps", "256",
    )
    assert code == 0
    manifest = json.loads((out / "manifest.json").read_text())
    chunks = manifest["chunks"]
    assert len(chunks) == 3
    for chunk in chunks:
        assert chunk["source_sequence_start"] is not None
        assert chunk["source_sequence_end"] is not None
        assert (
            chunk["source_sequence_end"] - chunk["source_sequence_start"]
        ) == 99
        assert chunk["rtp_timestamp_end"] >= chunk["rtp_timestamp_start"]
    starts = [c["source_sequence_start"] for c in chunks]
    assert starts == sorted(starts)


def test_acceptance_empty_stream(fixture_root: Path, tmp_path: Path):
    out = tmp_path / "empty"
    code, payload = run_cli(
        "process", "--input", str(fixture_root / "boundary" / "empty"),
        "--out", str(out),
    )
    assert code == 0
    assert payload["chunks"] == 0
    assert Code.EMPTY_STREAM.value in payload["errors"]
    code, verify = run_cli(
        "verify", "--out", str(out), "--manifest", str(out / "manifest.json")
    )
    assert code == 0
    assert verify["chunks"] == 0
    assert verify["bitrate_applicable"] is False


def test_parameter_error_exit_code(fixture_root: Path, tmp_path: Path):
    code, payload = run_cli(
        "process",
        "--input",
        str(fixture_root / "good"),
        "--out",
        str(tmp_path / "o"),
        "--rate",
        "22050",
    )
    assert code == 2
    assert payload["status"] == "error"


def test_simulated_interrupt_exit_code(fixture_root: Path, tmp_path: Path):
    code, payload = run_cli(
        "process",
        "--input",
        str(fixture_root / "good"),
        "--out",
        str(tmp_path / "i"),
        "--simulate-interrupt-after",
        "1",
    )
    assert code == 3
    assert payload["status"] == "interrupted"
    assert "resume_hint" in payload


def test_entries_installed_and_script_runs():
    console = ROOT / ".venv" / "bin" / "region-media"
    if not console.exists():
        pytest.skip("console script not installed")
    result = subprocess.run(
        [str(console), "--version"], capture_output=True, text=True, check=False
    )
    assert result.returncode == 0
    assert "0.1.0" in result.stdout
