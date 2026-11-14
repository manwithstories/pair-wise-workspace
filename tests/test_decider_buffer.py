"""决策器规则与缓冲账本边界用例。"""

import unittest

from abr.buffer import BufferLedger
from abr.decider import Decider
from abr.estimator import HarmonicMeanEstimator
from abr.ladder import Ladder, LadderValidationError
from abr.simulator import NoUsableSegmentError, simulate
from abr.trace import TraceSample

LADDER = Ladder(bitrates_kbps=(400, 800, 1500, 2500, 4000, 6000), chunk_sec=4.0)


class TestDecider(unittest.TestCase):
    def test_startup_takes_lowest(self):
        d = Decider(LADDER)
        self.assertEqual(d.decide(0.0, 0.0), 0)
        self.assertEqual(d.decide(500.0, 4.0), 0)

    def test_startup_skips_to_top_on_huge_estimate(self):
        d = Decider(LADDER)
        top = d.decide(2.5 * LADDER.bitrates_kbps[-2], 0.0)
        self.assertEqual(top, len(LADDER.bitrates_kbps) - 1)

    def test_upgrade_needs_two_consecutive_ok(self):
        d = Decider(LADDER)
        d.decide(0.0, 0.0)
        d.decide(0.0, 0.0)
        self.assertEqual(d.decide(2000.0, 10.0), 0)   # 第 1 次满足
        self.assertEqual(d.decide(2000.0, 10.0), 1)   # 连续第 2 次，升档

    def test_upgrade_blocked_by_low_buffer(self):
        d = Decider(LADDER)
        d.decide(0.0, 0.0)
        d.decide(0.0, 0.0)
        for _ in range(4):
            self.assertEqual(d.decide(2000.0, 6.0), 0)

    def test_emergency_downgrade_on_low_estimate(self):
        d = Decider(LADDER)
        d.decide(2.5 * LADDER.bitrates_kbps[-2], 0.0)  # 启动直升顶档
        d.decide(9000.0, 10.0)
        idx = d.decide(1000.0, 10.0)  # 估计远低于当前 0.8 倍，落到可支撑档
        self.assertEqual(idx, 1)  # 800kbps 是 <=1000 的最高档

    def test_emergency_downgrade_on_low_buffer(self):
        d = Decider(LADDER)
        d.decide(2.5 * LADDER.bitrates_kbps[-2], 0.0)
        d.decide(9000.0, 10.0)
        idx = d.decide(9000.0, 3.0)  # 水位低于 5 秒，至少降一级
        self.assertEqual(idx, len(LADDER.bitrates_kbps) - 2)

    def test_cooldown_blocks_immediate_reupgrade(self):
        d = Decider(LADDER)
        d.decide(0.0, 0.0)
        d.decide(0.0, 0.0)
        d.decide(2000.0, 10.0)
        self.assertEqual(d.decide(2000.0, 10.0), 1)   # 升档
        d.decide(100.0, 10.0)                          # 紧急降回 0
        self.assertEqual(d.decide(2000.0, 10.0), 0)   # 冷却期内不升
        self.assertEqual(d.decide(2000.0, 10.0), 0)
        self.assertEqual(d.decide(2000.0, 10.0), 1)   # 冷却结束且连续满足


class TestBuffer(unittest.TestCase):
    def test_rebuffer_event_and_ms(self):
        buf = BufferLedger(cap_s=30.0)
        buf.add_chunk(4.0)  # 播放已启动
        buf.drain(4.0)      # 水位耗尽
        buf.drain(2.0)  # 空缓冲继续流逝 -> 1 次事件，2000ms
        self.assertEqual(buf.rebuffer_events, 1)
        self.assertAlmostEqual(buf.rebuffer_ms, 2000.0)
        buf.add_chunk(4.0)
        buf.drain(10.0)  # 4s 后再次耗尽，剩余 6s 计入
        self.assertEqual(buf.rebuffer_events, 2)
        self.assertAlmostEqual(buf.rebuffer_ms, 8000.0)

    def test_pre_start_waiting_is_not_rebuffer(self):
        buf = BufferLedger(cap_s=30.0)
        buf.drain(3.0)  # 启动前等待只推进时钟
        self.assertEqual(buf.rebuffer_events, 0)
        self.assertAlmostEqual(buf.rebuffer_ms, 0.0)
        self.assertAlmostEqual(buf.clock_ms, 3000.0)

    def test_pause_at_cap(self):
        buf = BufferLedger(cap_s=8.0)
        buf.add_chunk(4.0)
        buf.add_chunk(4.0)
        self.assertTrue(buf.should_pause())
        buf.pause(1.0)
        self.assertAlmostEqual(buf.paused_ms, 1000.0)
        self.assertAlmostEqual(buf.level_s, 7.0)


class TestSimulatorEdges(unittest.TestCase):
    def test_trace_shorter_than_chunk_raises(self):
        samples = [TraceSample(i * 1000, 5000.0, 20.0, 0.0) for i in range(3)]
        buf = BufferLedger(cap_s=30.0)
        with self.assertRaises(NoUsableSegmentError):
            simulate(samples, HarmonicMeanEstimator(), Decider(LADDER), LADDER, buf)

    def test_zero_bandwidth_no_deadlock(self):
        samples = [TraceSample(i * 1000, 5000.0, 20.0, 0.0) for i in range(10)]
        samples += [TraceSample((i + 10) * 1000, 0.0, 20.0, 100.0) for i in range(30)]
        buf = BufferLedger(cap_s=30.0)
        records, _ = simulate(samples, HarmonicMeanEstimator(), Decider(LADDER), LADDER, buf)
        self.assertGreater(len(records), 0)
        self.assertGreater(buf.rebuffer_events, 0)
        self.assertAlmostEqual(buf.clock_ms, 40000.0)

    def test_all_zero_trace_no_usable_segment(self):
        samples = [TraceSample(i * 1000, 0.0, 20.0, 100.0) for i in range(40)]
        buf = BufferLedger(cap_s=30.0)
        with self.assertRaises(NoUsableSegmentError):
            simulate(samples, HarmonicMeanEstimator(), Decider(LADDER), LADDER, buf)
        self.assertAlmostEqual(buf.clock_ms, 40000.0)


class TestLadderValidation(unittest.TestCase):
    def test_non_monotonic_rejected(self):
        with self.assertRaises(LadderValidationError):
            Ladder(bitrates_kbps=(800, 400, 1500))

    def test_chunk_bytes(self):
        self.assertAlmostEqual(LADDER.chunk_bytes(400), 400 * 1000 / 8 * 4)


if __name__ == "__main__":
    unittest.main()
