"""End-to-end and unit tests for the aoa pipeline."""
from __future__ import annotations

import contextlib
import io
import json
import tempfile
import unittest
from pathlib import Path

import numpy as np

import aoa_cli
from aoa import decoder, encoder, fixtures, harmonics, metrics, normalization, render_paths
from aoa.types import (
    AudioFrame,
    ChannelLayout,
    Direction,
    DirectionError,
    NormMode,
    NumericalError,
)


class TestHarmonics(unittest.TestCase):
    def test_basis_cardinal_directions(self):
        front = harmonics.basis_vector(Direction(0, 0), NormMode.ACN_SN3D)
        np.testing.assert_allclose(front, [1, 0, 0, 1], atol=1e-12)
        left = harmonics.basis_vector(Direction(90, 0), NormMode.ACN_SN3D)
        np.testing.assert_allclose(left, [1, 1, 0, 0], atol=1e-12)
        up = harmonics.basis_vector(Direction(0, 90), NormMode.ACN_SN3D)
        np.testing.assert_allclose(up, [1, 0, 1, 0], atol=1e-12)

    def test_convention_scales(self):
        d = Direction(0, 0)
        self.assertAlmostEqual(
            harmonics.basis_vector(d, NormMode.ACN_N3D)[3], np.sqrt(3.0)
        )
        self.assertAlmostEqual(
            harmonics.basis_vector(d, NormMode.FUMA)[0], 1.0 / np.sqrt(2.0)
        )

    def test_direction_range_validation(self):
        with self.assertRaises(DirectionError):
            Direction(180.0001, 0)
        with self.assertRaises(DirectionError):
            Direction(0, -90.5)
        Direction(-180, 90)  # boundary values are legal


class TestNormalization(unittest.TestCase):
    def test_roundtrip_all_pairs(self):
        rng = np.random.default_rng(1)
        frames = rng.standard_normal((512, 4))
        for a in NormMode:
            for b in NormMode:
                err = normalization.roundtrip_error(frames, a, b)
                self.assertLess(err, 1e-12, (a, b))

    def test_conversion_values(self):
        frames = np.array([[1.0, 2.0, 3.0, 4.0]])  # W, Y, Z, X in SN3D
        fuma = normalization.convert(frames, NormMode.ACN_SN3D, NormMode.FUMA)
        np.testing.assert_allclose(
            fuma, [[1.0 / np.sqrt(2.0), 4.0, 2.0, 3.0]], atol=1e-12
        )

    def test_rejects_bad_labels(self):
        with self.assertRaises(normalization.NormalizationError):
            normalization.validate_channel_labels(["W", "W", "Z", "X"], NormMode.ACN_SN3D)
        with self.assertRaises(normalization.NormalizationError):
            normalization.validate_channel_labels(["W", "Y", "Z"], NormMode.ACN_SN3D)
        with self.assertRaises(normalization.NormalizationError):
            normalization.validate_channel_labels(["W", "Y", "Z", "Q"], NormMode.ACN_SN3D)


class TestEncoder(unittest.TestCase):
    def test_superposition(self):
        enc = encoder.Encoder(NormMode.ACN_SN3D)
        s1 = np.sin(np.linspace(0, 1, 256))
        s2 = np.cos(np.linspace(0, 1, 256))
        mix, _ = enc.encode_mix([(Direction(0, 0), s1), (Direction(90, 0), s2)])
        np.testing.assert_allclose(
            mix.samples, enc.encode(Direction(0, 0), s1) + enc.encode(Direction(90, 0), s2)
        )

    def test_frame_count_mismatch_raises(self):
        enc = encoder.Encoder(NormMode.ACN_SN3D)
        with self.assertRaises(ValueError):
            enc.encode_mix([(Direction(0, 0), np.zeros(10)), (Direction(1, 0), np.zeros(11))])

    def test_clipping_and_silence_diagnostics(self):
        enc = encoder.Encoder(NormMode.ACN_SN3D)
        loud = [np.ones(64) * 0.9, np.ones(64) * 0.9]
        _, diag = enc.encode_mix([(Direction(0, 0), loud[0]), (Direction(0, 0), loud[1])])
        self.assertGreater(diag.clipped_samples, 0)
        self.assertTrue(diag.warnings)
        silent, diag2 = enc.encode_mix([(Direction(0, 0), np.zeros(64))])
        self.assertEqual(metrics.total_energy(silent.samples), 0.0)
        self.assertEqual(diag2.clipped_samples, 0)


class TestDecoder(unittest.TestCase):
    def test_singular_layout_warns_and_stays_finite(self):
        layout = fixtures.make_layout("singular")
        design = decoder.build_decoder(layout, NormMode.ACN_SN3D)
        self.assertTrue(design.warnings)
        self.assertTrue(np.all(np.isfinite(design.matrix)))

    def test_least_squares_falls_back_on_singular(self):
        layout = fixtures.make_layout("ring8")  # horizontal: Z unobservable
        design = decoder.build_decoder(
            layout, NormMode.ACN_SN3D, strategy=decoder.LEAST_SQUARES
        )
        self.assertEqual(design.strategy, decoder.REGULARIZED)
        self.assertTrue(any("switched" in w for w in design.warnings))

    def test_source_on_speaker_peaks_at_that_speaker(self):
        layout = fixtures.make_layout("ring8")
        design = decoder.build_decoder(layout, NormMode.ACN_SN3D)
        for k, d in enumerate(layout.directions):
            g = design.matrix @ harmonics.basis_vector(d, NormMode.ACN_SN3D)
            self.assertEqual(int(np.argmax(g)), k)

    def test_bad_strategy_and_regularization(self):
        layout = fixtures.make_layout("quad")
        with self.assertRaises(ValueError):
            decoder.build_decoder(layout, NormMode.ACN_SN3D, strategy="nope")
        with self.assertRaises(ValueError):
            decoder.build_decoder(layout, NormMode.ACN_SN3D, regularization=0.0)


class TestRenderPaths(unittest.TestCase):
    def setUp(self):
        self.layout = fixtures.make_layout("ring8")
        self.design = decoder.build_decoder(self.layout, NormMode.ACN_SN3D)

    def test_front_source_symmetric_ears(self):
        b = harmonics.basis_vector(Direction(0, 0), NormMode.ACN_SN3D)
        frames = np.ones((512, 1)) * b[None, :]
        spk = render_paths.render_speakers(frames, self.design.matrix)
        ears = render_paths.render_binaural(spk.signals, self.layout)
        self.assertAlmostEqual(ears.rms[0], ears.rms[1], places=12)

    def test_lateral_source_ear_dominance(self):
        b = harmonics.basis_vector(Direction(90, 0), NormMode.ACN_SN3D)
        frames = np.ones((512, 1)) * b[None, :]
        spk = render_paths.render_speakers(frames, self.design.matrix)
        ears = render_paths.render_binaural(spk.signals, self.layout)
        self.assertGreater(ears.rms[0], 4.0 * ears.rms[1])

    def test_shared_decoder_consistency(self):
        frames = np.random.default_rng(2).standard_normal((256, 4)) * 0.1
        spk = render_paths.render_speakers(frames, self.design.matrix)
        ears = render_paths.render_binaural(spk.signals, self.layout)
        self.assertEqual(spk.signals.shape, (256, 8))
        self.assertEqual(ears.signals.shape, (256, 2))
        self.assertTrue(np.all(np.isfinite(ears.signals)))


class TestMetricsAndFixtures(unittest.TestCase):
    def test_rms_peak_condition(self):
        x = np.ones((100, 2))
        np.testing.assert_allclose(metrics.rms_per_channel(x), [1.0, 1.0])
        self.assertEqual(metrics.peak(x), 1.0)
        self.assertAlmostEqual(metrics.condition_number(np.eye(3)), 1.0)

    def test_fixture_reproducibility(self):
        a = fixtures.make_source_signals(17, 6, 128, 48000)
        b = fixtures.make_source_signals(17, 6, 128, 48000)
        np.testing.assert_array_equal(a, b)
        d = fixtures.make_directions(17, 6)
        self.assertEqual(len(d), 6)
        for direction in d:
            self.assertLessEqual(abs(direction.elevation_deg), 30.0)

    def test_inject_silence_and_clipping(self):
        sig = fixtures.make_source_signals(3, 2, 100, 48000, inject_silence=True)
        self.assertEqual(np.count_nonzero(sig[0, 25:50]), 0)
        sig = fixtures.make_source_signals(3, 2, 100, 48000, inject_clipping=True)
        self.assertGreater(np.max(np.abs(sig[1])), 1.0)

    def test_pcm16_roundtrip(self):
        x = np.linspace(-0.9, 0.9, 1000)
        back = fixtures.from_pcm16(fixtures.to_pcm16(x))
        self.assertLess(np.max(np.abs(back - x)), 1.0 / 32767.0 + 1e-9)

    def test_write_guards(self):
        with self.assertRaises(ValueError):
            fixtures.validate_for_write(AudioFrame(np.zeros((0, 4)), 48000))
        with self.assertRaises(NumericalError):
            fixtures.validate_for_write(AudioFrame(np.full((4, 4), np.nan), 48000))
        with self.assertRaises(NumericalError):
            fixtures.validate_for_write(AudioFrame(np.full((4, 4), 2.0), 48000))


class TestCli(unittest.TestCase):
    def _run(self, argv):
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            code = aoa_cli.main(argv)
        return code, buf.getvalue()

    def test_verify_acceptance_shape(self):
        code, out = self._run([
            "verify", "--frames", "4800", "--seed", "17",
            "--norm-in", "fuma", "--norm-out", "acn-sn3d",
            "--layout", "ring8", "--regularization", "1e-6", "--json",
        ])
        self.assertEqual(code, 0)
        report = json.loads(out)
        self.assertEqual(report["source_count"], 6)
        self.assertEqual(report["encoded_channels"], 4)
        self.assertEqual(report["speaker_channels"], 8)
        self.assertEqual(len(report["energy_by_direction"]), 6)
        self.assertEqual(len(report["decoded_rms"]), 8)
        self.assertEqual(len(report["binaural_rms"]), 2)
        self.assertLess(report["energy_error_db"], 1.0)
        self.assertLess(report["roundtrip_error"], 1e-6)
        self.assertLessEqual(report["max_crosstalk_db"], -18.0)
        self.assertGreaterEqual(report["max_crosstalk_db"], -35.0)
        self.assertLessEqual(len(report["warnings"]), 3)

    def test_exit_code_usage(self):
        code, _ = self._run([
            "verify", "--frames", "-5", "--seed", "1",
            "--norm-in", "fuma", "--norm-out", "acn-sn3d",
        ])
        self.assertEqual(code, 2)
        code, _ = self._run([
            "verify", "--frames", "100", "--seed", "1",
            "--norm-in", "bogus", "--norm-out", "acn-sn3d",
        ])
        self.assertEqual(code, 2)

    def test_encode_decode_roundtrip_files(self):
        with tempfile.TemporaryDirectory() as tmp:
            mono = Path(tmp) / "mono.wav"
            bfmt = Path(tmp) / "b.wav"
            spk = Path(tmp) / "spk.wav"
            t = np.arange(4800) / 48000.0
            aoa_cli.write_wav(str(mono), AudioFrame(0.3 * np.sin(2 * np.pi * 220 * t)[:, None], 48000))
            code, _ = self._run(["encode", "--azimuth", "30", "--input", str(mono),
                                 "--output", str(bfmt)])
            self.assertEqual(code, 0)
            code, _ = self._run(["decode", "--input", str(bfmt), "--output", str(spk),
                                 "--layout", "ring8"])
            self.assertEqual(code, 0)
            decoded = aoa_cli.read_wav(str(spk))
            self.assertEqual(decoded.channel_count, 8)
            self.assertEqual(decoded.frame_count, 4800)

    def test_encode_rejects_bad_direction(self):
        with tempfile.TemporaryDirectory() as tmp:
            mono = Path(tmp) / "mono.wav"
            aoa_cli.write_wav(str(mono), AudioFrame(np.zeros((64, 1)), 48000))
            code, _ = self._run(["encode", "--azimuth", "200", "--input", str(mono),
                                 "--output", str(Path(tmp) / "x.wav")])
            self.assertEqual(code, 2)


if __name__ == "__main__":
    unittest.main()
