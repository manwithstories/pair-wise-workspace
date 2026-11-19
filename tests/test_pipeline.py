"""Integration and unit tests for media_batch."""

import json
import shutil
import tempfile
import unittest
from fractions import Fraction
from pathlib import Path

import numpy as np

from media_batch import analyze, cli, fixtures, pipeline, probe, subtitle, timestamps
from media_batch.errors import ErrorLog, MediaBatchError
from media_batch.index import CheckpointIndex, ChunkRecord, verify_chunks
from media_batch.timebase import FPS_2997, MonotonicClock


class SubtitleTests(unittest.TestCase):
    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.dir, True)

    def test_srt_errors_and_offsets(self):
        body = ("1\n00:00:00,000 --> 00:00:01,000\nok one\n\n"
                "2\n00:02:00,000 --> 00:01:59,000\nreversed\n\n"
                "3\n\n"
                "4\n00:00:00,500 --> 00:00:02,000\noverlaps one\n\n"
                "5\n00:00:03,000 --> 00:00:04,000\nok two\n")
        p = self.dir / "t.srt"
        p.write_text(body)
        log = ErrorLog()
        cues = subtitle.parse_srt(p, log)
        self.assertEqual([c.index for c in cues], [0, 3, 4])
        codes = [e.code for e in log]
        self.assertEqual(codes, ["invalid_time", "truncated_record", "overlap"])
        data = p.read_bytes()
        for e in log:  # every offset is exact, not merely near the right block
            if e.code == "truncated_record":
                # the byte just before the offset is the end of the cut record
                self.assertEqual(data[e.source_offset - 1:e.source_offset], b"\n")
            else:
                self.assertIn(data[e.source_offset:e.source_offset + 1],
                              b"0123456789")
        inv = log.by_code("invalid_time")[0]
        self.assertEqual(data[inv.source_offset:inv.source_offset + 2], b"00")
        trunc = log.by_code("truncated_record")[0]
        self.assertEqual(data[trunc.source_offset:trunc.source_offset + 1], b"\n")

    def test_vtt_errors(self):
        p = self.dir / "t.vtt"
        p.write_text("WEBVTT\n\n1\n00:00.000 --> 00:01.000\na\n\n"
                     "2\n00:0x.000 --> 00:02.000\nb\n")
        log = ErrorLog()
        cues = subtitle.parse_vtt(p, log)
        self.assertEqual(len(cues), 1)
        self.assertEqual(log.by_code("invalid_time")[0].record_index, 1)


class TimecodeTests(unittest.TestCase):
    def test_drop_frame_and_wrap(self):
        self.assertEqual(timestamps.timecode_to_frames("00:00:00;00", FPS_2997), 0)
        self.assertEqual(timestamps.timecode_to_frames("00:01:00;02", FPS_2997), 1800)
        self.assertEqual(timestamps.timecode_to_frames("00:00:01;00", FPS_2997), 30)
        with self.assertRaises(ValueError):
            timestamps.timecode_to_frames("00:01:00;00", FPS_2997)  # dropped number
        log = ErrorLog()
        series = timestamps.unwrap_timecodes(
            ["23:59:59;29", "00:00:00;00", "00:00:01;00", "00:00:01;00"], FPS_2997, log)
        self.assertEqual(series.wraps, 1)
        self.assertTrue(all(b > a for a, b in
                            zip(series.frames[:2], series.frames[1:3])))
        self.assertEqual(len(log.by_code("timestamp_duplicate")), 1)

    def test_regression_not_sorted(self):
        log = ErrorLog()
        series = timestamps.unwrap_timecodes(
            ["00:00:05;00", "00:00:02;00", "00:00:06;00"], FPS_2997, log)
        self.assertEqual(len(log.by_code("timestamp_regression")), 1)
        self.assertEqual(series.frames[1] < series.frames[0], True)  # kept as-is

    def test_drift(self):
        rate = 48000
        anchors = [i * rate for i in range(11)]  # 10 one-second anchors
        # packet clock runs 200 ms/min fast
        pkt = [Fraction(a, rate) * Fraction(60200, 60000) for a in anchors]
        rep = timestamps.estimate_drift(anchors, pkt, rate)
        self.assertFalse(rep.trusted)
        self.assertTrue(rep.untrusted)
        ok = timestamps.estimate_drift(anchors, [Fraction(a, rate) for a in anchors], rate)
        self.assertTrue(ok.trusted)

    def test_monotonic_clock(self):
        log = ErrorLog()
        clock = MonotonicClock(log)
        self.assertTrue(clock.observe(100))
        self.assertFalse(clock.observe(50))
        self.assertEqual(len(log.by_code("timestamp_regression")), 1)


class AnalyzeTests(unittest.TestCase):
    def test_silence_threshold_and_merge(self):
        rate = 48000
        tone = (np.sin(2 * np.pi * 440 * np.arange(rate) / rate) * 10000).astype(np.int16)
        gap_long = np.zeros(int(rate * 0.5), dtype=np.int16)
        gap_short = np.zeros(int(rate * 0.15), dtype=np.int16)
        x = np.concatenate([tone, gap_long, tone, gap_short, tone])
        intervals = analyze.detect_silence(x, rate)
        self.assertEqual(len(intervals), 1)
        s, e = intervals[0]
        self.assertGreaterEqual(e - s, 300)
        self.assertLess(abs(s - 1000), 40)

    def test_fingerprint_gain_and_padding(self):
        rate = 48000
        t = np.arange(rate) / rate
        tone = np.sin(2 * np.pi * 550 * t)
        a = (tone * 0.3 * 32767).astype(np.int16)
        b = np.concatenate([np.zeros(4800, dtype=np.int16),
                            (tone * 0.1 * 32767).astype(np.int16)])
        fpa, fpb = analyze.audio_fingerprint(a, rate), analyze.audio_fingerprint(b, rate)
        self.assertGreaterEqual(analyze.fingerprint_similarity(fpa, fpb), 0.95)
        other = (np.sin(2 * np.pi * 900 * t) * 0.3 * 32767).astype(np.int16)
        fp_other = analyze.audio_fingerprint(other, rate)
        self.assertLess(analyze.fingerprint_similarity(fpa, fp_other), 0.90)
        mapping = analyze.find_duplicates([fpa, fp_other, fpb])
        self.assertEqual(mapping, {2: 0})


class IndexTests(unittest.TestCase):
    def test_commit_verify_rollback(self):
        d = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, d, True)
        out = d / "out.bin"
        out.write_bytes(b"aaaa" * 100 + b"bbbb" * 100)
        idx = CheckpointIndex(d / "i.db")
        idx.commit_chunk(ChunkRecord(0, "x", 0, 400,
                                     __import__("hashlib").sha256(b"aaaa" * 100).hexdigest(), 1))
        idx.commit_chunk(ChunkRecord(1, "y", 400, 400,
                                     __import__("hashlib").sha256(b"bbbb" * 100).hexdigest(), 2))
        valid, corrupt = verify_chunks(idx.committed_chunks(), out)
        self.assertEqual(len(valid), 2)
        data = bytearray(out.read_bytes())
        data[500] ^= 0xFF
        out.write_bytes(bytes(data))
        valid, corrupt = verify_chunks(idx.committed_chunks(), out)
        self.assertEqual((len(valid), corrupt.seq), (1, 1))
        self.assertEqual(idx.rollback_from(1), 1)
        idx.close()


class PipelineE2ETests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.dir = Path(tempfile.mkdtemp())
        fixtures.generate(cls.dir)
        cls.log = ErrorLog()

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.dir, True)

    def test_1_probe(self):
        info = probe.stream_info(self.dir / "input" / "program.mp4")
        self.assertTrue(info.has_audio and info.has_video)
        pkts = probe.probe_packets(self.dir / "input" / "program.mp4", ErrorLog())
        self.assertTrue(probe.detect_vfr(pkts))
        self.assertTrue(all(p.pos >= 0 for p in pkts[:10]))
        log = ErrorLog()
        pkts = probe.probe_packets(self.dir / "input" / "truncated.mp4", log)
        self.assertTrue(pkts)
        bad = log.by_code("bad_packet")
        self.assertEqual(len(bad), 1)
        self.assertGreater(bad[0].source_offset, 0)
        with self.assertRaises(MediaBatchError):
            probe.probe_packets(self.dir / "input" / "broken.mp4", ErrorLog())

    def test_1b_missing_audio_and_drift(self):
        log = ErrorLog()
        info = probe.stream_info(self.dir / "input" / "video_only.mp4", log)
        self.assertFalse(info.has_audio)
        self.assertEqual(len(log.by_code("missing_audio_track")), 1)
        log = ErrorLog()
        anchors, times, rate = probe.audio_clock_anchors(
            self.dir / "input" / "drift.mka", log)
        self.assertEqual(rate, 48000)
        report = timestamps.estimate_drift(anchors, times, rate)
        self.assertFalse(report.trusted)
        self.assertGreater(float(report.ms_per_minute), 100.0)
        self.assertTrue(report.untrusted)

    def test_2_interrupt_resume_verify(self):
        rc = cli.main(["demo", "--workdir", str(self.dir), "--interrupt-after", "2"])
        self.assertEqual(rc, 2)
        rc = cli.main(["resume", "--workdir", str(self.dir), "--json"])
        self.assertEqual(rc, 0)
        report = json.loads((self.dir / "output" / "manifest.json").read_text())
        self.assertEqual(len(report["output_sha256"]), 64)
        self.assertGreaterEqual(len(report["subtitle_errors"]), 3)
        self.assertLess(report["recovery_seconds"], 3.0)
        self.assertEqual(report["reprocessed_chunks"][0], 2)
        # two deliberate duplicates: cue 80 == cue 30, cue 95 == cue 40
        self.assertEqual(len(report["duplicates"]), 2)
        plan = json.loads((self.dir / "work" / "plan.json").read_text())
        seq_to_text = {p["seq"]: p["text"] for p in plan}
        texts = {seq_to_text[int(dup)] for dup in report["duplicates"]}
        self.assertEqual(texts, {"cue 80 text", "cue 95 text"})
        self.assertTrue(any(e["source_offset"] >= 0
                            for e in report["subtitle_errors"]))
        codes = {e["code"] for e in report["errors"]}
        for expected in ("invalid_time", "truncated_record", "overlap",
                         "bad_packet", "missing_audio_track", "bad_container",
                         "untrusted_drift"):
            self.assertIn(expected, codes)
        drift = {d["source"].split("/")[-1]: d for d in report["drift"]}
        self.assertTrue(drift["program.mp4"]["trusted"])
        self.assertFalse(drift["drift.mka"]["trusted"])
        rc = cli.main(["verify", "--workdir", str(self.dir)])
        self.assertEqual(rc, 0)
        rc = cli.main(["verify", "--workdir", str(self.dir), "--corrupt-at", "4096"])
        self.assertEqual(rc, 2)

    def test_3_rollback_on_corrupt_checkpoint(self):
        # corrupt the PCM temp file inside a committed chunk, then resume:
        # only the corrupt chunk and later uncommitted ones are recomputed
        idx = CheckpointIndex(self.dir / "work" / "index.db")
        recs = idx.committed_chunks()
        idx.close()
        self.assertGreaterEqual(len(recs), 7)
        target = recs[5]
        pcm = self.dir / "work" / "output.pcm"
        data = bytearray(pcm.read_bytes())
        data[target.output_offset] ^= 0xFF
        pcm.write_bytes(bytes(data))
        report = pipeline.run(self.dir, resume=True)
        self.assertEqual(report["reprocessed_chunks"][0], target.seq)
        self.assertEqual(report["rolled_back_chunks"],
                         len(recs) - recs.index(target))

    def test_4_resume_is_idempotent(self):
        # A completed run resumed again recomputes nothing and writes no
        # extra audio: the checkpoint table and PCM must line up exactly.
        pcm = self.dir / "work" / "output.pcm"
        before = pcm.stat().st_size
        report = pipeline.run(self.dir, resume=True)
        self.assertEqual(report["reprocessed_chunks"], [])
        self.assertEqual(pcm.stat().st_size, before)

    def test_5_verify_offset_is_exact(self):
        final = self.dir / "output" / "program.out.wav"
        clean = final.read_bytes()
        self.addCleanup(final.write_bytes, clean)
        for offset in (44, 45, 4096, 100_000):
            data = bytearray(clean)
            data[offset] ^= 0xFF
            final.write_bytes(bytes(data))
            self.assertEqual(_verify_offset(self.dir), offset)
        final.write_bytes(clean)
        self.assertEqual(cli.main(["verify", "--workdir", str(self.dir)]), 0)


def _verify_offset(workdir: Path) -> int:
    """Run verify's locating logic directly and return the reported offset."""
    from media_batch.output import locate_chunk_damage

    index = CheckpointIndex(workdir / "work" / "index.db")
    records = index.committed_chunks()
    index.close()
    final = workdir / "output" / "program.out.wav"
    data_offset = 44  # soundfile's canonical PCM_16 WAV header
    _, offset = locate_chunk_damage(
        records, final, workdir / "work" / "output.pcm", data_offset)
    return offset


if __name__ == "__main__":
    unittest.main()
