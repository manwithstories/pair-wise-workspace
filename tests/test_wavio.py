from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

import numpy as np

from audiosplit.errors import AudioError
from audiosplit.wavio import WavReader, read_header, write_wav_atomic, write_wav_range_atomic
from tests.helpers import make_wav_bytes, write_tmp_wav


class WavReaderTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)

    def tearDown(self) -> None:
        self.temp.cleanup()

    def test_bit_depths_and_downmix(self) -> None:
        for bits in (8, 16, 24, 32):
            with self.subTest(bits=bits):
                stereo = np.array([[0.5, 0.25], [-0.5, -0.25]], dtype=np.float64)
                path = write_tmp_wav(self.root / f"depth_{bits}.wav", stereo, bits=bits)
                with WavReader(path) as reader:
                    self.assertEqual(reader.header.channels, 2)
                    samples = np.concatenate(list(reader.iter_mono(block_frames=1)))
                np.testing.assert_allclose(samples, [0.375, -0.375], atol=2e-4)

    def test_atomic_writer_round_trip(self) -> None:
        path = self.root / "nested" / "part.wav"
        source = np.array([0.0, 0.25, -0.25], dtype=np.float32)
        write_wav_atomic(path, 48000, source)
        with WavReader(path) as reader:
            result = np.concatenate(list(reader.iter_mono()))
        np.testing.assert_allclose(result, source, atol=1e-4)
        self.assertEqual(read_header(path).sample_rate, 48000)
        self.assertFalse(any(path.parent.glob("*.tmp")))

    def test_streaming_range_writer(self) -> None:
        source_path = self.root / "source.wav"
        source = np.linspace(-0.5, 0.5, 1000, dtype=np.float32)
        write_wav_atomic(source_path, 48000, source)
        output_path = self.root / "range.wav"
        with WavReader(source_path) as reader:
            write_wav_range_atomic(output_path, 48000, reader, 100, 900, block_frames=37)
        with WavReader(output_path) as reader:
            result = np.concatenate(list(reader.iter_mono(block_frames=41)))
        np.testing.assert_allclose(result, source[100:900], atol=1e-4)

    def test_rejects_invalid_headers(self) -> None:
        cases = {
            "truncated_header": (
                b"RIFF\x04\x00\x00\x00WAV",
                "truncated_header",
            ),
            "invalid_bit_depth": (
                make_wav_bytes(np.zeros(4), bits=12, data_bytes=b"\0" * 16),
                "invalid_bit_depth",
            ),
            "non_pcm": (
                make_wav_bytes(np.zeros(4), format_tag=3, data_bytes=b"\0" * 8),
                "non_pcm",
            ),
            "sample_rate_mismatch": (
                make_wav_bytes(np.zeros(4), byte_rate=1),
                "sample_rate_mismatch",
            ),
            "empty_audio": (make_wav_bytes(np.zeros(0)), "empty_audio"),
        }
        for name, (payload, expected_code) in cases.items():
            with self.subTest(name=name):
                path = self.root / f"{name}.wav"
                path.write_bytes(payload)
                with self.assertRaises(AudioError) as caught:
                    with WavReader(path):
                        pass
                self.assertEqual(caught.exception.code, expected_code)

    def test_midstream_eof_is_reported(self) -> None:
        frames = np.linspace(-0.5, 0.5, 1000)
        payload = make_wav_bytes(frames, declared_data_size=4000)
        path = self.root / "cut.wav"
        path.write_bytes(payload[:-800])
        with WavReader(path) as reader:
            with self.assertRaises(AudioError) as caught:
                list(reader.iter_mono(block_frames=100))
        self.assertEqual(caught.exception.code, "unexpected_eof")


if __name__ == "__main__":
    unittest.main()

