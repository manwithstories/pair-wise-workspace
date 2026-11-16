"""端到端模拟、指标语义、确定性与 CLI 退出码测试。"""

from __future__ import annotations

import json
import unittest
from contextlib import redirect_stdout
from io import StringIO

from tests._helpers import BITRATES
from abr.cli import main as cli_main
from abr.controller import SimulationConfig, SimulationController
from abr.runner import RunSettings, run_simulation
from abr.trace import TraceConfig, TraceGenerator
from abr.types import SimulationError, TracePattern, validate_bitrates


class TestEndToEnd(unittest.TestCase):
    def settings(self, seed=42):
        return RunSettings(
            duration_s=120.0,
            segment_duration_s=2.0,
            seed=seed,
            bitrates=BITRATES,
            pattern=TracePattern.MIXED,
        )

    def test_deterministic_full_run(self):
        levels = tuple(validate_bitrates(list(BITRATES)))
        _, m1 = run_simulation(self.settings(), levels)
        _, m2 = run_simulation(self.settings(), levels)
        self.assertEqual(m1.to_dict(), m2.to_dict())

    def test_acceptance_metric_ranges(self):
        levels = tuple(validate_bitrates(list(BITRATES)))
        d = run_simulation(self.settings(seed=42), levels)[1].to_dict()
        self.assertEqual(d["segments"], 60)
        self.assertEqual(len(d["selected_bitrates"]), 60)
        self.assertGreaterEqual(d["switch_count"], 5)
        self.assertLessEqual(d["switch_count"], 20)
        self.assertGreaterEqual(d["rebuffer_seconds"], 0.0)
        self.assertLessEqual(d["rebuffer_seconds"], 15.0)
        self.assertGreaterEqual(d["avg_buffer_seconds"], 3.0)
        self.assertLessEqual(d["avg_buffer_seconds"], 30.0)
        self.assertEqual(
            d["switch_count"], d["upshift_count"] + d["downshift_count"]
        )
        self.assertEqual(d["estimator_samples"], 60)
        self.assertEqual(d["failed_segment_count"], 0)

    def test_metrics_distinguish_switch_types(self):
        levels = tuple(validate_bitrates(list(BITRATES)))
        d = run_simulation(self.settings(), levels)[1].to_dict()
        self.assertLessEqual(d["opportunistic_upshift_count"],
                             d["upshift_count"])
        self.assertLessEqual(d["forced_downshift_count"],
                             d["downshift_count"])

    def test_controller_rejects_bad_start_level(self):
        levels = tuple(validate_bitrates(list(BITRATES)))
        trace = TraceGenerator(
            TraceConfig(duration_s=60, pattern=TracePattern.STABLE)
        ).generate()
        with self.assertRaises(SimulationError):
            SimulationController(trace, levels,
                                 SimulationConfig(start_level_index=99))

    def test_cli_simulate_exit_codes(self):
        self.assertEqual(cli_main([
            "simulate", "--duration", "40", "--seed", "42",
            "--bitrates", "300,800,1500",
        ]), 0)
        self.assertEqual(cli_main(["simulate", "--bitrates", "100,100"]), 2)
        self.assertEqual(cli_main(["trace", "--pattern", "nope"]), 2)

    def test_cli_json_is_valid(self):
        buf = StringIO()
        with redirect_stdout(buf):
            code = cli_main(["simulate", "--json", "--seed", "42"])
        self.assertEqual(code, 0)
        payload = json.loads(buf.getvalue())
        self.assertIn("segments", payload)
        self.assertIn("selected_bitrates", payload)

    def test_compare_runs_multiple_policies(self):
        buf = StringIO()
        with redirect_stdout(buf):
            code = cli_main(["compare", "--duration", "60", "--seed", "42"])
        self.assertEqual(code, 0)
        self.assertIn("balanced", buf.getvalue())


if __name__ == "__main__":
    unittest.main(verbosity=2)
