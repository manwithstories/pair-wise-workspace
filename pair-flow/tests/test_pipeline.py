"""端到端与单元测试：正常、截断、损坏输入全覆盖。

运行方式（不引入新依赖，标准库 unittest）：
    .venv/bin/python -m unittest tests.test_pipeline -v
或安装 pytest 后：
    .venv/bin/python -m pytest tests/
"""

from __future__ import annotations

import json
import os
import tempfile
import unittest

from relay_media import cli, report as report_mod
from relay_media.bitstream import CODECS, scan_bytes, scan_file
from relay_media.fixtures import (generate_audio, generate_video,
                                  truncate_file, write_index)
from relay_media.fingerprint import deduplicate, hamming_distance
from relay_media.keyframe_index import (IndexDocument, IndexEntry,
                                        rebuild_index, validate_index)
from relay_media.pipeline import Pipeline, PipelineConfig
from relay_media.segmenter import segment_stream
from relay_media.silence import detect_silence
from relay_media.timeline import ErrorKind, TimestampNormalizer
from relay_media.transport import simulate
from relay_media.bitstream import ScanResult, CodecUnit


class BitstreamTests(unittest.TestCase):
    def test_h264_annexb_offsets(self):
        stream = (b"\x00\x00\x00\x01\x67" + b"a" * 8 +
                  b"\x00\x00\x01\x65" + b"b" * 20)
        result = scan_bytes(stream, "h264")
        self.assertEqual([u.unit_type for u in result.units], [7, 5])
        # 4 字节起始码(4) + SPS 头(1) + 8 载荷 = 13
        self.assertEqual(result.keyframe_offsets(), [13])
        self.assertEqual(result.units[1].start_offset, 13)

    def test_truncated_and_corrupt_are_isolated(self):
        good = b"\x00\x00\x01\x65" + b"x" * 10
        truncated = scan_bytes(good[:6], "h264", stream_truncated=True)
        self.assertTrue(truncated.units[-1].truncated)
        self.assertTrue(any(
            e.kind == ErrorKind.TRUNCATED_UNIT for e in truncated.errors))
        corrupt = scan_bytes(b"\x00\x00\x01\x85" + b"y" * 5, "h264")
        self.assertTrue(corrupt.units[0].corrupt)

    def test_all_codecs_fixture_roundtrip(self):
        with tempfile.TemporaryDirectory() as tmp:
            for codec in CODECS:
                path = os.path.join(tmp, f"v.{codec}")
                gen = generate_video(path, codec, duration_s=2.0,
                                     fps=15, gop=5)
                scan = scan_file(path, codec)
                self.assertFalse(scan.errors)
                # 实际编码关键帧由编码器 GOP 策略决定，至少应出现 1 个
                self.assertGreaterEqual(len(scan.keyframe_offsets()), 1)


class TimelineTests(unittest.TestCase):
    def test_missing_wrap_nonmonotonic(self):
        norm = TimestampNormalizer(90_000, wrap_modulus=1 << 33)
        norm.feed(0, 0, 0)
        norm.feed(1, 3600, 100)
        point = norm.feed(2, None, 200)  # 缺失补点
        self.assertTrue(point.estimated)
        kinds = {e.kind for e in norm.errors}
        self.assertIn(ErrorKind.TIMESTAMP_MISSING, kinds)
        # 首帧即缺失 -> 无法换算，返回 None 且不入时间线
        fresh = TimestampNormalizer(90_000)
        self.assertIsNone(fresh.feed(0, None, 0))


class IndexTests(unittest.TestCase):
    def test_validate_rebuild_missing_page(self):
        stream = (b"\x00\x00\x01\x67" + b"a" * 8 +
                  b"\x00\x00\x01\x65" + b"b" * 16)
        scan = scan_bytes(stream, "h264")
        doc = IndexDocument("h264", [
            IndexEntry(offset=999),     # 越界
            IndexEntry(offset=4),       # 非关键帧
        ])
        validate_index(doc, scan)
        rebuild_index(doc, scan)
        sources = {e.offset: e.source for e in doc.entries}
        self.assertIn(scan.keyframe_offsets()[0], sources)
        self.assertTrue(any(
            e.kind == ErrorKind.INDEX_MISSING_PAGE for e in doc.errors))


class SegmenterTests(unittest.TestCase):
    def _scan(self, count, gop=5):
        units = [CodecUnit("h264", 5 if i % gop == 0 else 1,
                           i * 100, i * 100 + 3, (i + 1) * 100,
                           keyframe=(i % gop == 0)) for i in range(count)]
        return ScanResult("h264", units, [], count * 100, False)

    def test_keyframe_aligned_segments(self):
        scan = self._scan(20)
        pts = {u.start_offset: i * 100_000
               for i, u in enumerate(scan.units)}
        plan = segment_stream(
            scan, scan.keyframe_offsets(), target_duration_us=500_000,
            max_bytes=10_000, pts_by_offset=pts)
        self.assertEqual(len(plan.segments), 4)
        self.assertTrue(all(s.starts_on_keyframe for s in plan.segments))

    def test_byte_limit_degrades_boundary(self):
        scan = self._scan(20)
        pts = {u.start_offset: i * 100_000
               for i, u in enumerate(scan.units)}
        plan = segment_stream(
            scan, scan.keyframe_offsets(), target_duration_us=10_000_000,
            max_bytes=350, pts_by_offset=pts)
        self.assertTrue(any(
            e.kind == ErrorKind.DEGRADED_BOUNDARY for e in plan.errors))


class SilenceTests(unittest.TestCase):
    def test_detect_known_ranges_and_truncation(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "a.wav")
            audio = generate_audio(
                path, duration_s=6.0,
                silence_ranges_s=((1.0, 2.0), (4.0, 6.0)))
            result = detect_silence(path, min_duration_us=100_000)
            self.assertEqual(len(result.intervals), 2)
            for got, exp in zip(result.intervals,
                                audio.silence_ranges_us):
                self.assertLessEqual(abs(got.start_us - exp[0]), 25)
                self.assertLessEqual(abs(got.end_us - exp[1]), 25)
            trunc = detect_silence(
                path, expected_duration_us=audio.duration_us + 1_000_000)
            self.assertTrue(trunc.truncated)


class FingerprintTests(unittest.TestCase):
    def test_exact_and_near_duplicate(self):
        fps = [type("F", (), {"segment_index": i, "visual": v,
                              "temporal": 1, "source_offset": i * 10,
                              "frame_count": 1, "end_offset": i * 10 + 10,
                              "to_dict": lambda self: {}})()
               for i, v in enumerate((100, 100, 100 ^ 0b111))]
        # 门限 3：前两个完全重复，第三个汉明距离 3 判为近似重复
        result = deduplicate(fps, hamming_threshold=3)
        self.assertEqual(result.representatives, [0])
        self.assertTrue(result.duplicates[0].exact)
        self.assertFalse(result.duplicates[1].exact)
        self.assertEqual(hamming_distance(fps[0].visual, fps[2].visual), 3)
        # 门限 0（仅完全重复）时第三个成为独立代表
        strict = deduplicate(fps, hamming_threshold=0)
        self.assertEqual(strict.representatives, [0, 2])


class TransportTests(unittest.TestCase):
    def test_deterministic_and_thresholds(self):
        first = simulate(200_000, loss_burst=0.08, seed=12789).metrics()
        second = simulate(200_000, loss_burst=0.08, seed=12789).metrics()
        self.assertEqual(first, second)
        self.assertGreaterEqual(first["recovery_rate"], 0.90)
        self.assertLessEqual(first["p95_latency_ms"], 250)
        self.assertLessEqual(first["av_sync_error_ms"], 40)

    def test_high_loss_leaves_unrecovered(self):
        result = simulate(120_000, loss_burst=0.45, seed=11)
        self.assertTrue(result.unrecovered_ranges)
        self.assertEqual(len(result.errors), len(result.unrecovered_ranges))


class PipelineCliTests(unittest.TestCase):
    def _demo_env(self, tmp, codec="h264"):
        video = os.path.join(tmp, f"v.{codec}")
        audio = os.path.join(tmp, "a.wav")
        index = os.path.join(tmp, "index.json")
        generate_video(video, codec, duration_s=6.0, fps=25, gop=10)
        generate_audio(audio, duration_s=6.0,
                       silence_ranges_s=((1.0, 2.0),))
        full_bytes = os.path.getsize(video)
        truncate_file(video, 4096)
        scan = scan_file(video, codec, stream_truncated=True)
        write_index(index, codec, scan.keyframe_offsets()[:1])
        workdir = os.path.join(tmp, "work")
        cfg = PipelineConfig(
            codec=codec, workdir=workdir, truncate_at=4096,
            transport_bytes=full_bytes)
        return video, audio, index, cfg

    def test_full_pipeline_normal_truncated(self):
        with tempfile.TemporaryDirectory() as tmp:
            video, audio, index, cfg = self._demo_env(tmp)
            result = Pipeline(cfg).run(video, audio, index)
            self.assertEqual(
                [s["name"] for s in result["stages"]],
                ["parse", "index", "timeline", "segment", "silence",
                 "fingerprint", "transport"])
            self.assertTrue(result["error_offsets"])
            report = report_mod.build_report(result)
            verdict = report_mod.verify_report(report)
            self.assertTrue(verdict.ok)

    def test_deterministic_rerun_and_resume(self):
        with tempfile.TemporaryDirectory() as tmp:
            video, audio, index, cfg = self._demo_env(tmp)
            first = Pipeline(cfg).run(video, audio, index)
            os.remove(os.path.join(cfg.workdir,
                                   "pipeline.checkpoint.json"))
            second = Pipeline(cfg).run(video, audio, index)
            self.assertEqual(
                json.dumps(first, sort_keys=True),
                json.dumps(second, sort_keys=True))
            resumed = Pipeline(cfg).run(video, audio, index, resume=True)
            self.assertEqual(resumed, first)

    def test_cli_exit_codes(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = os.path.join(tmp, "report.json")
            code = cli.main([
                "demo", "--codec", "h264", "--duration", "6",
                "--loss-burst", "0.08", "--truncate-at", "4096",
                "--output", out])
            self.assertEqual(code, cli.EXIT_OK)
            self.assertEqual(cli.main(["verify", out]), cli.EXIT_OK)
            self.assertEqual(cli.main(
                ["demo", "--codec", "h264", "--duration", "-1",
                 "--output", out]), cli.EXIT_USAGE)
            self.assertEqual(cli.main(
                ["verify", os.path.join(tmp, "missing.json")]),
                cli.EXIT_USAGE)
            bad_out = os.path.join(tmp, "bad.json")
            high = cli.main([
                "demo", "--codec", "h264", "--duration", "6",
                "--loss-burst", "0.5", "--truncate-at", "4096",
                "--output", bad_out])
            self.assertEqual(high, cli.EXIT_UNRECOVERABLE)


if __name__ == "__main__":
    unittest.main()
