from __future__ import annotations

import json
import wave
from pathlib import Path

import numpy as np
import pytest

from tlcomp.audio import check_audio, decode_wav
from tlcomp.errors import AssetError, ProjectError
from tlcomp.timeline import load_timeline


def _encode(samples: np.ndarray, width: int) -> bytes:
    samples = np.clip(samples, -1.0, 1.0)
    if width == 1:
        return np.round(samples * 127.0 + 128.0).astype(np.uint8).tobytes()
    if width == 2:
        return np.round(samples * 32767.0).astype("<i2").tobytes()
    if width == 4:
        return np.round(samples * 2147483647.0).astype("<i4").tobytes()
    signed = np.round(samples * 8388607.0).astype(np.int32)
    signed = np.where(signed < 0, signed + (1 << 24), signed)
    octets = np.stack((signed & 0xFF, (signed >> 8) & 0xFF, (signed >> 16) & 0xFF), axis=-1)
    return octets.astype(np.uint8).tobytes()


def _write_wav(path: Path, samples: np.ndarray, width: int = 2, rate: int = 8000) -> None:
    if samples.ndim == 1:
        samples = samples[:, None]
    with wave.open(str(path), "wb") as handle:
        handle.setnchannels(samples.shape[1])
        handle.setsampwidth(width)
        handle.setframerate(rate)
        handle.writeframes(_encode(samples, width))


@pytest.mark.parametrize("width", [1, 2, 3, 4])
def test_decode_all_pcm_widths(tmp_path: Path, width: int) -> None:
    path = tmp_path / f"pcm{width}.wav"
    source = np.array([0.25, -0.5, 0.75], dtype=np.float32)
    _write_wav(path, source, width)
    decoded = decode_wav(path)
    assert decoded.sample_width == width
    assert decoded.channels == 1
    np.testing.assert_allclose(decoded.samples[:, 0], source, atol=0.01)


def test_decode_stereo_uses_both_channels(tmp_path: Path) -> None:
    path = tmp_path / "stereo.wav"
    samples = np.array([[0.1, -0.2], [0.3, -0.9], [0.0, 0.0]], dtype=np.float32)
    _write_wav(path, samples, width=2)
    decoded = decode_wav(path)
    assert decoded.channels == 2
    np.testing.assert_allclose(decoded.samples, samples, atol=0.001)


def test_cue_jitter_metrics_and_worst_cue(project_factory, tmp_path: Path) -> None:
    project = project_factory(frames=6, with_layers=False, layers=[])
    wav_path = tmp_path / "jitter.wav"
    samples = np.zeros(2000, dtype=np.float32)
    offsets = [3, 7, 10]
    cues = [1, 3, 5]
    fps_value = 30000 / 1001
    for frame, offset in zip(cues, offsets):
        index = round(frame * 1000 / fps_value * 8.0) + offset
        samples[index] = 0.8
    _write_wav(wav_path, samples, width=2)
    data = json.loads(project.read_text())
    data["audio"] = {
        "path": str(wav_path), "cues": cues, "threshold": 0.5, "search_ms": 10,
    }
    project.write_text(json.dumps(data))
    result = check_audio(load_timeline(project))
    assert result["cues"] == 3
    assert result["worst_cue"] == 2
    assert result["max_abs_error_ms"] == pytest.approx(1.292, abs=0.001)
    assert [detail["error_ms"] for detail in result["cue_details"]] == pytest.approx(
        [0.383, 0.9, 1.292], abs=0.001
    )


def test_cue_out_of_timeline_is_rejected(project_factory) -> None:
    project = project_factory(frames=6, with_layers=False, layers=[])
    data = json.loads(project.read_text())
    data["audio"] = {"path": "missing.wav", "cues": [6]}
    project.write_text(json.dumps(data))
    with pytest.raises(ProjectError, match="cue out of range"):
        load_timeline(project)


def test_short_audio_or_missing_cue_has_explicit_error(project_factory, tmp_path: Path) -> None:
    project = project_factory(frames=6, with_layers=False, layers=[])
    short = tmp_path / "short.wav"
    _write_wav(short, np.zeros(100, dtype=np.float32))
    data = json.loads(project.read_text())
    data["audio"] = {"path": str(short), "cues": [5], "search_ms": 20}
    project.write_text(json.dumps(data))
    with pytest.raises(ProjectError, match="out of audio range"):
        check_audio(load_timeline(project))

    silent = tmp_path / "silent.wav"
    _write_wav(silent, np.zeros(5000, dtype=np.float32))
    data["audio"]["path"] = str(silent)
    project.write_text(json.dumps(data))
    with pytest.raises(AssetError, match="above threshold"):
        check_audio(load_timeline(project))


def test_missing_wav_is_asset_error(project_factory) -> None:
    project = project_factory(frames=6, with_layers=False, layers=[])
    data = json.loads(project.read_text())
    data["audio"] = {"path": "no-such.wav", "cues": [1]}
    project.write_text(json.dumps(data))
    with pytest.raises(AssetError, match="cannot decode WAV"):
        check_audio(load_timeline(project))
