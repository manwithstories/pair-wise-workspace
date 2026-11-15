import unittest
from pathlib import Path
import tempfile

import numpy as np

from support import ROOT  # noqa: F401
from abrl.codec.dct import (
    BASIS,
    dct_block,
    idct_block,
    quant_table,
    quantize,
    dequantize,
    zigzag_scan,
    zigzag_unscan,
)
from abrl.codec.encoder import EncodeParams, decode, encode
from abrl.media.yuv import Frame, iter_frames, resize, write_frames


class DctTests(unittest.TestCase):
    def test_roundtrip_without_quantization(self):
        rng = np.random.default_rng(0)
        block = (rng.integers(0, 256, (8, 8)) - 128).astype(np.int64)
        reconstructed = idct_block(dct_block(block))
        self.assertLessEqual(np.abs(reconstructed - block).max(), 8)

    def test_dc_of_constant_block(self):
        level = 72
        block = np.full((8, 8), level - 128, dtype=np.int64)
        coeffs = dct_block(block)
        self.assertEqual(np.count_nonzero(coeffs), 1)
        self.assertAlmostEqual(coeffs[0, 0], (level - 128) * 8, delta=12)

    def test_quantization_grows_with_qp(self):
        block = np.arange(64, dtype=np.int64).reshape(8, 8) - 32
        coeffs = dct_block(block)
        coarse = np.count_nonzero(quantize(coeffs, quant_table(40)))
        fine = np.count_nonzero(quantize(coeffs, quant_table(8)))
        self.assertLess(coarse, fine)

    def test_zigzag_is_inverse(self):
        rng = np.random.default_rng(1)
        blocks = rng.integers(-20, 20, (5, 8, 8)).astype(np.int16)
        self.assertTrue(np.array_equal(zigzag_unscan(zigzag_scan(blocks)), blocks))


class MediaTests(unittest.TestCase):
    def test_write_and_iterate_yuv420(self):
        frame = Frame(
            np.full((16, 16), 100, np.uint8),
            np.full((8, 8), 120, np.uint8),
            np.full((8, 8), 130, np.uint8),
        )
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "clip.yuv"
            write_frames(path, [frame, frame])
            read_back = list(iter_frames(path, 16, 16))
        self.assertEqual(len(read_back), 2)
        self.assertTrue(np.array_equal(read_back[0].y, frame.y))
        self.assertEqual(read_back[0].u.shape, (8, 8))

    def test_resize_keeps_even_planes(self):
        frame = Frame(
            np.tile(np.arange(16, dtype=np.uint8), (16, 1)),
            np.zeros((8, 8), np.uint8),
            np.full((8, 8), 255, np.uint8),
        )
        scaled = resize(frame, 32, 32)
        self.assertEqual(scaled.y.shape, (32, 32))
        self.assertEqual(scaled.u.shape, (16, 16))


class CodecTests(unittest.TestCase):
    def _frames(self):
        rng = np.random.default_rng(2)
        frames = []
        for i in range(6):
            base = np.clip(128 + 30 * np.sin(np.arange(32 * 32).reshape(32, 32) / 8 + i), 0, 255)
            y = base.astype(np.uint8)
            uv = rng.integers(100, 140, (16, 16)).astype(np.uint8)
            frames.append(Frame(y, uv, uv))
        return frames

    def test_encode_decode_bitstream_roundtrip(self):
        frames = self._frames()
        params = EncodeParams(width=32, height=32, fps=30, qp=12, gop=4)
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "out.abr"
            size = encode(frames, params, path)
            self.assertGreater(size, 0)
            self.assertFalse((Path(tmp) / "out.abr.tmp").exists())
            decoded = decode(path, 12)
        self.assertEqual(len(decoded), len(frames))
        self.assertEqual(decoded[0].y.shape, (32, 32))
        self.assertGreater(np.mean(decoded[0].y), 0)

    def test_rejects_odd_dimensions(self):
        with self.assertRaises(ValueError):
            EncodeParams(width=24, height=24, fps=30, qp=20, gop=8).validate()


if __name__ == "__main__":
    unittest.main()


class BitstreamContractTests(unittest.TestCase):
    def test_bitrate_matches_real_byte_size(self):
        import tempfile
        from pathlib import Path
        from abrl.codec.encoder import EncodeParams, encode
        rng = np.random.default_rng(7)
        frames = []
        for _ in range(10):
            y = rng.integers(0, 256, (32, 32), dtype=np.uint8)
            uv = rng.integers(0, 256, (16, 16), dtype=np.uint8)
            frames.append(Frame(y, uv, uv))
        params = EncodeParams(32, 32, fps=25, qp=18, gop=5)
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp) / "real.abr"
            size = encode(frames, params, out)
            self.assertEqual(out.stat().st_size, size)
            self.assertFalse((Path(tmp) / "real.abr.tmp").exists())
            expected_kbps = size * 8 / (10 / 25) / 1000
            self.assertGreater(size, 0)
            self.assertAlmostEqual(expected_kbps, size * 20 / 1000)

    def test_encode_rejects_non_multiple_of_16(self):
        with self.assertRaises(ValueError):
            EncodeParams(24, 24, 30, 20, 8).validate()
