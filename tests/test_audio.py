"""音频归一测试：重采样、声道布局、尾部处理与 soundfile 往返。"""

import numpy as np

from mediasync.audio import (
    AudioSpec,
    normalize_audio,
    normalize_layout,
    read_audio,
    resample_linear,
    write_audio,
)
from mediasync.errors import ErrorBook, ErrorCode


def test_resample_keeps_integer_positions_exact():
    # 2Hz -> 4Hz：源整数样本在输出中精确保留（线性插值无漂移）。
    src = np.arange(6, dtype=np.float64)[:, None]
    out = resample_linear(src, 2, 4)
    assert out.shape[0] == 12
    assert np.allclose(out[::2, 0], src[:, 0])
    assert np.allclose(out[1, 0], 0.5)  # 中点插值


def test_resample_identity_and_empty():
    src = np.ones((4, 2))
    assert np.array_equal(resample_linear(src, 48000, 48000), src)
    empty = np.empty((0, 2))
    assert resample_linear(empty, 48000, 24000).shape == (0, 2)


def test_layout_missing_channels_padded_and_recorded():
    book = ErrorBook()
    mono = np.ones((8, 1))
    out, ops = normalize_layout(
        mono, AudioSpec(48000, "5.1"), source="a.wav", errors=book
    )
    assert out.shape == (8, 6)
    # 单声道上混到 L/R，C/LFE/Ls/Rs 为零。
    assert np.allclose(out[:, 0], 1.0) and np.allclose(out[:, 1], 1.0)
    assert np.allclose(out[:, 2:], 0.0)
    assert book.has(ErrorCode.AUDIO_MISSING_CHANNEL)
    assert any(op["type"] == "pad_zero_channels" for op in ops)


def test_layout_extra_channels_drop_and_fold():
    book = ErrorBook()
    surround = np.arange(30, dtype=np.float64).reshape(5, 6)
    dropped, drop_ops = normalize_layout(
        surround, AudioSpec(48000, "stereo"), source="a.wav", errors=book
    )
    assert dropped.shape == (5, 2)
    assert np.array_equal(dropped, surround[:, :2])
    assert any(op["type"] == "drop_extra_channels" for op in drop_ops)

    folded, fold_ops = normalize_layout(
        surround,
        AudioSpec(48000, "mono"),
        source="a.wav",
        errors=ErrorBook(),
        extra_policy="fold",
    )
    assert folded.shape == (5, 1)
    assert np.allclose(folded[:, 0], surround.mean(axis=1))
    assert any(op["type"] == "fold_extra_channels" for op in fold_ops)


def test_normalize_partial_tail_pad_and_truncate():
    book = ErrorBook()
    # 1000 帧，帧长 1024：pad 补 24；truncate 截掉 1000。
    samples = np.ones((1000, 2))
    padded = normalize_audio(
        samples, 48000, AudioSpec(48000, "stereo"),
        source="a.wav", errors=book, frame_size=1024, tail_policy="pad",
    )
    assert padded.output_frames == 1024
    assert np.allclose(padded.samples[1000:], 0.0)
    assert book.has(ErrorCode.AUDIO_PARTIAL_TAIL)

    truncated = normalize_audio(
        samples, 48000, AudioSpec(48000, "stereo"),
        source="a.wav", errors=ErrorBook(), frame_size=1024, tail_policy="truncate",
    )
    assert truncated.output_frames == 0


def test_normalize_full_pipeline_records_conversions():
    book = ErrorBook()
    # 单声道 24000Hz、2048 帧 -> 立体声 48000Hz，输出帧数翻倍且为整帧。
    samples = np.ones((2048, 1))
    result = normalize_audio(
        samples, 24000, AudioSpec(48000, "stereo"),
        source="a.wav", errors=book, frame_size=1024,
    )
    assert result.samples.shape == (4096, 2)
    types = {op["type"] for op in result.conversions}
    assert {"resample", "linear_channel_map"} <= types


def test_soundfile_roundtrip_inside_workdir(tmp_path):
    spec = AudioSpec(48000, "stereo")
    samples = (np.random.RandomState(7).randn(2048, 2) * 0.1)
    path = write_audio("out/a.wav", tmp_path, samples, spec.sample_rate)
    assert str(path).startswith(str(tmp_path.resolve()))
    loaded, rate = read_audio("out/a.wav", tmp_path)
    assert rate == 48000
    assert loaded.shape == (2048, 2)
    assert np.allclose(loaded, samples, atol=1e-6)
