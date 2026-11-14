import numpy as np

from audioscan.batch import AnalysisOptions, analyze_file
from audioscan.detector import FrameSegment
from audioscan.framing import Envelope, scan_envelopes
from audioscan.refine import refine_segments
from audioscan.riff import WavReader
from audioscan.synth import _pack_wav


def _tone(seconds, sample_rate, level=0.18, fade=0.03):
    t = np.arange(int(seconds * sample_rate)) / sample_rate
    signal = level * np.sin(2 * np.pi * 180 * t)
    count = max(1, int(fade * sample_rate))
    signal[:count] *= np.linspace(0.0, 1.0, count)
    signal[-count:] *= np.linspace(1.0, 0.0, count)
    return signal.astype(np.float32)


def _write_gap_fixture(path, sample_rate=48000):
    lead = np.zeros(int(0.3 * sample_rate), np.float32)
    gap = np.zeros(int(0.45 * sample_rate), np.float32)
    tail = np.zeros(int(0.3 * sample_rate), np.float32)
    audio = np.concatenate((lead, _tone(0.4, sample_rate), gap, _tone(0.4, sample_rate), tail))
    path.write_bytes(_pack_wav(audio[:, None], sample_rate, "float32"))
    return audio


def test_short_silence_is_merged_only_with_requested_gap(tmp_path):
    path = tmp_path / "gap.wav"
    _write_gap_fixture(path)
    separate = analyze_file(path, AnalysisOptions(merge_gap_ms=250))
    merged = analyze_file(path, AnalysisOptions(merge_gap_ms=500))
    assert separate["segment_count"] == 2
    assert merged["segment_count"] == 1
    assert merged["segments"][0]["start_sample"] < merged["segments"][0]["end_sample"]


def test_fade_expansion_honors_max_fade(tmp_path):
    sample_rate = 48000
    count = int(1.6 * sample_rate)
    t = np.arange(count) / sample_rate
    voice = (0.18 * np.sin(2 * np.pi * 170 * t)).astype(np.float32)
    ramp = int(0.7 * sample_rate)
    voice[:ramp] *= np.linspace(0.0, 1.0, ramp)
    voice[-ramp:] *= np.linspace(1.0, 0.0, ramp)
    audio = np.concatenate((np.zeros(int(0.2 * sample_rate)), voice, np.zeros(int(0.2 * sample_rate))))
    path = tmp_path / "long-fade.wav"
    path.write_bytes(_pack_wav(audio[:, None], sample_rate, "float32"))
    result = analyze_file(path, AnalysisOptions(max_fade_ms=300, merge_gap_ms=250))
    assert result["segment_count"] == 1
    segment = result["segments"][0]
    assert segment["fade_in_ms"] <= 300
    assert segment["fade_out_ms"] <= 300
    assert result["silence_total_samples"] == result["total_samples"] - (
        segment["end_sample"] - segment["start_sample"]
    )


def test_refine_all_silence_returns_empty(tmp_path):
    path = tmp_path / "silence.wav"
    path.write_bytes(_pack_wav(np.zeros((4800, 1), np.float32), 48000, "float32"))
    with WavReader(path) as reader:
        envelope = scan_envelopes(reader)
    floor = np.full(envelope.frame_count, -120.0, dtype=np.float32)
    from audioscan.detector import DetectionResult

    detection = DetectionResult([], floor, floor + 70.0, floor + 67.0)
    assert refine_segments(path, envelope, detection) == []
