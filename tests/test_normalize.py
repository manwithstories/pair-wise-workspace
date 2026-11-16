"""Normalization tests: gain, true-peak protection, clipping accounting."""

from __future__ import annotations

import math
from array import array as double_array
from pathlib import Path

import pytest
import soundfile as sf

from audio_tool import io as audio_io
from audio_tool.loudness import LoudnessMeter
from audio_tool.model import ProcessConfig
from audio_tool.normalize import compute_gain, run_two_pass


def base_config(**overrides) -> ProcessConfig:
    values = dict(target_rate=48000, target_lufs=-16.0, true_peak_db=-1.0)
    values.update(overrides)
    return ProcessConfig(**values)


def measure_file(path: Path, channels: int) -> float | None:
    info = audio_io.probe(path)
    meter = LoudnessMeter(info.channels, info.samplerate, (1.0,) * info.channels)
    with audio_io.BlockReader(info, 4096) as reader:
        for block in reader:
            meter.process(block.planes, block.frames)
    return meter.finish().lufs


def test_hits_target_lufs_on_every_fixture(fixture_tree, tmp_path):
    for name in ("tone_44100_stereo", "tone_48000_51", "tone_96000_quad",
                 "mono_odd_tail", "quiet_music"):
        result = run_two_pass(fixture_tree[name], tmp_path / f"{name}.wav",
                              base_config())
        assert result.after.lufs == pytest.approx(-16.0, abs=0.1), name
        assert result.gain_db == pytest.approx(
            -16.0 - result.before.lufs, abs=0.05), name


def test_output_sample_rate_and_channels(fixture_tree, tmp_path):
    result = run_two_pass(fixture_tree["tone_44100_stereo"],
                          tmp_path / "out.wav", base_config())
    info = audio_io.probe(tmp_path / "out.wav")
    assert info.samplerate == 48000
    assert info.channels == 2
    assert info.frames == result.output_frames


def test_output_file_measures_the_reported_lufs(fixture_tree, tmp_path):
    destination = tmp_path / "verified.wav"
    result = run_two_pass(fixture_tree["tone_48000_51"], destination,
                          base_config())
    measured = measure_file(destination, 6)
    assert measured == pytest.approx(result.after.lufs, abs=0.05)


def test_silence_returns_an_explainable_finite_state(fixture_tree, tmp_path):
    # Silence has no defined loudness.  It is passed through at unity gain and
    # reported with a status that says exactly why, rather than crashing or
    # emitting NaN/-inf into the report.
    destination = tmp_path / "silent.wav"
    result = run_two_pass(fixture_tree["silent"], destination, base_config())
    assert result.status == "silent"
    assert result.gain_db == 0.0
    assert result.before.lufs is None and result.after.lufs is None
    assert result.before.digital_silence is True
    assert result.after.digital_silence is True
    assert result.note and "-70 LUFS" in result.note
    payload = result.to_dict()
    assert payload["status"] == "silent"
    assert payload["before"]["rms_dbfs"] == -240.0     # finite, not -inf
    assert destination.exists()
    assert audio_io.probe(destination).frames == result.output_frames


def test_silence_leaves_no_staging_file_behind(fixture_tree, tmp_path):
    out_dir = tmp_path / "out"
    out_dir.mkdir()
    run_two_pass(fixture_tree["silent"], out_dir / "silent.wav", base_config())
    assert [path.name for path in out_dir.iterdir()] == ["silent.wav"]


def test_true_peak_protection_limits_gain(tmp_path):
    # A loud transient in a quiet bed: loudness wants +7 dB, the peak forbids it.
    rate = 48000
    data = double_array("d", [0.001 * math.sin(2 * math.pi * 300 * i / rate)
                       for i in range(rate * 3)])
    for index in range(1000, 1200):
        data[index] = 0.99
    source = tmp_path / "transient.wav"
    with sf.SoundFile(str(source), "w", samplerate=rate, channels=1,
                      subtype="PCM_24", format="WAV") as handle:
        handle.buffer_write(data, "float64")
    result = run_two_pass(source, tmp_path / "out.wav", base_config())
    assert result.true_peak_limited is True
    assert result.gain_db < result.requested_gain_db
    assert result.after.true_peak <= 10 ** (-1.0 / 20.0)
    assert result.output_clipped == 0


def test_true_peak_never_exceeds_ceiling_for_normal_material(fixture_tree, tmp_path):
    for name in ("tone_44100_stereo", "quiet_music", "tone_96000_quad"):
        result = run_two_pass(fixture_tree[name], tmp_path / f"{name}.wav",
                              base_config())
        ceiling = 10 ** (-1.0 / 20.0)
        assert result.after.true_peak <= ceiling + 1e-9, name


def test_original_clipping_is_counted(fixture_tree, tmp_path):
    result = run_two_pass(fixture_tree["clipped_loud"],
                          tmp_path / "clipped.wav", base_config())
    assert result.original_clipped > 0
    # Attenuating loud clipped material must not add clipping.
    assert result.added_clipped == 0


def test_quiet_material_is_lifted_without_clipping(fixture_tree, tmp_path):
    result = run_two_pass(fixture_tree["quiet_music"],
                          tmp_path / "quiet.wav", base_config())
    assert result.gain_db > 10.0
    assert result.output_clipped == 0
    assert result.added_clipped == 0


def test_zero_padding_is_reported(fixture_tree, tmp_path):
    result = run_two_pass(fixture_tree["mono_odd_tail"],
                          tmp_path / "odd.wav", base_config())
    assert result.zero_padded_frames > 0


def test_compute_gain_passes_silence_through_at_unity():
    from audio_tool.model import LoudnessStats

    stats = LoudnessStats(frames=0, digital_silence=True, lufs=None)
    applied, requested, limited = compute_gain(stats, base_config())
    assert (applied, requested, limited) == (0.0, 0.0, False)


def test_compute_gain_requests_exact_delta():
    from audio_tool.model import LoudnessStats

    stats = LoudnessStats(frames=48000, digital_silence=False, lufs=-30.0,
                          true_peak=0.1)
    applied, requested, limited = compute_gain(stats, base_config())
    assert requested == pytest.approx(14.0)
    assert applied == pytest.approx(14.0)
    assert limited is False


def test_computation_is_deterministic(fixture_tree, tmp_path):
    first = run_two_pass(fixture_tree["tone_44100_stereo"],
                         tmp_path / "a.wav", base_config())
    second = run_two_pass(fixture_tree["tone_44100_stereo"],
                          tmp_path / "b.wav", base_config())
    assert first.to_dict() == second.to_dict()
    assert (tmp_path / "a.wav").read_bytes() == (tmp_path / "b.wav").read_bytes()


def test_does_not_overwrite_source(fixture_tree, tmp_path):
    source = tmp_path / "copy.wav"
    source.write_bytes(fixture_tree["tone_44100_stereo"].read_bytes())
    original = source.read_bytes()
    with pytest.raises(audio_io.WriteError):
        run_two_pass(source, source, base_config())
    assert source.read_bytes() == original


def test_custom_target_rate(fixture_tree, tmp_path):
    result = run_two_pass(fixture_tree["tone_48000_51"],
                          tmp_path / "out.wav",
                          base_config(target_rate=44100))
    info = audio_io.probe(tmp_path / "out.wav")
    assert info.samplerate == 44100
    assert result.after.lufs == pytest.approx(-16.0, abs=0.1)


def test_pcm16_output_subtype(fixture_tree, tmp_path):
    run_two_pass(fixture_tree["tone_44100_stereo"], tmp_path / "out16.wav",
                 base_config(output_subtype="PCM_16"))
    info = audio_io.probe(tmp_path / "out16.wav")
    assert info.subtype == "PCM_16"
