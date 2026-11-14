"""轨迹生成确定性与逐行校验用例。"""

import tempfile
import unittest
from pathlib import Path

from abr.trace import (
    TraceValidationError,
    generate_samples,
    read_csv,
    write_csv,
)


class TestTraceGeneration(unittest.TestCase):
    def test_same_seed_byte_identical(self):
        with tempfile.TemporaryDirectory() as tmp:
            p1, p2 = Path(tmp) / "a.csv", Path(tmp) / "b.csv"
            for path in (p1, p2):
                samples = generate_samples(seed=7, duration_s=120,
                                           regimes=["wifi", "4g", "3g"], outages=2)
                write_csv(path, samples)
            self.assertEqual(p1.read_bytes(), p2.read_bytes())

    def test_outage_produces_zero_bandwidth(self):
        samples = generate_samples(seed=7, duration_s=300,
                                   regimes=["wifi", "4g"], outages=3)
        self.assertTrue(any(s.bandwidth_kbps == 0.0 for s in samples))

    def test_unknown_regime_rejected(self):
        with self.assertRaises(ValueError):
            generate_samples(seed=1, duration_s=10, regimes=["5g"])


class TestTraceValidation(unittest.TestCase):
    def _write(self, tmp: str, text: str) -> Path:
        path = Path(tmp) / "bad.csv"
        path.write_text(text, encoding="utf-8")
        return path

    def test_empty_file(self):
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(TraceValidationError):
                read_csv(self._write(tmp, ""))

    def test_missing_column_reports_line_1(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = self._write(tmp, "ts_ms,bandwidth_kbps\n0,1000\n")
            with self.assertRaises(TraceValidationError) as ctx:
                read_csv(path)
            self.assertEqual(ctx.exception.line_no, 1)

    def test_non_monotonic_ts_reports_line(self):
        with tempfile.TemporaryDirectory() as tmp:
            text = ("ts_ms,bandwidth_kbps,rtt_ms,loss_pct\n"
                    "0,1000,20,0.1\n"
                    "1000,1000,20,0.1\n"
                    "1000,1000,20,0.1\n")
            with self.assertRaises(TraceValidationError) as ctx:
                read_csv(self._write(tmp, text))
            self.assertEqual(ctx.exception.line_no, 4)

    def test_negative_bandwidth_reports_line(self):
        with tempfile.TemporaryDirectory() as tmp:
            text = ("ts_ms,bandwidth_kbps,rtt_ms,loss_pct\n"
                    "0,1000,20,0.1\n"
                    "1000,-5,20,0.1\n")
            with self.assertRaises(TraceValidationError) as ctx:
                read_csv(self._write(tmp, text))
            self.assertEqual(ctx.exception.line_no, 3)

    def test_header_only_file(self):
        with tempfile.TemporaryDirectory() as tmp:
            text = "ts_ms,bandwidth_kbps,rtt_ms,loss_pct\n"
            with self.assertRaises(TraceValidationError):
                read_csv(self._write(tmp, text))


if __name__ == "__main__":
    unittest.main()
