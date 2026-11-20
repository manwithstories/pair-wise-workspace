"""Segment planning, bitrate-ladder selection, metrics and manifest."""

from __future__ import annotations

import json
import math
from pathlib import Path

import numpy as np
import pytest

from archiveflow.ladder import (build_ladder, select_representations,
                                simulate_bandwidth)
from archiveflow.cli import RecoverConfig, _enforce_output_limits
from archiveflow.manifest import (atomic_write_json, load_manifest,
                                  safe_segment_name, validate_manifest)
from archiveflow.errors import ParameterError, UnrecoverableError
from archiveflow.metrics import bitrate_error_pct, psnr_db, ssim
from archiveflow.segment import (FLAG_NO_KEYFRAME, FLAG_PLACEHOLDER,
                                 FLAG_SHORT_SEGMENT, FLAG_TRUNCATED_TAIL,
                                 SegmentSpec, mark_placeholder_segments,
                                 plan_segments)


class TestSegmentPlanning:
    def test_boundaries_are_exact_target_multiples(self):
        # keyframes every 1 s; target segment 2 s over 6 s of media.
        kfs = [float(i) * 1000.0 for i in range(7)]
        specs = plan_segments(6000.0, kfs, 2000.0, truncated=False)
        assert [s.start_ms for s in specs] == [0.0, 2000.0, 4000.0]
        assert all(abs(s.end_ms - s.start_ms - 2000.0) < 1e-9 for s in specs)
        assert all(s.flags == () for s in specs)

    def test_missing_keyframe_adds_explainable_flag(self):
        # Only a keyframe at origin; the boundary at 2 s has none nearby.
        specs = plan_segments(4000.0, [0.0], 2000.0, truncated=False)
        assert FLAG_NO_KEYFRAME in specs[1].flags

    def test_truncated_tail_flag_propagates(self):
        specs = plan_segments(3000.0, [0.0], 2000.0, truncated=True)
        assert FLAG_TRUNCATED_TAIL in specs[-1].flags

    def test_short_final_segment_flagged(self):
        # 2.5 s of media with 2 s target -> final 0.5 s segment is short.
        specs = plan_segments(2500.0, [0.0], 2000.0, truncated=False)
        assert FLAG_SHORT_SEGMENT in specs[-1].flags

    def test_rejects_non_positive_target_and_empty_media(self):
        with pytest.raises(ValueError):
            plan_segments(1000.0, [0.0], 0.0, False)
        assert plan_segments(0.0, [0.0], 1000.0, False) == []

    def test_placeholder_flag_only_on_affected_segments(self):
        specs = [
            SegmentSpec(0, 0.0, 2000.0, True),
            SegmentSpec(1, 2000.0, 4000.0, True),
        ]
        marked = mark_placeholder_segments(specs, [2500.0])
        assert FLAG_PLACEHOLDER not in marked[0].flags
        assert FLAG_PLACEHOLDER in marked[1].flags
        # No placeholders -> unchanged (and input list not mutated).
        assert mark_placeholder_segments(specs, []) is specs
        assert FLAG_PLACEHOLDER not in specs[1].flags


class TestLadder:
    def test_build_ladder_orders_and_sizes_representations(self):
        reps = build_ladder([2_500_000, 500_000, 1_200_000])
        assert [r.name for r in reps] == ["360p", "720p", "1080p"]
        assert reps[0].width < reps[1].width < reps[2].width

    def test_bandwidth_dip_damps_downshift_then_recovers(self):
        reps = build_ladder([500_000, 1_200_000, 2_500_000])
        bw = [3_000_000.0] * 2 + [100_000.0] * 3 + [3_000_000.0] * 4
        choices = select_representations(reps, len(bw), bw)
        # At most one tier down in a single step.
        for a, b in zip(choices, choices[1:]):
            assert b >= a - 1
        # The sustained dip reaches the lowest tier.
        assert min(choices) == 0
        # Upshift only after recovery; the stream climbs back, never instant.
        assert choices[-1] > choices[4]

    def test_simulated_bandwidth_is_deterministic(self):
        assert simulate_bandwidth(9, seed=5) == simulate_bandwidth(9, seed=5)


class TestMetrics:
    def test_identical_frames_psnr_capped_high_and_ssim_one(self):
        frame = np.random.default_rng(0).integers(0, 256, (16, 16, 3),
                                                  dtype=np.uint8)
        assert psnr_db(frame, frame) == 99.0
        assert ssim(frame, frame) == pytest.approx(1.0, abs=1e-9)

    def test_psnr_degrades_with_noise(self):
        ref = np.full((32, 32, 3), 128, dtype=np.uint8)
        noisy = np.clip(ref.astype(int) + 40, 0, 255).astype(np.uint8)
        value = psnr_db(ref, noisy)
        assert math.isfinite(value) and 10 < value < 40

    def test_bitrate_error_pct_signed_and_guarded(self):
        assert bitrate_error_pct(110, 100) == pytest.approx(10.0)
        assert bitrate_error_pct(90, 100) == pytest.approx(-10.0)
        assert bitrate_error_pct(90, 0) == 0.0


class TestManifest:
    def test_atomic_write_then_load_roundtrip(self, tmp_path: Path):
        target = tmp_path / "nested" / "manifest.json"
        payload = {"segments": [], "version": 1}
        atomic_write_json(target, payload)
        assert json.loads(target.read_text())["version"] == 1
        assert load_manifest(target)["segments"] == []
        # No leftover temp files.
        assert not list(tmp_path.rglob(".manifest-*.tmp"))

    def test_safe_name_rejects_traversal(self):
        assert safe_segment_name("seg_0.mp4") == "seg_0.mp4"
        for bad in ("../escape.mp4", "a/b.mp4", "", ".."):
            with pytest.raises(UnrecoverableError):
                safe_segment_name(bad)

    def test_validate_detects_missing_file_and_escape(self, tmp_path: Path):
        seg = tmp_path / "present.mp4"
        seg.write_bytes(b"x")
        good = {
            "segments": [
                {"file": "present.mp4", "duration_ms": 1000.0},
                {"file": "missing.mp4", "duration_ms": 1000.0},
            ],
            "representations": [{"name": "360p"}],
            "duration_ms": 2000.0,
        }
        problems = validate_manifest(good, tmp_path)
        assert any("missing.mp4" in p for p in problems)
        bad = {
            "segments": [{"file": "../escape.mp4", "duration_ms": 1000.0}],
            "representations": [{"name": "360p"}],
            "duration_ms": 1000.0,
        }
        problems = validate_manifest(bad, tmp_path)
        # Rejected by the name allowlist or, failing that, the resolve check.
        assert any("unsafe" in p or "escapes" in p for p in problems)

    def test_validate_rejects_empty_segments_and_bad_duration(
            self, tmp_path: Path):
        problems = validate_manifest(
            {"segments": [], "duration_ms": 0}, tmp_path)
        assert problems  # empty segments reported


class TestOutputLimits:
    def _cfg(self, **kw):
        return RecoverConfig(out_dir=Path("/tmp/unused"),
                             targets=[500_000], **kw)

    def _segments(self, n, dur_ms=4000.0):
        return [SegmentSpec(index=i, start_ms=i * dur_ms,
                            end_ms=(i + 1) * dur_ms, keyframe_start=True)
                for i in range(n)]

    def test_within_limits_passes(self):
        reps = build_ladder([500_000])
        cfg = self._cfg(max_segments=10, max_output_bytes=10 ** 9)
        _enforce_output_limits(cfg, self._segments(2), reps)  # no raise

    def test_segment_count_cap_rejected(self):
        reps = build_ladder([500_000, 1_200_000])  # 2 reps
        cfg = self._cfg(max_segments=3)
        with pytest.raises(ParameterError, match="segments"):
            _enforce_output_limits(cfg, self._segments(2), reps)  # 4 > 3

    def test_output_byte_cap_rejected(self):
        reps = build_ladder([2_000_000])
        # One 4 s segment at 2 Mbps projects to 1 MiB; cap below that.
        cfg = self._cfg(max_segments=100, max_output_bytes=500_000)
        with pytest.raises(ParameterError, match="MiB"):
            _enforce_output_limits(cfg, self._segments(1), reps)
