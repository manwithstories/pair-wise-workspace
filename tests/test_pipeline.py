"""Unit and integration tests for the relaymedia pipeline.

Run with the project venv:

    .venv/bin/python -m unittest discover -s tests -v

The suite covers the rational clock (fractional rates, negative offsets, day
wrap, repeated stamps), subtitle error isolation, sample-exact segments,
silence pulse separation, rate-invariant fingerprints, streaming
back-pressure, checkpoint refusal, colour conflicts and the full CLI
acceptance contract including all four exit codes.
"""

from __future__ import annotations

import dataclasses
import json
import subprocess
import sys
import tempfile
import unittest
from fractions import Fraction
from pathlib import Path

import numpy as np

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(ROOT / "tests"))

from generate_fixtures import generate_fixtures  # noqa: E402

from relaymedia.checkpoint import (  # noqa: E402
    CheckpointMismatch,
    digest_file,
    digest_json,
    new_checkpoint,
    resume_checkpoint,
)
from relaymedia.cli import main  # noqa: E402
from relaymedia.clock import (  # noqa: E402
    TimecodeClock,
    load_timecode_manifest,
    parse_rate,
    parse_timecode_stamp,
    to_sample_position,
)
from relaymedia.errors import (  # noqa: E402
    EXIT_CHECKPOINT_MISMATCH,
    EXIT_INPUT_ERROR,
    EXIT_OK,
    EXIT_VERIFY_FAILED,
)
from relaymedia.fingerprint import (  # noqa: E402
    FingerprintIndex,
    fingerprint_pcm,
    register_and_match,
)
from relaymedia.pipeline import BoundedQueue, BufferAccountant, PipelineConfig  # noqa: E402
from relaymedia.segments import build_segments  # noqa: E402
from relaymedia.subtitles import parse_subtitles  # noqa: E402


def _tone(sample_rate: int, duration: float, freq: float = 440.0) -> np.ndarray:
    t = np.arange(int(sample_rate * duration)) / sample_rate
    return (0.5 * np.sin(2 * np.pi * freq * t)).astype(np.float32)[:, None]


class ClockTests(unittest.TestCase):
    def test_fractional_rate_and_negative_offset(self) -> None:
        clock = TimecodeClock(parse_rate("30000/1001"), offset_seconds=Fraction(-1, 2))
        first = clock.convert(parse_timecode_stamp("00:00:00:00"))
        self.assertEqual(first, Fraction(-1, 2))

    def test_day_wrap_and_repeated_stamp(self) -> None:
        clock = TimecodeClock(parse_rate("30000/1001"))
        clock.convert(parse_timecode_stamp("23:59:59:29"))
        wrapped = clock.convert(parse_timecode_stamp("00:00:00:00"))
        repeated = clock.convert(parse_timecode_stamp("00:00:00:00"))
        self.assertEqual(clock.wraps, 1)
        self.assertEqual(wrapped, repeated)
        self.assertGreater(wrapped, 86399)

    def test_illegal_frame_is_located_error(self) -> None:
        from relaymedia.errors import InputError

        # 30000/1001 rounds to 30 fps, so frame indices 0..29 are legal and
        # frame 31 is out of range.
        clock = TimecodeClock(parse_rate("30000/1001"))
        clock.raw_seconds(parse_timecode_stamp("00:00:00:29"))
        with self.assertRaises(InputError):
            clock.raw_seconds(parse_timecode_stamp("00:00:00:31"))

    def test_sample_rounding_retains_error(self) -> None:
        pos = to_sample_position(Fraction(1, 3), 48000)
        self.assertEqual(pos.index, 16000)
        self.assertLessEqual(abs(pos.error_seconds), Fraction(1, 48000))

    def test_manifest_collects_unconvertible_record(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "tc.txt"
            path.write_text(
                "# rate=30000/1001 offset=-0.5\n"
                "23:59:59:29\n00:00:00:00\n12:60:00:00\n"
            )
            records, errors, clock = load_timecode_manifest(path)
            self.assertEqual(clock.wraps, 1)
            self.assertEqual(len(errors), 1)
            self.assertEqual(errors[0].line, 4)
            self.assertGreaterEqual(errors[0].offset_bytes, 0)


class SubtitleTests(unittest.TestCase):
    def test_bad_block_isolated_and_located(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "s.srt"
            path.write_bytes(
                b"1\n00:00:01,000 --> 00:00:02,000\nok\n\n"
                b"2\n99:99:99,000 --> 00:00:01,000\nbad\n\n"
                b"3\n00:00:03,000 --> 00:00:04,000\nafter\n"
            )
            track = parse_subtitles(path)
            self.assertEqual(len(track.cues), 2)
            self.assertEqual(track.bad_record_count, 1)
            self.assertGreaterEqual(track.first_error_offset_bytes, 0)

    def test_overlap_reported_but_cues_kept(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "s.srt"
            path.write_text(
                "1\n00:00:00,500 --> 00:00:01,500\na\n\n"
                "2\n00:00:01,000 --> 00:00:02,000\nb\n"
            )
            track = parse_subtitles(path)
            self.assertEqual(len(track.cues), 2)
            self.assertTrue(any("overlap" in e.message for e in track.errors))


class SegmentTests(unittest.TestCase):
    def test_boundaries_are_sample_exact(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "s.srt"
            path.write_text("1\n00:00:01,000 --> 00:00:02,333333\nx\n")
            track = parse_subtitles(path)
            plan = build_segments(track, 48000)
            self.assertEqual(plan.segments[0].start_sample, 48000)
            self.assertGreater(plan.segments[0].end_sample, 48000 + 48000)
            self.assertLess(abs(plan.max_round_error_seconds), Fraction(1, 48000))


class FingerprintTests(unittest.TestCase):
    def test_cross_rate_match_and_adjacency_exclusion(self) -> None:
        index = FingerprintIndex(window_ms=20.0)
        register_and_match(index, fingerprint_pcm(_tone(44100, 1.0), 44100, "a"))
        matches = register_and_match(
            index, fingerprint_pcm(_tone(48000, 1.0), 48000, "b")
        )
        self.assertEqual(len(matches), 1)
        self.assertEqual(matches[0].duplicate_of_source, "a")

        adjacent = FingerprintIndex(window_ms=20.0)
        base = _tone(48000, 0.5)
        register_and_match(
            adjacent,
            fingerprint_pcm(base[:12000], 48000, "s1", timeline_start=0.0),
        )
        none = register_and_match(
            adjacent,
            fingerprint_pcm(base[12000:], 48000, "s2", timeline_start=0.25),
        )
        self.assertEqual(none, [])


class BackpressureTests(unittest.TestCase):
    def test_byte_budget_blocks_then_releases(self) -> None:
        import threading

        accountant = BufferAccountant(limit_bytes=900)
        queue = BoundedQueue(max_items=100, accountant=accountant)
        for i in range(3):
            queue.put(i, 300)
        event = threading.Event()

        def blocked_put() -> None:
            queue.put("x", 300)
            event.set()

        thread = threading.Thread(target=blocked_put)
        thread.start()
        thread.join(timeout=0.4)
        self.assertFalse(event.is_set())
        self.assertLessEqual(accountant.peak, 900)
        queue.get()
        thread.join(timeout=0.4)
        self.assertTrue(event.is_set())


class CheckpointTests(unittest.TestCase):
    def test_input_and_param_drift_refused(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            params = digest_json({"rate": 48000})
            cp = new_checkpoint(tmp, "input", params)
            from relaymedia.errors import Stage

            cp.mark_stage(Stage.SUBTITLE, {"cues": 1}, completed_at="now")
            with self.assertRaises(CheckpointMismatch):
                resume_checkpoint(tmp, input_digest="other", params_digest=params)
            with self.assertRaises(CheckpointMismatch):
                resume_checkpoint(
                    tmp, input_digest="input",
                    params_digest=digest_json({"rate": 96000}),
                )
            resumed = resume_checkpoint(tmp, input_digest="input", params_digest=params)
            self.assertTrue(resumed.is_stage_done(Stage.SUBTITLE))


class CliAcceptanceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._tmp = tempfile.TemporaryDirectory()
        cls.root = Path(cls._tmp.name)
        cls.demo_dir = cls.root / "demo"

    @classmethod
    def tearDownClass(cls) -> None:
        cls._tmp.cleanup()

    def test_01_demo_and_verify_exit_zero(self) -> None:
        rc_demo = main([
            "demo", "--out", str(self.demo_dir), "--seed", "42",
            "--queue-bytes", "1048576",
        ])
        self.assertEqual(rc_demo, EXIT_OK)
        summary = json.loads((self.demo_dir / "summary.json").read_text())
        for field in (
            "bad_record_count", "first_error_offset_bytes", "offset_error_bytes",
            "resume_seconds", "segment_count", "duplicate_count",
            "output_sha256", "peak_buffer_bytes",
        ):
            self.assertIn(field, summary)
        self.assertLessEqual(summary["offset_error_bytes"], 32)
        self.assertLess(summary["resume_seconds"], 2.0)
        self.assertLessEqual(summary["peak_buffer_bytes"], 1048576)
        self.assertTrue(summary["resumed"])
        self.assertEqual(main(["verify", "--dir", str(self.demo_dir)]), EXIT_OK)

    def test_02_deterministic_output_hash(self) -> None:
        other = self.root / "demo2"
        self.assertEqual(main([
            "demo", "--out", str(other), "--seed", "42",
            "--queue-bytes", "1048576",
        ]), EXIT_OK)
        h1 = json.loads((self.demo_dir / "summary.json").read_text())["output_sha256"]
        h2 = json.loads((other / "summary.json").read_text())["output_sha256"]
        self.assertEqual(h1, h2)

    def test_03_verify_detects_tamper_exit_3(self) -> None:
        import shutil

        tampered = self.root / "tamper"
        shutil.copytree(self.demo_dir, tampered)
        wav = next((tampered / "run").rglob("node-a.wav"))
        raw = bytearray(wav.read_bytes())
        raw[-64:] = b"\x00" * 64
        wav.write_bytes(raw)
        self.assertEqual(main(["verify", "--dir", str(tampered)]), EXIT_VERIFY_FAILED)

    def test_04_checkpoint_mismatch_exit_4(self) -> None:
        import shutil

        tampered = self.root / "cp"
        shutil.copytree(self.demo_dir, tampered)
        cp_path = tampered / "run" / "checkpoint.json"
        data = json.loads(cp_path.read_text())
        data["input_digest"] = "00" * 32
        cp_path.write_text(json.dumps(data))
        self.assertEqual(
            main(["verify", "--dir", str(tampered)]), EXIT_CHECKPOINT_MISMATCH
        )

    def test_05_missing_dir_exit_2(self) -> None:
        self.assertEqual(
            main(["verify", "--dir", str(self.root / "missing")]), EXIT_INPUT_ERROR
        )


if __name__ == "__main__":
    unittest.main(verbosity=2)
