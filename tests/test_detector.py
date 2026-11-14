import numpy as np

from audioscan.batch import AnalysisOptions, analyze_file
from audioscan.detector import FrameSegment, detect_frames
from audioscan.framing import noise_floor_db
from audioscan.synth import _pack_wav


def _write(path, samples, sample_rate=48000):
    path.write_bytes(_pack_wav(samples.astype(np.float32), sample_rate, "float32"))


def test_all_silence_has_no_segments_and_all_voice_is_one(tmp_path):
    silence_path = tmp_path / "silence.wav"
    _write(silence_path, np.zeros((48000, 1)))
    silence = analyze_file(silence_path)
    assert silence["segment_count"] == 0
    assert silence["silence_total_samples"] == silence["total_samples"] == 48000

    t = np.arange(48000) / 48000.0
    voice_path = tmp_path / "voice.wav"
    _write(voice_path, (0.3 * np.sin(2 * np.pi * 180 * t))[:, None])
    voice = analyze_file(voice_path)
    assert voice["segment_count"] == 1
    assert voice["segments"][0]["start_sample"] == 0
    assert voice["segments"][0]["end_sample"] == voice["total_samples"]
    assert voice["silence_total_samples"] == 0


def test_hysteresis_hangover_bridges_short_dropout():
    levels = np.array([-60, -60, -39, -42, -60, -60, -60, -39, -39, -60, -60, -60], dtype=np.float32)
    segments = detect_frames(levels, -40.0, -43.0, hangover_frames=5)
    assert segments == [FrameSegment(2, 9)]


def test_noise_floor_uses_window_percentile_and_interpolation():
    rms = np.concatenate((np.full(100, 0.001), np.full(100, 0.01))).astype(np.float32)
    floor = noise_floor_db(rms, sample_rate=1000, frame_samples=10, window_seconds=1.0)
    assert floor.shape == rms.shape
    assert abs(float(floor[0]) - (-60.0)) < 0.1
    assert abs(float(floor[-1]) - (-40.0)) < 0.1
    assert np.all(np.diff(floor) >= -1e-5)


def test_stereo_with_dead_channel_falls_back_to_mono(tmp_path):
    sample_rate = 48000
    total = np.zeros((sample_rate, 2), dtype=np.float32)
    t = np.arange(24000) / sample_rate
    total[12000:36000, 0] = 0.3 * np.sin(2 * np.pi * 220 * t)
    path = tmp_path / "dead-right.wav"
    _write(path, total)
    result = analyze_file(path, AnalysisOptions())
    assert result["channels"] == 2
    assert result["channel_mode"] == "mono"
    assert result["segment_count"] == 1
