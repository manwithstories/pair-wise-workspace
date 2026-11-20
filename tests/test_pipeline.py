"""联合测试：边界条件与故障路径（标准库 unittest，零额外依赖）。

不依赖外部文件，夹具由生成器在临时目录现造；覆盖
* MediaClock 回绕/突变/起始负 PTS 的区分
* 抖动缓冲乱序、重复、窗口外迟到
* 恢复引擎可恢复/不可恢复缺口与重复计数
* 清单越界、重复序号、缺失引用、截断的拒绝路径
* 检查点原子提交、不匹配拒绝、续跑不重复计数
* 端到端 process/verify 的指标达标与损坏拒绝

运行：.venv/bin/python -m unittest tests.test_pipeline -v
"""

from __future__ import annotations

import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from region_media.buffer import JitterBuffer  # noqa: E402
from region_media.checkpoint import (  # noqa: E402
    CheckpointError, CheckpointStore, RunConfig)
from region_media.cli import main as cli_main  # noqa: E402
from region_media.generator import build_plan  # noqa: E402
from region_media.fixture_writer import generate_all, generate_scenario  # noqa: E402
from region_media.ingest import (  # noqa: E402
    IngestStats, NetworkConditioner, NetworkProfile,
    demux_segment, unwrap_packets)
from region_media.manifest import ManifestError, parse_manifest  # noqa: E402
from region_media.metrics import TARGETS  # noqa: E402
from region_media.model import (  # noqa: E402
    MediaClock, PacketModel, SegmentStatus, TrackKind, WrapResult,
    WRAP_MOD)
from region_media.recovery import RecoveryEngine  # noqa: E402


def _pkt(seq, pts, track=TrackKind.AUDIO):
    return PacketModel(track, seq, 0, 0, pts, pts, 20_000, seq * 1000, 64)


def _rp(seq, pts, dur=20000, seg=0):
    return PacketModel(TrackKind.AUDIO, seq, seg, 0, pts, pts, dur, 1000, 64)


class TestMediaClock(unittest.TestCase):
    def test_negative_start_pts_not_wrap(self):
        clk = MediaClock(1000, TrackKind.AUDIO)
        pts = [clk.unwrap(r)[0] for r in (-21, 0, 21, 42)]
        self.assertEqual((clk.wrap_events, clk.jump_events), (0, 0))
        self.assertEqual(pts, [0, 21000, 42000, 63000])

    def test_continuous_wrap_classified_once(self):
        clk = MediaClock(1000, TrackKind.AUDIO)
        v, out = WRAP_MOD - 100, []
        for _ in range(12):
            out.append(clk.unwrap(v % WRAP_MOD)[0])
            v += 21
        self.assertEqual((clk.wrap_events, clk.jump_events), (1, 0))
        self.assertTrue(all(b >= a for a, b in zip(out, out[1:])))

    def test_large_positive_jump_detected(self):
        clk = MediaClock(1000, TrackKind.AUDIO)
        clk.unwrap(0)
        _, res = clk.unwrap(50_000_000)
        self.assertEqual(res, WrapResult.JUMP)
        self.assertEqual((clk.jump_events, clk.wrap_events), (1, 0))

    def test_small_reorder_is_ok(self):
        clk = MediaClock(1000, TrackKind.AUDIO)
        clk.unwrap(100)
        _, res = clk.unwrap(95)
        self.assertEqual(res, WrapResult.OK)
        self.assertEqual(clk.wrap_events, 0)


class TestJitterBuffer(unittest.TestCase):
    def test_in_window_reorder(self):
        jb = JitterBuffer()
        for s in (0, 2, 1):
            self.assertEqual(jb.push(_pkt(s, s * 20000)),
                             SegmentStatus.RECEIVED)
        self.assertEqual([p.seq for p in jb.drain(TrackKind.AUDIO)],
                         [0, 1, 2])

    def test_duplicate_vs_late_distinguished(self):
        jb = JitterBuffer()
        for s in (0, 1, 3):
            jb.push(_pkt(s, s * 20000))
        jb.drain(TrackKind.AUDIO)
        jb.skip_gap(TrackKind.AUDIO, 2)
        jb.drain(TrackKind.AUDIO)
        self.assertEqual(jb.push(_pkt(2, 40000)), SegmentStatus.MISSING)
        self.assertEqual(jb.push(_pkt(1, 20000)), SegmentStatus.DUPLICATE)
        st = jb.stats()["audio"]
        self.assertEqual((st["late_drops"], st["duplicates"]), (1, 1))

    def test_window_within_bounds(self):
        jb = JitterBuffer()
        for s in range(8):
            jb.push(_pkt(s, s * 20000))
        w = jb.stats()["audio"]["window_us"]
        self.assertTrue(20_000 <= w <= 2_000_000)


class TestRecovery(unittest.TestCase):
    def test_periodic_gap_filled(self):
        eng = RecoveryEngine()
        out = []
        for s, pts in ((0, 0), (1, 20000), (4, 80000)):
            out.extend(eng.handle_packet(_rp(s, pts)))
        seqs = [(p.seq, p.synthetic) for p in out]
        self.assertEqual(seqs, [(0, False), (1, False), (2, True),
                                (3, True), (4, False)])
        self.assertEqual((eng.stats.recovered, eng.stats.missing), (2, 2))

    def test_large_residual_gap_unrecoverable(self):
        eng = RecoveryEngine()
        eng.handle_packet(_rp(0, 0))
        out = eng.handle_packet(_rp(2, 99999))
        self.assertEqual(len(out), 1)
        self.assertEqual(eng.stats.unrecoverable, 1)
        self.assertEqual(eng.stats.errors[-1].kind.value,
                         "gap_unrecoverable")

    def test_duplicate_only_counted(self):
        eng = RecoveryEngine()
        eng.handle_packet(_rp(0, 0))
        eng.note_duplicate(_rp(0, 0))
        self.assertEqual((eng.stats.duplicates, eng.stats.recovered), (1, 0))


class TestManifestValidation(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.root = os.path.join(self.tmp, "pl")
        generate_scenario(self.root, build_plan("normal"))

    def test_path_traversal_rejected(self):
        evil = os.path.join(self.root, "evil.m3u8")
        with open(evil, "w", encoding="utf-8") as fh:
            fh.write("#EXTM3U\n#EXT-X-TARGETDURATION:1\n"
                     "#EXTINF:1.0,\n../../etc/passwd\n#EXT-X-ENDLIST\n")
        with self.assertRaises(ManifestError) as cm:
            parse_manifest(evil)
        self.assertEqual(cm.exception.event.kind.value, "path_traversal")

    def test_missing_reference_marked(self):
        m = parse_manifest(os.path.join(self.root, "master.m3u8"))
        os.remove(os.path.join(m.base_dir, m.refs[0].uri))
        m2 = parse_manifest(os.path.join(self.root, "master.m3u8"))
        self.assertTrue(any(r.rejected for r in m2.refs))
        self.assertTrue(any(e.kind.value == "missing_reference"
                            for e in m2.errors))

    def test_truncated_manifest_flagged(self):
        root = os.path.join(self.tmp, "tr")
        generate_scenario(root, build_plan("truncated"))
        m = parse_manifest(os.path.join(root, "master.m3u8"))
        self.assertTrue(m.truncated)
        self.assertTrue(any(e.kind.value == "truncated_manifest"
                            for e in m.errors))

    def test_duplicate_sequence_conflict(self):
        # DASH 两个同 contentType 的 AdaptationSet 产生相同 (seq, kind)，
        # 触发重复序号冲突（HLS 媒体列表序号自动递增，难以制造同序号）
        m = parse_manifest(os.path.join(self.root, "master.m3u8"))
        target = m.refs_for(TrackKind.AUDIO)[0].uri
        mpd = os.path.join(self.root, "dup.mpd")
        with open(mpd, "w", encoding="utf-8") as fh:
            fh.write(
                '<?xml version="1.0"?>'
                '<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static">'
                '<Period>'
                '<AdaptationSet contentType="audio">'
                '<SegmentTemplate timescale="48000" startNumber="0"'
                f' media="{target}">'
                '<SegmentTimeline><S d="48000"/></SegmentTimeline>'
                '</SegmentTemplate><Representation id="a0"/></AdaptationSet>'
                '<AdaptationSet contentType="audio">'
                '<SegmentTemplate timescale="48000" startNumber="0"'
                f' media="{target}">'
                '<SegmentTimeline><S d="48000"/></SegmentTimeline>'
                '</SegmentTemplate><Representation id="a1"/></AdaptationSet>'
                '</Period></MPD>')
        m2 = parse_manifest(mpd)
        self.assertTrue(any(e.kind.value == "duplicate_sequence"
                            for e in m2.errors))


class TestCheckpoint(unittest.TestCase):
    def test_resume_and_mismatch(self):
        d = tempfile.mkdtemp()
        db = os.path.join(d, "idx.sqlite")
        cfg = RunConfig("m.m3u8", 16, 42, 123456)
        cp = CheckpointStore(db)
        self.assertTrue(cp.initialize_run(cfg))
        cp.commit_chunk(0, 3, {"packets_total": 10},
                        [(0, "received", 5, 0, 111)], [])
        cp.close()
        cp2 = CheckpointStore(db)
        self.assertFalse(cp2.initialize_run(cfg))
        self.assertEqual(cp2.resume_from(), 3)
        self.assertEqual(cp2.load_counters()["packets_total"], 10)
        with self.assertRaises(CheckpointError) as cm:
            cp2.initialize_run(RunConfig("m.m3u8", 8, 42, 123456))
        self.assertEqual(cm.exception.kind.value, "checkpoint_mismatch")
        cp2.close()

    def test_failed_commit_rolls_back(self):
        d = tempfile.mkdtemp()
        db = os.path.join(d, "ro.sqlite")
        cfg = RunConfig("m.m3u8", 16, 1, 1)
        cp = CheckpointStore(db)
        cp.initialize_run(cfg)
        cp.close()
        os.chmod(db, 0o444)
        try:
            cp2 = CheckpointStore(db)
            cp2.initialize_run(cfg)
            with self.assertRaises(CheckpointError):
                cp2.commit_chunk(1, 5, {}, [], [])
        finally:
            os.chmod(db, 0o644)


class TestWrapInjection(unittest.TestCase):
    def test_wrap_injection_clean(self):
        d = tempfile.mkdtemp()
        root = os.path.join(d, "w")
        generate_scenario(root, build_plan("normal"))
        m = parse_manifest(os.path.join(root, "master.m3u8"))
        cond = NetworkConditioner(NetworkProfile(
            seed=1, wrap_track=TrackKind.AUDIO, wrap_at_packet=3))
        clocks, st, audio_pts = {}, IngestStats(), []
        for seq in sorted({r.seq for r in m.refs}):
            ref = [r for r in m.refs if r.seq == seq
                   and r.kind == TrackKind.AUDIO][0]
            raw = demux_segment(os.path.join(m.base_dir, ref.uri),
                                seq, None, st)
            resolved = unwrap_packets(cond.apply(raw, st), clocks, st,
                                      {TrackKind.AUDIO: 1000})
            audio_pts += [p.pts_us for p in resolved
                          if p.track == TrackKind.AUDIO]
        self.assertGreaterEqual(st.wrap_events, 1)
        self.assertEqual(st.jump_events, 0)
        self.assertTrue(all(b >= a for a, b in zip(audio_pts, audio_pts[1:])))


class TestEndToEnd(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp()
        cls.fixtures = os.path.join(cls.tmp, "fx")
        generate_all(cls.fixtures)
        cls.work = tempfile.mkdtemp()

    def _process(self, scenario):
        from region_media.pipeline import PipelineConfig, run_pipeline
        cfg = PipelineConfig(
            manifest_path=os.path.join(self.fixtures, scenario,
                                       "master.m3u8"),
            index_path=os.path.join(self.work, f"{scenario}.sqlite"),
            chunk_size=16, resume=True, scenario=scenario)
        return run_pipeline(cfg)

    def test_normal_meets_targets(self):
        s = self._process("normal")
        self.assertLessEqual(s["sync_error_ms_p95"],
                             TARGETS["sync_error_ms_p95"])
        self.assertGreaterEqual(s["packet_recovery_rate"],
                                TARGETS["packet_recovery_rate"])
        self.assertLessEqual(s["latency_ms_p95"],
                             TARGETS["latency_ms_p95"])
        self.assertFalse(s["vfr_detected"])

    def test_resume_no_double_count(self):
        from region_media.pipeline import PipelineConfig, run_pipeline
        idx = os.path.join(self.work, "resume.sqlite")
        cfg = PipelineConfig(
            manifest_path=os.path.join(self.fixtures, "normal",
                                       "master.m3u8"),
            index_path=idx, chunk_size=16, resume=True, scenario="normal")
        first, second = run_pipeline(cfg), run_pipeline(cfg)
        self.assertEqual(first["packets_total"], second["packets_total"])
        self.assertTrue(second["checkpoint_resumed"])
        self.assertEqual(first["duplicates"], second["duplicates"])

    def test_corrupt_has_rejections(self):
        self.assertGreater(self._process("corrupt")["rejected_segments"], 0)

    def test_vfr_scenario_detected(self):
        self.assertTrue(self._process("vfr")["vfr_detected"])

    def test_wrap_scenario_records_event(self):
        s = self._process("wrap")
        self.assertGreaterEqual(s["wrap_events"], 1)
        self.assertEqual(s["jump_events"], 0)

    def test_loss_recovers_gaps(self):
        self.assertGreaterEqual(self._process("loss")["packet_recovery_rate"],
                                TARGETS["packet_recovery_rate"])

    def test_cli_exit_codes(self):
        rc = cli_main(["process", "--manifest", "/no/such.m3u8",
                       "--index", os.path.join(self.work, "x.sqlite")])
        self.assertEqual(rc, 2)
        rc = cli_main([
            "process",
            "--manifest", os.path.join(self.fixtures, "normal",
                                       "master.m3u8"),
            "--index", os.path.join(self.work, "cli.sqlite"),
            "--chunk-size", "16", "--resume"])
        self.assertEqual(rc, 0)


if __name__ == "__main__":
    unittest.main(verbosity=2)
