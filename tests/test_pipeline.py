"""端到端管线测试: 时间边界、混合模式、音频同步、确定性与内存模型。"""

from __future__ import annotations

import json
import tempfile
import unittest
from fractions import Fraction
from pathlib import Path

import numpy as np

from compositor.audio import analyze_audio, frame_start_sample, synthesize_pcm
from compositor.blend import (
    composite_layer,
    from_premultiplied,
    to_premultiplied,
)
from compositor.exporter import preview_digests
from compositor.renderer import FrameRenderer
from compositor.timeline import parse_timeline
from compositor.types import BlendMode, Timeline
from fixtures.generate import build_fixture


class TimeConversionTests(unittest.TestCase):
    def test_frame_second_roundtrip_exact(self):
        from compositor.types import frame_to_second, second_to_frame

        for fps in (24, 30, 60):
            for frame in (0, 1, 7, 119):
                t = frame_to_second(frame, fps)
                self.assertEqual(second_to_frame(t, fps), frame)
                self.assertIsInstance(t, Fraction)

    def test_sample_mapping_is_integer_and_monotonic(self):
        raw = build_fixture()
        timeline, report = parse_timeline(raw)
        self.assertTrue(report.to_dict()["ok"])
        samples = [frame_start_sample(f, timeline)
                   for f in range(timeline.frame_count)]
        self.assertTrue(all(isinstance(s, int) for s in samples))
        self.assertEqual(samples, sorted(samples))
        self.assertEqual(len(set(samples)), len(samples))
        self.assertEqual(samples[1] - samples[0],
                         timeline.sample_rate // timeline.fps)


class HalfOpenBoundaryTests(unittest.TestCase):
    def test_boundary_frame_not_duplicated(self):
        raw = build_fixture()
        timeline, _ = parse_timeline(raw)
        renderer = FrameRenderer(timeline)
        ids_at_59 = {l.layer_id for l in renderer.active_layers(59)}
        ids_at_60 = {l.layer_id for l in renderer.active_layers(60)}
        self.assertIn("hero_a", ids_at_59)
        self.assertNotIn("hero_a", ids_at_60)
        self.assertIn("hero_b", ids_at_60)
        self.assertNotIn("hero_b", ids_at_59)

    def test_layer_order_stable_by_track_then_declaration(self):
        raw = build_fixture()
        timeline, _ = parse_timeline(raw)
        renderer = FrameRenderer(timeline)
        ordered = renderer.active_layers(70)
        keys = [(l.track, l.order) for l in ordered]
        self.assertEqual(keys, sorted(keys))
        # 背景轨道 0 永远最先合成。
        self.assertEqual(ordered[0].layer_id, "background")

    def test_out_of_range_frame_raises(self):
        raw = build_fixture()
        timeline, _ = parse_timeline(raw)
        renderer = FrameRenderer(timeline)
        with self.assertRaises(Exception):
            renderer.render_frame(timeline.frame_count)
        with self.assertRaises(Exception):
            renderer.render_frame(-1)


class BlendModeTests(unittest.TestCase):
    def setUp(self):
        self.dst = np.zeros((3, 3, 4), dtype=np.float64)

    def test_source_over_half_alpha(self):
        src = np.zeros((3, 3, 4), dtype=np.float64)
        src[..., 0] = 1.0
        src[..., 3] = 0.5
        composite_layer(self.dst, src, BlendMode.SOURCE_OVER)
        np.testing.assert_allclose(self.dst[0, 0], [1.0, 0, 0, 0.5])

    def test_transparent_pixel_does_not_pollute_color(self):
        self.dst[..., 0] = 0.4
        self.dst[..., 3] = 0.8
        before = self.dst.copy()
        src = np.zeros((3, 3, 4), dtype=np.float64)
        src[..., 1] = 1.0  # alpha 保持 0
        for mode in BlendMode:
            composite_layer(self.dst, src, mode)
        # 零 alpha 图层为零操作(允许浮点末位误差)。
        np.testing.assert_allclose(self.dst, before, atol=1e-12)
        np.testing.assert_array_equal(self.dst[..., 3], before[..., 3])

    def test_premultiply_roundtrip(self):
        rgba = np.random.default_rng(0).random((4, 4, 4))
        rgba[..., 3] = np.linspace(0.01, 1, 16).reshape(4, 4)
        premul = to_premultiplied(rgba)
        back = from_premultiplied(premul[..., :3], rgba[..., 3:4])
        np.testing.assert_allclose(back, rgba, atol=1e-12)
        # 完全透明像素的预乘 RGB 必须为 0, 反预乘后颜色也归零(不携带颜色)。
        clear = rgba.copy()
        clear[..., 3] = 0.0
        self.assertTrue(np.all(to_premultiplied(clear)[..., :3] == 0.0))
        back_clear = from_premultiplied(to_premultiplied(clear)[..., :3],
                                        clear[..., 3:4])
        self.assertTrue(np.all(back_clear[..., :3] == 0.0))

    def test_add_and_screen_never_exceed_unit_alpha(self):
        self.dst[..., 3] = 0.9
        src = np.zeros((3, 3, 4), dtype=np.float64)
        src[..., 3] = 0.9
        src[..., :3] = 1.0
        composite_layer(self.dst, src, BlendMode.ADD)
        self.assertLessEqual(float(self.dst[..., 3].max()), 1.0)
        composite_layer(self.dst, src, BlendMode.SCREEN)
        self.assertLessEqual(float(self.dst[..., 3].max()), 1.0)
        self.assertTrue(np.all(self.dst[..., :3] <= 1.0))


class ClippingTests(unittest.TestCase):
    def test_offscreen_layer_does_not_overflow(self):
        raw = build_fixture()
        timeline, _ = parse_timeline(raw)
        renderer = FrameRenderer(timeline)
        # 负坐标粒子层在第 30 帧活跃, 渲染不得越界且画布尺寸稳定。
        frame = renderer.render_frame(30)
        self.assertEqual(frame.shape, (timeline.height, timeline.width, 4))
        self.assertEqual(frame.dtype, np.uint8)
        self.assertTrue(np.all(frame >= 0) and np.all(frame <= 255))


class AudioSyncTests(unittest.TestCase):
    def test_event_sample_ranges_and_max_error(self):
        raw = build_fixture()
        timeline, _ = parse_timeline(raw)
        analysis = analyze_audio(timeline)
        self.assertEqual(len(analysis), len(timeline.audio_events))
        by_id = {a.event_id: a for a in analysis}
        self.assertEqual(by_id["tone_0"].first_sample, 0)
        self.assertEqual(by_id["tone_half_offset"].av_sync_error_us, 16666)
        self.assertLessEqual(
            max(a.av_sync_error_us for a in analysis), 16700)
        for a in analysis:
            self.assertLessEqual(a.last_sample,
                                 timeline.sample_rate * 4 - 1)
            self.assertGreaterEqual(a.end_frame, 0)

    def test_pcm_deterministic_and_bounded(self):
        raw = build_fixture()
        timeline, _ = parse_timeline(raw)
        pcm_a = synthesize_pcm(timeline)
        pcm_b = synthesize_pcm(timeline)
        np.testing.assert_array_equal(pcm_a, pcm_b)
        self.assertTrue(np.all(np.abs(pcm_a) <= 1.0))
        self.assertEqual(len(pcm_a), timeline.sample_rate * 4)


class EndToEndTests(unittest.TestCase):
    def test_full_pipeline_digest_contract(self):
        raw = build_fixture()
        timeline, report = parse_timeline(raw)
        self.assertTrue(report.to_dict()["ok"])
        digests, summary, _ = preview_digests(timeline)
        self.assertEqual(summary.frame_count, 120)
        self.assertEqual([d.frame_index for d in digests], list(range(120)))
        for digest in digests:
            self.assertEqual(len(digest.pixel_sha256), 64)
            int(digest.pixel_sha256, 16)  # 必须是十六进制
            self.assertIsInstance(digest.opaque_pixels, int)
            self.assertGreaterEqual(digest.audio_start_sample, 0)
            self.assertGreaterEqual(digest.av_sync_error_us, 0)
        self.assertEqual(len(summary.summary_sha256), 64)
        self.assertLessEqual(summary.max_av_sync_error_us, 16700)

    def test_repeated_runs_identical_hashes(self):
        raw = build_fixture()
        timeline, _ = parse_timeline(raw)
        digests_a, summary_a, _ = preview_digests(timeline)
        digests_b, summary_b, _ = preview_digests(timeline)
        self.assertEqual([d.pixel_sha256 for d in digests_a],
                         [d.pixel_sha256 for d in digests_b])
        self.assertEqual(summary_a.summary_sha256,
                         summary_b.summary_sha256)

    def test_jsonl_output_and_summary_line(self):
        from compositor.exporter import export_jsonl

        raw = build_fixture()
        timeline, _ = parse_timeline(raw)
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp) / "digest.jsonl"
            summary, _ = export_jsonl(timeline, out)
            lines = out.read_text(encoding="utf-8").strip().splitlines()
        self.assertEqual(len(lines), 121)  # 120 帧 + 1 全局摘要
        for line in lines[:-1]:
            row = json.loads(line)
            self.assertEqual(row["kind"], "frame")
        self.assertEqual(json.loads(lines[-1])["kind"], "summary")
        self.assertEqual(json.loads(lines[-1])["summary_sha256"],
                         summary.summary_sha256)


if __name__ == "__main__":
    unittest.main()
