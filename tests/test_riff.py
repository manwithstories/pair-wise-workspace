import struct

import numpy as np
import pytest

from audioscan.riff import RiffError, WavReader
from audioscan.synth import _pack_wav


def _extensible_wav(path, samples, sample_rate=48000):
    payload = samples.astype("<i2").tobytes()
    guid = struct.pack("<IHH8s", 1, 0, 0x0010, b"\x80\x00\x00\xaa\x00\x38\x9b\x71")
    fmt = struct.pack("<HHIIHH", 0xFFFE, 1, sample_rate, sample_rate * 2, 2, 16)
    fmt += struct.pack("<HHI", 22, 16, 0) + guid
    junk = b"JUNK" + struct.pack("<I", 3) + b"abc" + b"\x00"
    chunks = junk + b"fmt " + struct.pack("<I", len(fmt)) + fmt
    chunks += b"data" + struct.pack("<I", len(payload)) + payload
    path.write_bytes(b"RIFF" + struct.pack("<I", 4 + len(chunks)) + b"WAVE" + chunks)


@pytest.mark.parametrize("encoding", ["pcm16", "pcm24", "pcm32", "float32"])
def test_roundtrip_formats_and_odd_junk_chunk(tmp_path, encoding):
    sample_rate = 48000
    t = np.arange(480, dtype=np.float32) / sample_rate
    samples = np.stack((0.3 * np.sin(2 * np.pi * 440 * t), 0.2 * np.sin(2 * np.pi * 660 * t)), axis=1)
    path = tmp_path / f"{encoding}.wav"
    path.write_bytes(_pack_wav(samples, sample_rate, encoding))
    with WavReader(path) as reader:
        assert reader.info is not None
        assert reader.info.encoding == encoding
        assert reader.info.channels == 2
        decoded = np.concatenate(list(reader.iter_frames(block_frames=100)), axis=0)
    tolerance = 1.0 / 32768 if encoding == "pcm16" else 2.0 / 8388608
    np.testing.assert_allclose(decoded, samples, atol=tolerance, rtol=0)


def test_extensible_pcm16(tmp_path):
    path = tmp_path / "extensible.wav"
    samples = np.round(np.sin(np.arange(100) / 5.0) * 12000).astype(np.int16)
    _extensible_wav(path, samples)
    with WavReader(path) as reader:
        assert reader.info is not None
        assert reader.info.encoding == "pcm16"
        decoded = np.concatenate(list(reader.iter_frames(block_frames=32)), axis=0)
    np.testing.assert_allclose(decoded[:, 0], samples.astype(np.float32) / 32768.0, atol=1e-7)


def test_short_header_and_truncated_data_are_invalid(tmp_path):
    bad = tmp_path / "bad.wav"
    bad.write_bytes(b"RIFF\x10\x00\x00\x00WAVE")
    with pytest.raises(RiffError):
        with WavReader(bad):
            pass
    good = tmp_path / "good.wav"
    good.write_bytes(_pack_wav(np.zeros((100, 1), np.float32), 22050, "pcm16"))
    truncated = tmp_path / "truncated.wav"
    truncated.write_bytes(good.read_bytes()[:-20])
    with pytest.raises(RiffError):
        with WavReader(truncated):
            pass


def test_unsupported_bit_depth_and_channel_count(tmp_path):
    path = tmp_path / "u8.wav"
    fmt = struct.pack("<HHIIHH", 1, 1, 48000, 48000, 1, 8)
    chunks = b"fmt " + struct.pack("<I", len(fmt)) + fmt + b"data" + struct.pack("<I", 4) + b"abcd"
    path.write_bytes(b"RIFF" + struct.pack("<I", 4 + len(chunks)) + b"WAVE" + chunks)
    with pytest.raises(RiffError):
        with WavReader(path):
            pass
    path3 = tmp_path / "three.wav"
    fmt = struct.pack("<HHIIHH", 1, 3, 48000, 48000 * 6, 6, 16)
    chunks = b"fmt " + struct.pack("<I", len(fmt)) + fmt + b"data" + struct.pack("<I", 12) + b"\x00" * 12
    path3.write_bytes(b"RIFF" + struct.pack("<I", 4 + len(chunks)) + b"WAVE" + chunks)
    with pytest.raises(RiffError):
        with WavReader(path3):
            pass
