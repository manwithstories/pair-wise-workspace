"""码率决策策略测试：可行性、确认、冷却、恢复滞后、预防性降档。"""

from __future__ import annotations

import unittest

from tests._helpers import BITRATES
from abr.policy import ABRPolicy, PolicyConfig, PolicyContext
from abr.types import (
    BandwidthEstimate,
    DecisionKind,
    PlaybackState,
    SwitchReason,
    validate_bitrates,
)


class TestPolicy(unittest.TestCase):
    def setUp(self):
        self.levels = tuple(validate_bitrates(list(BITRATES)))
        self.policy = ABRPolicy(
            PolicyConfig(confirm_segments=3, cooldown_segments=2,
                         safety_margin=1.3)
        )

    def estimate(self, kbps, trend=0.0, n=8):
        return BandwidthEstimate(kbps, kbps, kbps, trend, n, 0)

    def playback(self, buf=12.0, protection=False):
        return PlaybackState(buf, 10.0, 3.0, protection, 0.0, 0.0, 0.0)

    def context(self, cur, kbps, consecutive=0, cooldown=0, buf=12.0,
                protection=False, trend=0.0):
        return PolicyContext(
            0, self.levels, cur, self.estimate(kbps, trend),
            self.playback(buf, protection), consecutive, cooldown, 2.0,
        )

    def test_upshift_requires_confirmation(self):
        not_ready = self.policy.decide(self.context(0, 5000.0, consecutive=1))
        self.assertEqual(not_ready.kind, DecisionKind.HOLD)
        self.assertTrue(not_ready.risk_rejected)
        ready = self.policy.decide(self.context(0, 5000.0, consecutive=3))
        self.assertEqual(ready.kind, DecisionKind.UPSHIFT)
        self.assertEqual(ready.reason, SwitchReason.OPPORTUNISTIC)

    def test_cooldown_blocks_upshift(self):
        decision = self.policy.decide(
            self.context(0, 5000.0, consecutive=5, cooldown=2)
        )
        self.assertEqual(decision.kind, DecisionKind.HOLD)
        self.assertTrue(decision.cooldown_applied)

    def test_emergency_downshift_bypasses_cooldown(self):
        decision = self.policy.decide(
            self.context(4, 400.0, consecutive=0, cooldown=2)
        )
        self.assertEqual(decision.kind, DecisionKind.DOWNSHIFT)
        self.assertEqual(decision.reason, SwitchReason.FEASIBILITY)
        self.assertFalse(decision.cooldown_applied)

    def test_protection_forces_lowest_feasible(self):
        decision = self.policy.decide(
            self.context(3, 5000.0, consecutive=5, buf=1.0, protection=True)
        )
        self.assertEqual(decision.reason, SwitchReason.UNDERFLOW_PROTECTION)
        self.assertLessEqual(decision.chosen_index, 3)

    def test_recovery_hysteresis_blocks_upshift(self):
        # 缓冲仍在阈值附近的恢复滞后窗内，即便带宽充足也不升档
        decision = self.policy.decide(
            self.context(1, 5000.0, consecutive=5, buf=5.0)
        )
        self.assertEqual(decision.kind, DecisionKind.HOLD)

    def test_zero_bandwidth_picks_lowest(self):
        ctx = PolicyContext(
            0, self.levels, 4,
            BandwidthEstimate(0.0, 0.0, 0.0, 0.0, 0, 0),
            self.playback(12.0), 0, 0, 2.0,
        )
        self.assertEqual(self.policy.decide(ctx).chosen_index, 0)

    def test_time_feasibility_blocks_slow_high_level(self):
        # 带宽 3000、缓冲 0：idx4(4000) 下载 2.67s > 预算 2.0s，必须被剔除
        ctx = self.context(4, 3000.0, buf=0.0)
        feasible = self.policy._feasible_levels(ctx, 3000.0)
        self.assertNotIn(4, feasible)
        self.assertNotIn(5, feasible)
        self.assertIn(3, feasible)

    def test_preventive_downshift_on_thin_buffer(self):
        # 缓冲低于预警水位且当前档不可持续 -> 预防性降一档
        policy = ABRPolicy(PolicyConfig(confirm_segments=2, cooldown_segments=0))
        ctx = PolicyContext(
            0, self.levels, 3, self.estimate(1500.0),
            PlaybackState(4.0, 10.0, 3.0, False, 0.0, 0.0, 0.0),
            0, 0, 2.0,
        )
        decision = policy.decide(ctx)
        self.assertEqual(decision.kind, DecisionKind.DOWNSHIFT)
        self.assertEqual(decision.reason, SwitchReason.FEASIBILITY)

    def test_feasibility_and_protection_reasons_distinct(self):
        # 缓冲健康但不可行 -> feasibility；缓冲跌破阈值 -> protection
        healthy = self.policy.decide(self.context(4, 1000.0, buf=8.0))
        thin = self.policy.decide(self.context(4, 1000.0, buf=2.0))
        self.assertEqual(healthy.reason, SwitchReason.FEASIBILITY)
        self.assertEqual(thin.reason, SwitchReason.UNDERFLOW_PROTECTION)


if __name__ == "__main__":
    unittest.main(verbosity=2)
