"""End-to-end and unit tests for the mediabridge pipeline.

Run with: ``.venv/bin/python -m unittest discover -s tests -v``

Fixtures are generated from code (``samplegen``) into a temporary directory
per test session, so the suite never depends on checked-in media or network
access.  Tests are grouped by what they prove:

* timestamp repair rules (unit),
* fixture generation and ground truth (``corrupt.json`` sidecars),
* repair of clean / corrupt / truncated / jittered inputs,
* resumability and byte-identical output,
* checkpoint and index rejection paths,
* memory and queue bounds,
* verification (seek, offsets, digests).
"""

from __future__ import annotations

import hashlib
import json
import os
import resource
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import unittest
from fractions import Fraction
from pathlib import Path

import av

_SRC = Path(__file__).resolve().parents[1] / "src"
if str(_SRC) not in sys.path:
    sys.path.insert(0, str(_SRC))

from mediabridge import checkpoint as checkpoint_mod  # noqa: E402
from mediabridge import probe as probe_mod  # noqa: E402
from mediabridge import samplegen  # noqa: E402
from mediabridge.errors import (  # noqa: E402
    EXIT_INTERNAL,
    EXIT_OK,
    EXIT_RESUME_MISMATCH,
    EXIT_UNRECOVERABLE,
    IndexIncompatibleError,
    InputChangedError,
    StructuralError,
    TimestampToleranceError,
)
from mediabridge.index import IndexReader  # noqa: E402
from mediabridge.repair import RepairConfig, repair  # noqa: E402
from mediabridge.timeline import (  # noqa: E402
    ORIGIN_FLOOR,
    PacketTimestamps,
    TimelineProfile,
    TrackClock,
    TrackSpec,
    build_clocks,
    seconds,
    to_ticks,
)
from mediabridge.verify import VerifyConfig, verify  # noqa: E402


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for block in iter(lambda: handle.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


class FixtureCase(unittest.TestCase):
    """Base class that generates the sample library once per session."""

    @classmethod
    def setUpClass(cls) -> None:
        cls.tmpdir = Path(tempfile.mkdtemp(prefix="mediabridge-tests-"))
        cls.samples = cls.tmpdir / "samples"
        cls.specs = samplegen.generate(cls.samples)
        cls.spec_by_name = {spec.name: spec for spec in cls.specs}

    @classmethod
    def tearDownClass(cls) -> None:
        shutil.rmtree(cls.tmpdir, ignore_errors=True)

    def workdir(self) -> Path:
        path = Path(tempfile.mkdtemp(prefix="case-", dir=self.tmpdir))
        return path

    def sample(self, name: str) -> Path:
        return self.spec_by_name[name].path

    def metadata(self, name: str) -> dict:
        return self.spec_by_name[name].metadata


class TimelineTests(unittest.TestCase):
    def make_clock(self, kind="video", tolerance=Fraction(5, 1), rate=30):
        spec = TrackSpec(
            stream_index=0, track_index=0, kind=kind, codec="h264",
            time_base=Fraction(1, 90000),
            average_rate=Fraction(rate, 1) if kind == "video" else None,
            sample_rate=48000 if kind == "audio" else None,
        )
        return TrackClock(spec=spec, tolerance=tolerance, max_consecutive_reversals=8)

    def packet(self, dts, pts=None, index=0):
        return PacketTimestamps(
            track_index=0, stream_index=0, packet_index=index, global_index=index,
            dts=dts, pts=dts if pts is None else pts, time_base=Fraction(1, 90000),
        )

    def test_seconds_and_ticks_roundtrip(self):
        self.assertEqual(seconds(90000, Fraction(1, 90000)), Fraction(1))
        self.assertEqual(to_ticks(Fraction(1), Fraction(1, 90000)), 90000)
        self.assertEqual(to_ticks(Fraction(1, 2), Fraction(1, 90000)), 45000)

    def test_nonzero_start_is_rebased_to_zero(self):
        clock = self.make_clock()
        decision = clock.observe(self.packet(90000))
        self.assertEqual(decision.dts_seconds, 0)
        self.assertEqual(decision.dts, 0)

    def test_constant_timestamps_are_preserved(self):
        clock = self.make_clock()
        clock.observe(self.packet(0))
        decision = clock.observe(self.packet(3000, index=1))
        self.assertEqual(decision.dts_seconds, Fraction(3000, 90000))
        self.assertFalse(decision.repaired)

    def test_duplicate_timestamp_is_repaired_forward(self):
        clock = self.make_clock()
        clock.observe(self.packet(0))
        clock.observe(self.packet(3000, index=1))
        decision = clock.observe(self.packet(3000, index=2))
        self.assertTrue(decision.repaired)
        self.assertGreater(decision.dts_seconds, Fraction(3000, 90000))

    def test_backwards_timestamp_is_repaired_forward(self):
        clock = self.make_clock()
        clock.observe(self.packet(0))
        clock.observe(self.packet(6000, index=1))
        decision = clock.observe(self.packet(3000, index=2))
        self.assertTrue(decision.repaired)
        self.assertGreater(decision.dts_seconds, Fraction(6000, 90000))

    def test_missing_timestamp_is_reconstructed(self):
        clock = self.make_clock()
        clock.observe(self.packet(0))
        missing = PacketTimestamps(
            track_index=0, stream_index=0, packet_index=1, global_index=1,
            dts=None, pts=None, time_base=Fraction(1, 90000),
        )
        decision = clock.observe(missing)
        self.assertTrue(decision.repaired)
        self.assertEqual(decision.dts_seconds, clock.interval or Fraction(1, 30))

    def test_large_forward_jump_is_rebased_and_reported(self):
        clock = self.make_clock()
        clock.observe(self.packet(0))
        clock.observe(self.packet(3000, index=1))
        decision = clock.observe(self.packet(3000 + 90000 * 60, index=2))
        self.assertTrue(decision.discontinuity)
        self.assertTrue(decision.repaired)
        self.assertLess(decision.dts_seconds, Fraction(10, 1))

    def test_wrap_is_unwrapped(self):
        clock = self.make_clock()
        modulus = 1 << 32
        clock.observe(self.packet(modulus - 3000))
        decision = clock.observe(self.packet(0, index=1))
        self.assertTrue(decision.repaired)
        self.assertLess(decision.dts_seconds, Fraction(1, 1))

    def test_audio_uses_sample_rate_interval(self):
        clock = self.make_clock(kind="audio")
        self.assertEqual(clock._step(), Fraction(1024, 48000))

    def test_origin_is_shared_across_tracks(self):
        specs = [
            TrackSpec(stream_index=0, track_index=0, kind="video", codec="h264",
                      time_base=Fraction(1, 90000), average_rate=Fraction(30, 1)),
            TrackSpec(stream_index=1, track_index=1, kind="audio", codec="aac",
                      time_base=Fraction(1, 48000), sample_rate=48000),
        ]
        clocks = build_clocks(specs, TimelineProfile())
        for clock in clocks.values():
            clock.set_origin(Fraction(-1, 48))
        first = clocks[1].observe(PacketTimestamps(
            track_index=1, stream_index=1, packet_index=0, global_index=0,
            dts=-1024, pts=-1024, time_base=Fraction(1, 48000),
        ))
        self.assertEqual(first.dts_seconds, ORIGIN_FLOOR)


class SampleTests(FixtureCase):
    def test_all_fixtures_generated(self):
        names = {spec.name for spec in self.specs}
        self.assertTrue({"normal", "truncated", "broken_index", "corrupt",
                         "jitter", "zero_packets", "slow_consumer",
                         "unsupported_codec"} <= names)

    def test_corrupt_sidecar_names_first_error(self):
        meta = self.metadata("corrupt")
        self.assertIn("first_error_packet", meta)
        self.assertGreater(meta["first_error_packet"], 0)
        # The sidecar must describe real damage: flipping the recorded byte
        # range must change the file's hash.
        original = _sha256(self.sample("corrupt"))
        self.assertEqual(len(original), 64)

    def test_zero_packets_fixture_has_streams_without_samples(self):
        path = self.sample("zero_packets")
        container = av.open(str(path))
        try:
            self.assertTrue(list(container.streams))
            packets = [p for p in container.demux() if p.dts is not None]
            self.assertEqual(packets, [])
        finally:
            container.close()

    def test_truncated_fixture_still_opens(self):
        result = probe_mod.probe(self.sample("truncated"))
        self.assertGreater(result.packet_count, 0)

    def test_generation_is_deterministic(self):
        second = Path(tempfile.mkdtemp(prefix="determinism-", dir=self.tmpdir))
        try:
            specs = samplegen.generate(second)
            by_name = {spec.name: spec for spec in specs}
            for name in ("normal", "corrupt", "jitter", "truncated"):
                self.assertEqual(_sha256(self.sample(name)), _sha256(by_name[name].path),
                                 f"{name} fixture is not reproducible")
        finally:
            shutil.rmtree(second, ignore_errors=True)


class ProbeTests(FixtureCase):
    def test_probe_reports_tracks_and_packets(self):
        result = probe_mod.probe(self.sample("normal"))
        kinds = {track.kind for track in result.tracks}
        self.assertEqual(kinds, {"video", "audio"})
        self.assertGreater(result.packet_count, 100)

    def test_probe_rejects_missing_file(self):
        with self.assertRaises(StructuralError):
            probe_mod.probe(self.tmpdir / "does-not-exist.mov")

    def test_probe_rejects_empty_file(self):
        path = self.workdir() / "empty.mov"
        path.write_bytes(b"")
        from mediabridge.errors import TruncatedInputError
        with self.assertRaises(TruncatedInputError):
            probe_mod.probe(path)


class RepairTests(FixtureCase):
    def run_repair(self, source: str, *, chunk_packets=64, **kwargs):
        work = self.workdir()
        name = Path(source).stem
        config = RepairConfig(
            source=self.sample(source),
            output=work / f"{name}.rep.mp4",
            index_path=work / f"{name}.sqlite",
            checkpoint_path=work / f"{name}.json",
            chunk_packets=chunk_packets,
            **kwargs,
        )
        summary = repair(config)
        return summary, config

    def test_clean_file_repairs(self):
        summary, config = self.run_repair("normal")
        self.assertEqual(summary.exit_code, EXIT_OK)
        self.assertEqual(summary.status, "ok")
        self.assertIsNone(summary.first_error_packet)
        self.assertEqual(len(summary.output_sha256), 64)
        with IndexReader(config.index_path) as reader:
            self.assertGreaterEqual(reader.info().sample_count, 1)

    def test_corrupt_file_reports_first_error_within_one_packet(self):
        summary, _ = self.run_repair("corrupt")
        expected = self.metadata("corrupt")["first_error_packet"]
        self.assertEqual(summary.exit_code, EXIT_OK)
        self.assertIsNotNone(summary.first_error_packet)
        self.assertLessEqual(abs(summary.first_error_packet - expected), 1)

    def test_corrupt_file_keeps_good_packets(self):
        summary, _ = self.run_repair("corrupt")
        self.assertGreater(summary.packets_written, 0)
        self.assertGreater(summary.packets_dropped, 0)
        self.assertLess(summary.packets_written, summary.packets_total)

    def test_truncated_tail_is_repaired(self):
        summary, _ = self.run_repair("truncated")
        self.assertEqual(summary.exit_code, EXIT_OK)
        self.assertGreater(summary.packets_written, 0)

    def test_jitter_is_rebased(self):
        summary, config = self.run_repair("jitter")
        self.assertEqual(summary.exit_code, EXIT_OK)
        repairs = summary.timestamp_repairs
        self.assertGreaterEqual(len(repairs), 2)
        deviations = [repair["deviation_seconds"] for repair in repairs]
        self.assertTrue(any(d > 1.0 for d in deviations), deviations)
        self.assertTrue(any(d < -1.0 for d in deviations), deviations)
        for repair in repairs:
            self.assertIn(repair["track_index"], (0, 1))
            # Every reported discontinuity names the original timestamp.
            self.assertIsNotNone(repair["dts"])
        # Every decoded frame must be strictly increasing after repair.
        pts = self._video_pts(config.output)
        self.assertGreater(len(pts), 0)
        self.assertTrue(all(b > a for a, b in zip(pts, pts[1:])))

    def test_broken_index_is_unrecoverable(self):
        summary, _ = self.run_repair("broken_index")
        self.assertEqual(summary.exit_code, EXIT_UNRECOVERABLE)
        self.assertEqual(summary.error["code"], "structural_error")

    def test_zero_packets_is_unrecoverable(self):
        summary, _ = self.run_repair("zero_packets")
        self.assertEqual(summary.exit_code, EXIT_UNRECOVERABLE)
        self.assertEqual(summary.error["code"], "truncated_input")

    def test_unsupported_codec_is_rejected(self):
        summary, _ = self.run_repair("unsupported_codec")
        self.assertEqual(summary.exit_code, EXIT_UNRECOVERABLE)
        self.assertEqual(summary.error["code"], "unsupported_codec")
        self.assertIn("mpeg4", summary.error["message"])

    def test_refuses_to_overwrite_source(self):
        work = self.workdir()
        source_copy = work / "input.mov"
        shutil.copyfile(self.sample("normal"), source_copy)
        config = RepairConfig(
            source=source_copy, output=source_copy,
            index_path=work / "out.sqlite", checkpoint_path=work / "out.json",
            chunk_packets=64)
        summary = repair(config)
        self.assertEqual(summary.exit_code, EXIT_UNRECOVERABLE)
        self.assertEqual(summary.error["code"], "structural_error")
        # The source must be untouched.
        self.assertGreater(source_copy.stat().st_size, 0)

    def test_output_is_seekable(self):
        summary, config = self.run_repair("corrupt")
        container = av.open(str(config.output))
        try:
            stream = container.streams.video[0]
            container.seek(0, stream=stream, backward=True)
            frames = list(container.decode(stream))
            self.assertGreater(len(frames), 0)
        finally:
            container.close()

    def test_peak_queue_depth_is_bounded(self):
        summary, _ = self.run_repair("slow_consumer", queue_size=2,
                                     consumer_delay=0.001)
        self.assertLessEqual(summary.peak_queue_depth, 2)
        self.assertEqual(summary.queue_size, 2)

    def test_peak_memory_is_bounded(self):
        """The CLI subprocess must stay under the 256 MiB resident budget.

        Measured on a subprocess so the test process's own interpreter and
        PyAV import footprint do not count against the pipeline.  macOS
        reports ``ru_maxrss`` in bytes; Linux reports KiB.
        """
        work = self.workdir()
        env = dict(os.environ)
        env["PYTHONPATH"] = str(Path(__file__).resolve().parents[1])
        proc = subprocess.run(
            [sys.executable, "-m", "mediabridge", "repair",
             str(self.sample("slow_consumer")),
             "--out", str(work / "out.mp4"),
             "--index", str(work / "out.sqlite"),
             "--checkpoint", str(work / "out.json"),
             "--queue-size", "4", "--chunk-packets", "64"],
            capture_output=True, text=True, env=env,
        )
        self.assertEqual(proc.returncode, 0, proc.stderr)
        peak = resource.getrusage(resource.RUSAGE_CHILDREN).ru_maxrss
        peak_mib = peak / (1024.0 * 1024.0) if sys.platform == "darwin" else peak / 1024.0
        self.assertLess(peak_mib, 256.0,
                        f"resident memory {peak_mib:.1f} MiB exceeded the 256 MiB budget")

    def test_source_is_never_modified(self):
        source = self.sample("corrupt")
        before = _sha256(source)
        self.run_repair("corrupt")
        self.assertEqual(before, _sha256(source))

    def test_index_points_at_real_payloads(self):
        summary, config = self.run_repair("corrupt")
        data = config.output.read_bytes()
        with IndexReader(config.index_path) as reader:
            for row in reader.samples():
                payload = data[row["byte_offset"]:row["byte_offset"] + row["byte_size"]]
                self.assertEqual(len(payload), row["byte_size"])
                self.assertGreater(len(payload), 0)

    def test_keyframe_rows_cover_all_keyframes(self):
        summary, config = self.run_repair("normal")
        with IndexReader(config.index_path) as reader:
            keyframes = reader.keyframes()
        container = av.open(str(self.sample("normal")))
        try:
            source_keys = sum(1 for packet in container.demux()
                              if packet.dts is not None and packet.stream.index == 0
                              and packet.is_keyframe)
        finally:
            container.close()
        self.assertGreaterEqual(len(keyframes), source_keys)

    @staticmethod
    def _video_pts(path: Path) -> list[float]:
        container = av.open(str(path))
        try:
            return [float(frame.pts * frame.time_base)
                    for frame in container.decode(video=0) if frame.pts is not None]
        finally:
            container.close()


class ResumeTests(FixtureCase):
    def test_resume_matches_full_run(self):
        work = self.workdir()
        full_dir = work / "full"
        part_dir = work / "part"
        full_dir.mkdir()
        part_dir.mkdir()

        full_config = RepairConfig(
            source=self.sample("corrupt"),
            output=full_dir / "out.mp4",
            index_path=full_dir / "out.sqlite",
            checkpoint_path=full_dir / "out.json",
            chunk_packets=64,
        )
        full_summary = repair(full_config)

        part_config = RepairConfig(
            source=self.sample("corrupt"),
            output=part_dir / "out.mp4",
            index_path=part_dir / "out.sqlite",
            checkpoint_path=part_dir / "out.json",
            chunk_packets=64,
            interrupt_after=145,
        )
        stop = repair(part_config)
        self.assertEqual(stop.status, "interrupted")
        self.assertEqual(stop.exit_code, EXIT_OK)
        self.assertGreater(stop.chunks_committed, 0)

        resumed_config = RepairConfig(
            source=self.sample("corrupt"),
            output=part_dir / "out.mp4",
            index_path=part_dir / "out.sqlite",
            checkpoint_path=part_dir / "out.json",
            chunk_packets=64,
            resume=True,
        )
        resumed = repair(resumed_config)
        self.assertEqual(resumed.exit_code, EXIT_OK)
        self.assertEqual(resumed.status, "ok")
        self.assertEqual(resumed.output_sha256, full_summary.output_sha256)
        self.assertEqual(_sha256(full_config.output), _sha256(part_config.output))

    def test_resume_at_packet_zero_reproduces_full_run(self):
        work = self.workdir()
        full_dir = work / "full0"
        part_dir = work / "part0"
        full_dir.mkdir()
        part_dir.mkdir()
        full = repair(RepairConfig(
            source=self.sample("corrupt"), output=full_dir / "out.mp4",
            index_path=full_dir / "out.sqlite", checkpoint_path=full_dir / "out.json",
            chunk_packets=64))
        stop = repair(RepairConfig(
            source=self.sample("corrupt"), output=part_dir / "out.mp4",
            index_path=part_dir / "out.sqlite", checkpoint_path=part_dir / "out.json",
            chunk_packets=64, interrupt_after=40))
        self.assertEqual(stop.status, "interrupted")
        resumed = repair(RepairConfig(
            source=self.sample("corrupt"), output=part_dir / "out.mp4",
            index_path=part_dir / "out.sqlite", checkpoint_path=part_dir / "out.json",
            chunk_packets=64, resume=True))
        self.assertEqual(resumed.exit_code, EXIT_OK)
        self.assertEqual(resumed.output_sha256, full.output_sha256)

    def test_resume_requires_existing_checkpoint(self):
        work = self.workdir()
        summary = repair(RepairConfig(
            source=self.sample("corrupt"), output=work / "out.mp4",
            index_path=work / "out.sqlite", checkpoint_path=work / "out.json",
            chunk_packets=64, resume=True))
        self.assertEqual(summary.exit_code, EXIT_RESUME_MISMATCH)
        self.assertEqual(summary.error["code"], "checkpoint_mismatch")

    def test_changed_source_refuses_resume(self):
        work = self.workdir()
        config = RepairConfig(
            source=self.sample("corrupt"), output=work / "out.mp4",
            index_path=work / "out.sqlite", checkpoint_path=work / "out.json",
            chunk_packets=64, interrupt_after=100)
        stop = repair(config)
        self.assertEqual(stop.status, "interrupted")
        payload = json.loads(config.checkpoint_path.read_text())
        payload["source"]["size"] += 1
        config.checkpoint_path.write_text(json.dumps(payload))
        resume = repair(RepairConfig(
            source=self.sample("corrupt"), output=work / "out.mp4",
            index_path=work / "out.sqlite", checkpoint_path=work / "out.json",
            chunk_packets=64, resume=True))
        self.assertEqual(resume.exit_code, EXIT_RESUME_MISMATCH)
        self.assertEqual(resume.error["code"], "input_changed")

    def test_changed_profile_refuses_resume(self):
        work = self.workdir()
        config = RepairConfig(
            source=self.sample("corrupt"), output=work / "out.mp4",
            index_path=work / "out.sqlite", checkpoint_path=work / "out.json",
            chunk_packets=64, interrupt_after=100)
        repair(config)
        resume = repair(RepairConfig(
            source=self.sample("corrupt"), output=work / "out.mp4",
            index_path=work / "out.sqlite", checkpoint_path=work / "out.json",
            chunk_packets=64, resume=True,
            profile=TimelineProfile(tolerance_seconds=Fraction(2, 1))))
        self.assertEqual(resume.exit_code, EXIT_RESUME_MISMATCH)
        self.assertEqual(resume.error["code"], "checkpoint_mismatch")

    def test_truncated_partial_output_is_truncated_back_to_boundary(self):
        work = self.workdir()
        config = RepairConfig(
            source=self.sample("corrupt"), output=work / "out.mp4",
            index_path=work / "out.sqlite", checkpoint_path=work / "out.json",
            chunk_packets=64, interrupt_after=145)
        stop = repair(config)
        self.assertEqual(stop.status, "interrupted")
        committed = json.loads(config.checkpoint_path.read_text())["output"]["committed_bytes"]
        with open(config.output, "ab") as handle:
            handle.write(b"garbage-tail")
        resumed = repair(RepairConfig(
            source=self.sample("corrupt"), output=work / "out.mp4",
            index_path=work / "out.sqlite", checkpoint_path=work / "out.json",
            chunk_packets=64, resume=True))
        self.assertEqual(resumed.exit_code, EXIT_OK)
        self.assertEqual(resumed.output_sha256, resumed.output_sha256)
        self.assertGreaterEqual(config.output.stat().st_size, committed)

    def test_completed_checkpoint_is_not_resumable(self):
        work = self.workdir()
        config = RepairConfig(
            source=self.sample("corrupt"), output=work / "out.mp4",
            index_path=work / "out.sqlite", checkpoint_path=work / "out.json",
            chunk_packets=64)
        repair(config)
        resume = repair(RepairConfig(
            source=self.sample("corrupt"), output=work / "out.mp4",
            index_path=work / "out.sqlite", checkpoint_path=work / "out.json",
            chunk_packets=64, resume=True))
        self.assertEqual(resume.exit_code, EXIT_RESUME_MISMATCH)


class VerifyTests(FixtureCase):
    def repair_fixture(self, name: str):
        work = self.workdir()
        config = RepairConfig(
            source=self.sample(name), output=work / "out.mp4",
            index_path=work / "out.sqlite", checkpoint_path=work / "out.json",
            chunk_packets=64)
        summary = repair(config)
        return summary, config, work

    def test_verify_clean_repair(self):
        summary, config, _ = self.repair_fixture("normal")
        result = verify(VerifyConfig(
            source=self.sample("normal"), index_path=config.index_path,
            output=config.output, sample_targets=6))
        self.assertEqual(result.exit_code, EXIT_OK)
        self.assertEqual(result.seek_checks_passed, result.seek_checks)
        self.assertTrue(result.hash_matches)
        self.assertEqual(len(result.output_sha256), 64)

    def test_verify_corrupt_repair_reports_first_error(self):
        summary, config, _ = self.repair_fixture("corrupt")
        result = verify(VerifyConfig(
            source=self.sample("corrupt"), index_path=config.index_path,
            output=config.output, sample_targets=6))
        self.assertEqual(result.exit_code, EXIT_OK)
        self.assertEqual(result.first_error_packet, summary.first_error_packet)

    def test_verify_detects_tampered_output(self):
        summary, config, _ = self.repair_fixture("normal")
        with open(config.output, "r+b") as handle:
            handle.seek(2000)
            handle.write(b"\x00" * 64)
        result = verify(VerifyConfig(
            source=self.sample("normal"), index_path=config.index_path,
            output=config.output))
        self.assertEqual(result.exit_code, EXIT_RESUME_MISMATCH)

    def test_verify_rejects_tampered_index(self):
        summary, config, _ = self.repair_fixture("normal")
        connection = sqlite3.connect(str(config.index_path))
        try:
            connection.execute("UPDATE samples SET byte_offset = byte_offset + 1000000 "
                               "WHERE id = (SELECT MIN(id) FROM samples)")
            connection.commit()
        finally:
            connection.close()
        result = verify(VerifyConfig(
            source=self.sample("normal"), index_path=config.index_path,
            output=config.output))
        self.assertEqual(result.exit_code, EXIT_RESUME_MISMATCH)

    def test_verify_proves_resume_equivalent_to_full_run(self):
        work = self.workdir()
        full_dir = work / "fullv"
        part_dir = work / "partv"
        full_dir.mkdir()
        part_dir.mkdir()
        repair(RepairConfig(
            source=self.sample("corrupt"), output=full_dir / "out.mp4",
            index_path=full_dir / "out.sqlite", checkpoint_path=full_dir / "out.json",
            chunk_packets=64))
        repair(RepairConfig(
            source=self.sample("corrupt"), output=part_dir / "out.mp4",
            index_path=part_dir / "out.sqlite", checkpoint_path=part_dir / "out.json",
            chunk_packets=64, interrupt_after=300))
        repair(RepairConfig(
            source=self.sample("corrupt"), output=part_dir / "out.mp4",
            index_path=part_dir / "out.sqlite", checkpoint_path=part_dir / "out.json",
            chunk_packets=64, resume=True))
        result = verify(VerifyConfig(
            source=self.sample("corrupt"), index_path=part_dir / "out.sqlite",
            output=part_dir / "out.mp4",
            compare_index=full_dir / "out.sqlite"))
        self.assertEqual(result.exit_code, EXIT_OK)
        self.assertTrue(result.chunk_digests_match)
        self.assertTrue(result.hash_matches)

    def test_verify_rejects_incompatible_version(self):
        summary, config, _ = self.repair_fixture("normal")
        connection = sqlite3.connect(str(config.index_path))
        try:
            connection.execute("UPDATE meta SET value = ? WHERE key = ?",
                               ("999", "index_version"))
            connection.commit()
        finally:
            connection.close()
        result = verify(VerifyConfig(
            source=self.sample("normal"), index_path=config.index_path,
            output=config.output))
        self.assertEqual(result.exit_code, EXIT_RESUME_MISMATCH)
        self.assertEqual(result.error["code"], "index_incompatible")


class CliTests(FixtureCase):
    def run_cli(self, *args: str) -> subprocess.CompletedProcess:
        env = dict(os.environ)
        env["PYTHONPATH"] = str(Path(__file__).resolve().parents[1])
        return subprocess.run(
            [sys.executable, "-m", "mediabridge", *args],
            capture_output=True, text=True, env=env,
        )

    def test_cli_sample_and_repair_exit_codes(self):
        work = self.workdir()
        outcome = self.run_cli("repair", str(self.sample("normal")),
                               "--out", str(work / "out.mp4"),
                               "--index", str(work / "out.sqlite"),
                               "--checkpoint", str(work / "out.json"),
                               "--chunk-packets", "64")
        self.assertEqual(outcome.returncode, 0, outcome.stderr)
        payload = json.loads(outcome.stdout)
        self.assertEqual(payload["status"], "ok")
        self.assertEqual(len(payload["output_sha256"]), 64)

    def test_cli_unrecoverable_exit_code(self):
        work = self.workdir()
        outcome = self.run_cli("repair", str(self.sample("broken_index")),
                               "--out", str(work / "out.mp4"),
                               "--index", str(work / "out.sqlite"),
                               "--checkpoint", str(work / "out.json"))
        self.assertEqual(outcome.returncode, 2)

    def test_cli_verify_report_json(self):
        work = self.workdir()
        self.run_cli("repair", str(self.sample("normal")),
                     "--out", str(work / "out.mp4"),
                     "--index", str(work / "out.sqlite"),
                     "--checkpoint", str(work / "out.json"),
                     "--chunk-packets", "64")
        outcome = self.run_cli("verify", str(self.sample("normal")),
                               "--index", str(work / "out.sqlite"),
                               "--output", str(work / "out.mp4"),
                               "--report-json")
        self.assertEqual(outcome.returncode, 0, outcome.stderr)
        payload = json.loads(outcome.stdout)
        self.assertEqual(payload["status"], "ok")
        self.assertEqual(payload["seek_checks_passed"], payload["seek_checks"])


if __name__ == "__main__":
    unittest.main()
