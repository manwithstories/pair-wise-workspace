"""Edge-case and numeric-invariant tests for the block-FFT reverb toolkit.

The convolution truth reference is an explicit O(N*M) direct convolution
written in the test module itself — production code never imports it, and
no ready-made convolution library is used anywhere.
"""
from __future__ import annotations

import io
import json
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path

import numpy as np

from src import metrics as mt
from src import signal_gen as sg
from src.block_convolver import BlockConvolver, ensure_2d
from src.config import (
    EXIT_NUMERIC_OR_MEMORY,
    EXIT_OK,
    EXIT_PARAM_ERROR,
    EXIT_PATH_OR_TAIL_MISMATCH,
    ConfigError,
    MemoryGuardError,
    PathMismatchError,
    RenderConfig,
    StreamBlockError,
    validate_render_config,
)
from src.io_audio import read_json, read_wav, write_json, write_wav
from src.reverb_cli import main
from src.render_paths import render_offline, render_streaming


def direct_convolve(x, ir):
    """Explicit linear convolution — test-only ground truth."""
    x2, ir2 = ensure_2d(x, "x"), ensure_2d(ir, "ir")
    n, m = len(x2), len(ir2)
    out = np.zeros((n + m - 1, x2.shape[1]), dtype=np.float64)
    for i in range(n):
        out[i:i + m] += x2[i] * ir2
    return out


def block_convolve(x, ir, block_size):
    core = BlockConvolver(ir, block_size)
    chunks = [core.process_block(x[i:i + block_size])
              for i in range(0, len(x), block_size)]
    chunks.append(core.flush())
    return np.concatenate(chunks, axis=0)


class TestConvolutionCore(unittest.TestCase):
    def test_matches_direct_convolution_many_shapes(self):
        rng = np.random.default_rng(0)
        cases = [(1, 4, 8, 1), (100, 37, 16, 1), (100, 37, 16, 2),
                 (5, 1, 4, 1), (33, 100, 32, 1), (7, 200, 8, 2),
                 (64, 64, 64, 1), (2, 3, 16, 2)]
        for n, m, b, c in cases:
            x, ir = rng.standard_normal((n, c)), rng.standard_normal((m, c))
            err = np.max(np.abs(block_convolve(x, ir, b)
                                - direct_convolve(x, ir)))
            self.assertLess(err, 1e-10, f"shape {(n, m, b, c)} err={err}")

    def test_ir_longer_than_one_block(self):
        x, ir = sg.fixture_long_tail_ir(block_size=64)
        y = block_convolve(x, ir, 64)
        self.assertEqual(y.shape, (len(x) + len(ir) - 1, 1))
        self.assertLess(np.max(np.abs(y - direct_convolve(x, ir))), 1e-10)

    def test_partial_last_block(self):
        x, ir = sg.fixture_partial_last_block(block_size=32)
        y = block_convolve(x, ir, 32)
        self.assertEqual(len(y) % 32 != 0, True)
        self.assertEqual(len(y), len(x) + len(ir) - 1)
        np.testing.assert_allclose(y, direct_convolve(x, ir), rtol=0, atol=1e-10)


class TestEdgeConditions(unittest.TestCase):
    def test_empty_input_gives_deterministic_zero_tail(self):
        rng = np.random.default_rng(1)
        ir = rng.standard_normal((10, 1))
        core = BlockConvolver(ir, 8)
        head = core.process_block(np.zeros((0, 1)))
        tail = core.flush()
        self.assertEqual(head.shape, (0, 1))
        self.assertEqual(tail.shape, (9, 1))              # M - 1 zeros
        self.assertTrue(np.all(tail == 0.0))

    def test_zero_length_ir_rejected(self):
        with self.assertRaises(ValueError):
            BlockConvolver(np.zeros((0, 1)), 8)

    def test_all_zero_ir_is_deterministic_zeros(self):
        rng = np.random.default_rng(2)
        x, ir = rng.standard_normal((30, 1)), np.zeros((20, 1))
        y = block_convolve(x, ir, 8)
        self.assertEqual(y.shape, (49, 1))
        self.assertTrue(np.all(y == 0.0))

    def test_state_machine_rejects_bad_call_order(self):
        rng = np.random.default_rng(3)
        core = BlockConvolver(rng.standard_normal((5, 1)), 4)
        core.flush()
        with self.assertRaises(RuntimeError):
            core.flush()
        with self.assertRaises(RuntimeError):
            core.process_block(np.ones((2, 1)))

    def test_oversized_block_and_channel_mismatch_rejected(self):
        rng = np.random.default_rng(4)
        core = BlockConvolver(rng.standard_normal((5, 1)), 4)
        with self.assertRaises(ValueError):
            core.process_block(rng.standard_normal((8, 1)))
        stereo = BlockConvolver(rng.standard_normal((5, 2)), 4)
        with self.assertRaises(ValueError):
            stereo.process_block(rng.standard_normal((4, 1)))


class TestStereoAndDeterminism(unittest.TestCase):
    def test_stereo_channels_processed_independently(self):
        x, ir = sg.fixture_stereo_independent()
        y = block_convolve(x, ir, 32)
        self.assertEqual(y.shape, (len(x) + len(ir) - 1, 2))
        mono_l = block_convolve(x[:, :1], ir[:, :1], 32)
        mono_r = block_convolve(x[:, 1:], ir[:, 1:], 32)
        np.testing.assert_allclose(y[:, :1], mono_l, rtol=0, atol=1e-12)
        np.testing.assert_allclose(y[:, 1:], mono_r, rtol=0, atol=1e-12)
        self.assertGreater(np.max(np.abs(y[:, 0] - y[:, 1])), 1e-6)

    def test_seed_is_reproducible(self):
        cfg = RenderConfig(48000, 2, 1024, 12000, 1.25, 7)
        a = sg.build_scene(cfg)
        b = sg.build_scene(cfg)
        np.testing.assert_array_equal(a["input"], b["input"])
        np.testing.assert_array_equal(a["ir"], b["ir"])


class TestPathAgreement(unittest.TestCase):
    def test_offline_streaming_identical_within_tolerance(self):
        cfg = RenderConfig(48000, 2, 1024, 12000, 1.25, 7)
        scene = sg.build_scene(cfg)
        off = render_offline(scene["input"], scene["ir"], cfg.block_size)
        st = render_streaming(scene["input"], scene["ir"], cfg.block_size)
        self.assertEqual(off.output.shape, st.output.shape)
        self.assertLessEqual(
            mt.path_max_abs_error(off.output, st.output), 1e-6
        )
        self.assertEqual(off.block_count, st.block_count)
        # contiguous output coverage, no gaps or overlaps
        spans = [(r.output_start, r.output_end) for r in st.records]
        for (_, e1), (s2, _) in zip(spans, spans[1:]):
            self.assertEqual(e1, s2)
        self.assertEqual(spans[0][0], 0)
        self.assertEqual(spans[-1][1], cfg.output_samples)


class TestConfigValidation(unittest.TestCase):
    def _cfg(self, **kw):
        base = dict(sample_rate=48000, channels=2, block_size=1024,
                    ir_length=12000, duration=1.0, seed=1)
        base.update(kw)
        return RenderConfig(**base)

    def test_rejects_non_positive_and_strange_values(self):
        for kw in ({"sample_rate": 0}, {"block_size": -4},
                   {"ir_length": 0}, {"duration": 0}, {"channels": 3},
                   {"seed": -1}):
            with self.assertRaises(ConfigError, msg=str(kw)):
                validate_render_config(self._cfg(**kw))

    def test_memory_guard_rejects_oversized_config(self):
        cfg = self._cfg(memory_budget_bytes=1024)
        with self.assertRaises(MemoryGuardError):
            validate_render_config(cfg)

    def test_acceptance_config_passes(self):
        cfg = RenderConfig(48000, 2, 1024, 12000, 1.25, 7)
        validate_render_config(cfg)
        self.assertEqual(cfg.input_samples, 60000)
        self.assertEqual(cfg.output_samples, 71999)


class TestStreamFailureAndCli(unittest.TestCase):
    def test_stream_block_exception_stops_and_is_structured(self):
        cfg = RenderConfig(48000, 1, 16, 64, 0.1, 5)
        scene = sg.build_scene(cfg)
        captured = []

        class FailingConvolver:
            """Wrap the real core; succeed on block 0, raise on block 1."""
            def __init__(self, ir, block_size):
                self._real = BlockConvolver(ir, block_size)
                self._calls = 0

            def process_block(self, block):
                self._calls += 1
                if self._calls == 2:
                    raise RuntimeError("simulated second-block failure")
                return self._real.process_block(block)

            def flush(self):
                return self._real.flush()

        import src.render_paths as rp
        original = rp.BlockConvolver
        rp.BlockConvolver = FailingConvolver
        try:
            with self.assertRaises(StreamBlockError) as ctx:
                render_streaming(scene["input"], scene["ir"], 16,
                                 on_error=captured.append)
        finally:
            rp.BlockConvolver = original
        self.assertEqual(len(captured), 1)
        self.assertIn("block_index", captured[0])
        self.assertEqual(captured[0]["block_index"], 1)
        # Exactly one full block (16 samples) was emitted before the stop.
        self.assertEqual(ctx.exception.details["samples_written"], 16)

    def test_path_mismatch_returns_exit_code_four(self):
        import src.reverb_cli as cli
        original = cli.mt.assert_path_agreement
        cli.mt.assert_path_agreement = lambda *a, **k: (_ for _ in ()).throw(
            PathMismatchError("forced divergence"))
        argv = ["render", "--duration", "0.05", "--sample-rate", "48000",
                "--channels", "1", "--block-size", "1024",
                "--ir-length", "2048", "--seed", "1"]
        try:
            with redirect_stderr(io.StringIO()):
                code = cli.main(argv)
        finally:
            cli.mt.assert_path_agreement = original
        self.assertEqual(code, EXIT_PATH_OR_TAIL_MISMATCH)

    def test_cli_render_success_end_to_end(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp) / "result.json"
            argv = ["render", "--duration", "1.25", "--sample-rate", "48000",
                    "--channels", "2", "--block-size", "1024",
                    "--ir-length", "12000", "--seed", "7",
                    "--json-output", str(out)]
            buf = io.StringIO()
            with redirect_stdout(buf):
                code = main(argv)
            self.assertEqual(code, EXIT_OK)
            data = read_json(out)
            self.assertEqual(data["output_samples"], 71999)
            self.assertEqual(data["blocks"], 71)
            self.assertLessEqual(data["path_max_abs_error"], 1e-6)
            self.assertTrue(Path(str(out).rsplit(".", 1)[0]
                                 + "_output.wav").exists())
            audio, rate = read_wav(str(out).rsplit(".", 1)[0] + "_output.wav")
            self.assertEqual(rate, 48000)
            self.assertEqual(audio.shape[1], 2)
            self.assertFalse(data["anomalies"]["global_nan"])

    def test_cli_parameter_error_exit_two(self):
        with redirect_stderr(io.StringIO()):
            code = main(["render", "--duration", "-1", "--sample-rate",
                         "48000", "--channels", "1", "--block-size", "1024",
                         "--ir-length", "100", "--seed", "1"])
        self.assertEqual(code, EXIT_PARAM_ERROR)


if __name__ == "__main__":
    unittest.main(verbosity=2)
