"""Blend-kernel correctness and alpha-edge behaviour (16-bit buffer)."""

import unittest

import numpy as np

from timeliner.blend import _KERNELS, composite_region
from timeliner.errors import BlendModeError
from timeliner.model import BLEND_MODES


def ref_kernel(mode, s, d):
    if mode == "normal":
        return s
    if mode == "multiply":
        return s * d
    if mode == "screen":
        return s + d - s * d
    if mode == "overlay":
        return 2 * s * d if d <= 0.5 else 1 - 2 * (1 - s) * (1 - d)
    if mode == "add":
        return min(s + d, 1.0)
    if mode == "difference":
        return abs(d - s)
    if mode == "darken":
        return min(s, d)
    if mode == "lighten":
        return max(s, d)
    raise AssertionError(mode)


class TestKernels(unittest.TestCase):
    def test_all_modes_match_scalar_reference(self):
        rng = np.random.default_rng(3)
        sc = rng.uniform(0, 1, 257)
        dc = rng.uniform(0, 1, 257)
        for mode in BLEND_MODES:
            got = _KERNELS[mode](sc, dc)
            want = np.array([ref_kernel(mode, float(s), float(d))
                             for s, d in zip(sc, dc)])
            self.assertTrue(np.allclose(got, want, atol=1e-6), mode)

    def test_unknown_mode_raises(self):
        with self.assertRaises(BlendModeError):
            _KERNELS and composite_region(
                np.zeros((1, 1, 4), np.uint16),
                np.zeros((1, 1, 4), np.uint16), "nope")


def px(r, g, b, a):
    return np.array([[[r, g, b, a]]], dtype=np.uint16)


class TestAlphaEdges(unittest.TestCase):
    def test_half_white_over_black_is_gray_not_fringed(self):
        dst = px(0, 0, 0, 65535)
        src = px(65535, 65535, 65535, 32768)
        composite_region(dst, src, "normal")
        got = dst[0, 0]
        self.assertEqual(int(got[3]), 65535)
        for ch in got[:3]:
            self.assertAlmostEqual(int(ch), 32768, delta=2)

    def test_transparent_source_leaves_destination(self):
        dst = px(1000, 2000, 3000, 65535)
        src = px(65535, 0, 0, 0)
        composite_region(dst, src, "multiply")
        self.assertTrue((dst[0, 0] == [1000, 2000, 3000, 65535]).all())

    def test_repeated_accumulation_error_below_one_level(self):
        # 200 repeated blends of the same translucent layer on a 16-bit
        # buffer must drift by less than one 8-bit level (257/65535).
        dst = np.zeros((4, 4, 4), dtype=np.uint16)
        src = np.zeros((4, 4, 4), dtype=np.uint16)
        src[..., 0] = 40000
        src[..., 3] = 20000
        for _ in range(200):
            composite_region(dst, src, "screen")
        # reference in float64
        da = 0.0
        a = 20000 / 65535.0
        s = 40000 / 65535.0
        for _ in range(200):
            da = (s + da - s * da) * a + da * (1 - a)
        got = dst[..., 0].astype(np.float64) / 65535.0
        self.assertLess(np.abs(got - da).max(), 1.0 / 255.0)


if __name__ == "__main__":
    unittest.main()
