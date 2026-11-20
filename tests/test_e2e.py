"""端到端联合测试：回绕注入、流水线指标、中断续跑与 CLI 退出码。

运行：.venv/bin/python -m unittest tests.test_e2e -v
"""

from __future__ import annotations

import os
import tempfile
import unittest

from region_media.cli import main as cli_main
from region_media.fixture_writer import generate_all, generate_scenario
from region_media.generator import build_plan
from region_media.ingest import (
    IngestStats, NetworkConditioner, NetworkProfile,
    demux_segment, unwrap_packets)
from region_media.manifest import parse_manifest
from region_media.metrics import TARGETS
from region_media.model import TrackKind
from region_media.checkpoint import CheckpointStore
from region_media.pipeline import (
    Pipeline, PipelineConfig, PipelineInterrupted, run_pipeline)


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

    def test_interrupt_preserves_checkpoint_and_resume_matches(self):
        idx = os.path.join(self.work, "sig.sqlite")
        cfg = PipelineConfig(
            manifest_path=os.path.join(self.fixtures, "normal",
                                       "master.m3u8"),
            index_path=idx, chunk_size=2, resume=True, scenario="normal")
        pipe = Pipeline(cfg)
        pipe.open()
        orig = pipe._process_segment
        state = {"n": 0}

        def wrapped(seq, refs, errs, cc):
            state["n"] += 1
            if state["n"] == 3:
                pipe._interrupted = True
            return orig(seq, refs, errs, cc)

        pipe._process_segment = wrapped
        with self.assertRaises(PipelineInterrupted) as cm:
            pipe.run()
        pipe.close()
        self.assertGreaterEqual(cm.exception.last_seq, 0)
        cp = CheckpointStore(idx)
        self.assertEqual(cp.resume_from(), cm.exception.last_seq)
        cp.close()
        resumed = run_pipeline(cfg)
        full = run_pipeline(PipelineConfig(
            manifest_path=cfg.manifest_path,
            index_path=os.path.join(self.work, "full.sqlite"),
            chunk_size=2, resume=True, scenario="normal"))
        for k in ("packets_total", "packet_recovery_rate",
                  "sync_error_ms_p95", "latency_ms_p95"):
            self.assertEqual(resumed[k], full[k])

    def test_signal_handlers_install_and_restore(self):
        import signal as sig_mod
        cfg = PipelineConfig(
            manifest_path=os.path.join(self.fixtures, "normal",
                                       "master.m3u8"),
            index_path=os.path.join(self.work, "sig2.sqlite"),
            scenario="normal")
        pipe = Pipeline(cfg)
        before = sig_mod.getsignal(sig_mod.SIGTERM)
        pipe.install_signal_handlers()
        self.assertIs(sig_mod.getsignal(sig_mod.SIGTERM).__func__,
                      Pipeline._handle_signal)
        pipe.restore_signal_handlers()
        self.assertEqual(sig_mod.getsignal(sig_mod.SIGTERM), before)


if __name__ == "__main__":
    unittest.main(verbosity=2)
