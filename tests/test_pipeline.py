"""端到端与单元测试：夹具随测试生成，不依赖外部文件，仅用标准库 unittest。

运行：
    .venv/bin/python -m unittest tests.test_pipeline -v
    .venv/bin/python tests/test_pipeline.py
"""
from __future__ import annotations

import os
import sys
import tempfile
import unittest
from fractions import Fraction

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))

from mediapipe import fixtures  # noqa: E402
from mediapipe.buffer import BackpressurePolicy, BoundedQueue, QueueFullAborted  # noqa: E402
from mediapipe.checkpoint import (  # noqa: E402
    CheckpointState,
    CheckpointStore,
    StreamState,
    file_fingerprint,
)
from mediapipe.cli import main as cli_main  # noqa: E402
from mediapipe.demux import Demuxer  # noqa: E402
from mediapipe.errors import (  # noqa: E402
    EXIT_MEDIA_CORRUPT,
    EXIT_OK,
    CheckpointIncompatibleError,
    MediaCorruptionError,
)
from mediapipe.mapping import StreamMapping, default_mapping, validate_mapping  # noqa: E402
from mediapipe.packet import PacketRecord, Payload  # noqa: E402
from mediapipe.pipeline import Pipeline, PipelineConfig  # noqa: E402
from mediapipe.timebase import from_microseconds, to_microseconds  # noqa: E402


class TimebaseTest(unittest.TestCase):
    def test_roundtrip(self):
        for tb in [(1, 48000), (1, 90000), (1, 12288)]:
            for ticks in (0, 1, 1024, 90000):
                self.assertEqual(from_microseconds(to_microseconds(ticks, tb), tb), ticks)

    def test_negative_preserved(self):
        self.assertLess(to_microseconds(-1024, (1, 48000)), 0)


class BoundedQueueTest(unittest.TestCase):
    def _rec(self, seq):
        return PacketRecord(
            0, seq, 0, 0, Fraction(1, 1000), 0, 0, 1, 1000, True, False,
            Payload(b"x" * 100, seq, seq + 100),
        )

    def test_block_and_abort(self):
        q = BoundedQueue(250, policy=BackpressurePolicy.BLOCK)
        q.put(self._rec(0)); q.put(self._rec(1))
        self.assertLessEqual(q.stats.peak_bytes, 250)
        qa = BoundedQueue(150, policy=BackpressurePolicy.ABORT)
        qa.put(self._rec(0))
        with self.assertRaises(QueueFullAborted):
            qa.put(self._rec(1))


class PipelineTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.dir = tempfile.mkdtemp(prefix="mediapipe-test-")
        cls.sample = os.path.join(cls.dir, "sample.mov")
        fixtures.generate_sample(cls.sample, frames=60)

    def test_demux_reports_streams(self):
        with Demuxer(self.sample) as dx:
            infos = dx.stream_infos
            records = list(dx.iter_packets())
        self.assertEqual(len(infos), 2)
        self.assertEqual({i.type for i in infos}, {"video", "audio"})
        self.assertTrue(all(i.packet_count > 0 for i in infos))
        self.assertEqual(len(records), sum(i.packet_count for i in infos))
        self.assertTrue(all(r.payload.source_offset >= 0 for r in records))

    def test_mapping_validation(self):
        with Demuxer(self.sample) as dx:
            infos = dx.stream_infos
        with self.assertRaises(Exception):
            validate_mapping([StreamMapping(99, 0, "video")], infos)
        bad = [
            StreamMapping(infos[0].index, 0, "audio"),
            StreamMapping(infos[1].index, 1, "video"),
        ]
        with self.assertRaises(Exception):
            validate_mapping(bad, infos)
        self.assertTrue(validate_mapping(default_mapping(infos), infos))

    def test_corruptions_exit_3_with_offsets(self):
        for corrupter in (
            fixtures.corrupt_truncated,
            fixtures.corrupt_box,
            fixtures.corrupt_packet,
            fixtures.corrupt_missing_index,
        ):
            dst = os.path.join(self.dir, f"bad_{corrupter.__name__}.mp4")
            corrupter(self.sample, dst)
            with self.assertRaises(MediaCorruptionError) as ctx:
                with Demuxer(dst) as dx:
                    list(dx.iter_packets())
            self.assertEqual(ctx.exception.exit_code, EXIT_MEDIA_CORRUPT)
            self.assertIsNotNone(ctx.exception.offset)
            self.assertGreaterEqual(ctx.exception.offset.start, 0)

    def test_lossless_pipeline_bounded(self):
        out = os.path.join(self.dir, "out.mov")
        ckpt = os.path.join(self.dir, "out.ckpt")
        with Demuxer(self.sample) as dx:
            mappings = default_mapping(dx.stream_infos)
        cfg = PipelineConfig(
            input_path=self.sample, output_path=out, checkpoint_path=ckpt,
            queue_bytes=1 << 16, queue_packets=8, slow_consumer_ms=1,
            checkpoint_every=20, verify=True,
        )
        result = Pipeline(cfg, mappings).run()
        self.assertEqual(result.exit_code, EXIT_OK)
        self.assertTrue(result.verification["packet_sha256_match"])
        self.assertLessEqual(result.queue["queue_peak_bytes"], 1 << 16)
        self.assertLessEqual(result.queue["queue_peak_packets"], 8)

    def test_resume_skips_committed_and_verifies(self):
        out = os.path.join(self.dir, "r.mov")
        ckpt = os.path.join(self.dir, "r.ckpt")
        with Demuxer(self.sample) as dx:
            mappings = default_mapping(dx.stream_infos)
        cfg = PipelineConfig(
            input_path=self.sample, output_path=out, checkpoint_path=ckpt,
            queue_bytes=1 << 20, checkpoint_every=25, verify=True,
        )
        first = Pipeline(cfg, mappings).run()
        self.assertEqual(first.exit_code, EXIT_OK)
        second = Pipeline(cfg, mappings).run()
        self.assertTrue(second.resumed)
        self.assertIsNotNone(second.resumed_from_sequence)
        self.assertTrue(second.verification["packet_sha256_match"])

    def test_checkpoint_rejects_changed_input(self):
        second = os.path.join(self.dir, "second.mov")
        fixtures.generate_sample(second, frames=30)
        ckpt = os.path.join(self.dir, "fp.ckpt")
        store = CheckpointStore(ckpt, input_path=self.sample)
        store.save(CheckpointState(
            version=1, input_fingerprint=file_fingerprint(self.sample),
            mapping_fingerprint="x", committed_sequence=5,
            stream_states={"0": StreamState()}, output_bytes=1234,
        ))
        with self.assertRaises(CheckpointIncompatibleError):
            store.verify_resume(store.load(), file_fingerprint(second), "x")

    def test_cli_truncated_returns_3(self):
        bad = os.path.join(self.dir, "cli_bad.mp4")
        fixtures.corrupt_truncated(self.sample, bad)
        import io
        import contextlib
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            code = cli_main([
                "remux", "--input", bad,
                "--output", os.path.join(self.dir, "x.mp4"),
                "--checkpoint", os.path.join(self.dir, "x.ckpt"),
            ])
        self.assertEqual(code, EXIT_MEDIA_CORRUPT)
        self.assertIn("source_error_offset", buf.getvalue())
        self.assertIn("phase", buf.getvalue())


if __name__ == "__main__":
    unittest.main(verbosity=2)
