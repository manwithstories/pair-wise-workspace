import struct

import numpy as np
import pytest

from audiocut import wavio
from audiocut.errors import UnsupportedFormatError


def test_roundtrip_16_mono(tmp_path):
    rng = np.random.default_rng(1)
    sig = rng.uniform(-0.5, 0.5, 4800)
    p = wavio.write_wav(tmp_path / "a.wav", sig, 48000, bits=16)
    info = wavio.read_info(p)
    assert (info.sample_rate, info.channels, info.bits_per_sample) == (48000, 1, 16)
    assert info.num_samples == 4800
    out = np.concatenate(list(wavio.iter_blocks(p, info, block_samples=1000)))
    assert out.shape == (4800,)
    assert np.max(np.abs(out - sig)) < 1e-4


def test_roundtrip_24_stereo(tmp_path):
    rng = np.random.default_rng(2)
    sig = rng.uniform(-0.9, 0.9, (3000, 2))
    p = wavio.write_wav(tmp_path / "b.wav", sig, 44100, bits=24, channels=2)
    info = wavio.read_info(p)
    assert (info.sample_rate, info.channels, info.bits_per_sample) == (44100, 2, 24)
    out = np.concatenate(list(wavio.iter_blocks(p, info, block_samples=777)))
    assert np.max(np.abs(out - sig.mean(axis=1))) < 1e-6


def test_24bit_sign_extension():
    raw = struct.pack("<i", -8388608)[:3] + struct.pack("<i", 8388607)[:3]
    out = wavio.decode_pcm(raw, 24, 1)
    assert out[0] == pytest.approx(-1.0)
    assert out[1] == pytest.approx(8388607 / 8388608)


def test_unknown_odd_chunk_padded(tmp_path):
    sig = np.zeros(100)
    odd = (b"JUNK", b"abc")  # 3-byte unknown chunk must be skipped with pad
    p = wavio.write_wav(tmp_path / "c.wav", sig, 8000, extra_chunks=[odd])
    info = wavio.read_info(p)
    assert info.num_samples == 100
    out = np.concatenate(list(wavio.iter_blocks(p, info)))
    assert out.shape == (100,)


def test_float_format_rejected(tmp_path):
    fmt = struct.pack("<HHIIHH", 3, 1, 48000, 48000 * 4, 4, 32)
    payload = b"\x00" * 64
    body = b"fmt " + struct.pack("<I", 16) + fmt + b"data" + struct.pack("<I", len(payload)) + payload
    p = tmp_path / "f.wav"
    p.write_bytes(b"RIFF" + struct.pack("<I", 4 + len(body)) + b"WAVE" + body)
    with pytest.raises(UnsupportedFormatError) as exc:
        wavio.read_info(p)
    assert exc.value.exit_code == 3


def test_not_a_wav(tmp_path):
    p = tmp_path / "x.wav"
    p.write_bytes(b"NOPE" * 8)
    with pytest.raises(UnsupportedFormatError):
        wavio.read_info(p)


def test_empty_data_chunk(tmp_path):
    p = wavio.write_wav(tmp_path / "z.wav", np.zeros(0), 48000)
    info = wavio.read_info(p)
    assert info.num_samples == 0
    assert list(wavio.iter_blocks(p, info)) == []
