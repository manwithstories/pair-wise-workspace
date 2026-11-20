"""Unit tests for the timeline, manifest, audio and index components."""

from __future__ import annotations

from fractions import Fraction

import numpy as np
import pytest

from archive_media.audio import AudioConfig, process_array
from archive_media.audio_loudness import (
    LoudnessMeter,
    compute_gain_db,
    expected_samples,
)
from archive_media.errors import Code, ManifestSyntaxError
from archive_media.index import (
    KeyframeIndex,
    scan_decodable_points,
    verify_hit_rate,
)
from archive_media.manifest import parse_dash, parse_hls, parse_manifest, render_plan
from archive_media.timeline import TimelinePlanner, within_tolerance
from archive_media.verify import roundtrip_consistency

from fixture_media import encode_adts, tone_stereo

CONFIG = AudioConfig(48000, "stereo", target_lufs=-18.0)


def _two_segments(directory):
    for index in range(2):
        encode_adts(directory / f"s{index}.aac",
                    tone_stereo(48000, 1.0, 440.0), 48000)


def test_timeline_detects_reorder_and_collision():
    planner = TimelinePlanner(Fraction(1, 90000))
    planner.add(0, 0, 1)
    planner.add(1, 1, 1)
    planner.add(2, Fraction(1) + Fraction(1, 10**9), 1)
    assert Code.TIMESTAMP_COLLISION in {d.code for d in planner.diagnostics}
    planner2 = TimelinePlanner(Fraction(1, 90000))
    planner2.add(0, 0, 1)
    planner2.add(1, 2, 1)
    planner2.add(2, 1, 1)
    assert Code.REORDERED_REFERENCE in {d.code for d in planner2.diagnostics}
    assert within_tolerance(Fraction(1, 1000), Fraction(1, 1000), 1)


def test_manifest_rejects_escape_and_schemes(tmp_path):
    manifest = tmp_path / "p.m3u8"
    for bad in ("../escape.aac", "https://host/seg.aac", "/etc/passwd"):
        manifest.write_text(f"#EXTM3U\n#EXTINF:1,\n{bad}\n")
        with pytest.raises(ManifestSyntaxError) as exc:
            parse_manifest(manifest)
        assert exc.value.code == Code.REFERENCE_ESCAPE


def test_manifest_round_trip_preserves_timing(tmp_path):
    _two_segments(tmp_path)
    manifest = tmp_path / "p.m3u8"
    manifest.write_text(
        "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#ARCHIVE-REP:r0\n"
        "#EXTINF:1.0,\ns0.aac\n#EXTINF:1.0,\ns1.aac\n"
    )
    plan = parse_manifest(manifest)
    reparsed = parse_hls(render_plan(plan), manifest, tmp_path)
    before = [(s.sequence, str(s.start), str(s.duration), s.raw_ref)
              for s in plan.segments]
    after = [(s.sequence, str(s.start), str(s.duration), s.raw_ref)
             for s in reparsed.segments]
    assert before == after
    score, diags = roundtrip_consistency(plan)
    assert score == 1.0 and not diags


def test_dash_round_trip_preserves_template_and_synthesized_t(tmp_path):
    tick = 48000
    xml = (
        '<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><AdaptationSet>'
        '<SegmentTemplate timescale="48000" '
        'media="dash-damaged/dash-seg-$RepresentationID$-$Number$.m4s" '
        'startNumber="0"><SegmentTimeline>'
        f'<S t="0" d="{tick}"/><S t="{tick}" d="{tick}"/>'
        f'<S d="{tick}"/><S t="{3 * tick}" d="{tick}"/>'
        "</SegmentTimeline></SegmentTemplate>"
        '<Representation id="r0"/></AdaptationSet></MPD>'
    )
    manifest = tmp_path / "p.mpd"
    manifest.write_text(xml)
    plan = parse_manifest(manifest)
    assert plan.media_template.startswith("dash-damaged/")
    reparsed = parse_dash(render_plan(plan), manifest, tmp_path)
    before = [(s.sequence, s.raw_ref, str(s.start), s.synthesized)
              for s in plan.segments]
    after = [(s.sequence, s.raw_ref, str(s.start), s.synthesized)
             for s in reparsed.segments]
    assert before == after
    score, diags = roundtrip_consistency(plan)
    assert score == 1.0 and not diags


def test_missing_extinf_is_synthesized_and_flagged(tmp_path):
    _two_segments(tmp_path)
    manifest = tmp_path / "p.m3u8"
    manifest.write_text(
        "#EXTM3U\n#EXT-X-TARGETDURATION:1\n"
        "#EXTINF:1.0,\ns0.aac\ns1.aac\n"
    )
    plan = parse_manifest(manifest)
    assert plan.segments[1].synthesized
    assert Code.MISSING_INDEX_ENTRY in {
        d.code for d in plan.all_diagnostics()
    }


def test_resampling_and_exact_sample_count():
    rate = 44100
    tone = tone_stereo(rate, 1.0, 440.0)
    out, meta = process_array(
        tone, rate, "stereo", CONFIG, duration_seconds=Fraction(1, 1)
    )
    assert out.shape == (48000, 2)
    assert meta["boundary_error_samples"] == 0
    assert np.isfinite(out).all()


def test_chunked_meter_matches_whole_stream():
    rate = 48000
    tone = tone_stereo(rate, 2.0, 523.0)
    whole = LoudnessMeter(rate, 2)
    whole.push(tone)
    chunked = LoudnessMeter(rate, 2)
    for offset in range(0, len(tone), 7000):
        chunked.push(tone[offset:offset + 7000])
    assert whole.result()[0] == pytest.approx(chunked.result()[0], abs=1e-9)


def test_illegal_layout_is_rejected_with_track():
    with pytest.raises(Exception) as exc:
        process_array(
            np.zeros((100, 3), np.float32), 48000, "9.2", CONFIG,
            source_track="tape-reel-7",
        )
    assert exc.value.code == Code.UNSUPPORTED_LAYOUT
    assert exc.value.detail["source_track"] == "tape-reel-7"


def test_expected_samples_exact():
    assert expected_samples(Fraction(1, 3), 48000) == 16000


def test_gain_backs_off_for_peak_ceiling():
    # Loudness would ask for +10 dB but that crosses the -1 dBFS ceiling.
    gain, limited = compute_gain_db(
        -30.0, -5.0, target_lufs=-20.0, peak_ceiling_dbfs=-1.0
    )
    assert limited
    # Back-off gain = ceiling(-1) - peak(-5) = +4 dB, not the +10 LUFS asks for.
    assert gain == pytest.approx(4.0)
    # Loudness-driven case stays under the ceiling and keeps the full gain.
    gain2, limited2 = compute_gain_db(
        -25.0, -20.0, target_lufs=-18.0, peak_ceiling_dbfs=-1.0
    )
    assert not limited2 and gain2 == pytest.approx(7.0)
    # Silence never produces gain.
    gain3, limited3 = compute_gain_db(
        None, None, target_lufs=-18.0, peak_ceiling_dbfs=-1.0
    )
    assert gain3 == 0.0 and not limited3


def test_keyframe_scan_and_tolerance_hit_rate():
    pts = [Fraction(i, 4) for i in range(8)]
    offsets = [i * 1024 for i in range(8)]
    entries, _ = scan_decodable_points(
        1, pts, offsets, [False] * 8, explicit_index=[Fraction(0)]
    )
    promoted = sorted(e.pts_seconds for e in entries)
    assert promoted == pts[:3]
    index = KeyframeIndex("r0", entries)
    rate, misses = verify_hit_rate(index, promoted, 1.0)
    assert rate == 1.0 and not misses
    full_entries, diags = scan_decodable_points(1, pts, offsets, [False] * 8)
    assert len(full_entries) == 8 and diags
    full_index = KeyframeIndex("r0", full_entries)
    full_rate, full_misses = verify_hit_rate(full_index, pts, 1.0)
    assert full_rate == 1.0 and not full_misses
    far_rate, far_misses = verify_hit_rate(full_index, [Fraction(10)], 5.0)
    assert far_rate == 0.0 and far_misses[0].code == Code.INDEX_MISS
