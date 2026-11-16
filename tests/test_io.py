"""I/O tests: validation, interleaving order, padding, atomic writes."""

from __future__ import annotations

import math
import os
from array import array
from pathlib import Path

import pytest
import soundfile as sf

from audio_tool import io as audio_io
from audio_tool.model import (
    AudioBlock,
    InputFormatError,
    UnsupportedError,
    UsageError,
)


def test_probe_reports_header_facts(fixture_tree):
    info = audio_io.probe(fixture_tree["tone_48000_51"])
    assert info.samplerate == 48000
    assert info.channels == 6
    assert info.layout.name == "5.1"
    assert info.subtype == "PCM_24"
    assert info.frames > 0


def test_probe_rejects_unsupported_channel_count(fixture_tree):
    with pytest.raises(UnsupportedError) as excinfo:
        audio_io.probe(fixture_tree["broken_bad_channels"])
    assert "channel count 10" in excinfo.value.message


def test_probe_rejects_out_of_range_rate(fixture_tree):
    with pytest.raises(UnsupportedError) as excinfo:
        audio_io.probe(fixture_tree["broken_bad_rate"])
    assert excinfo.value.details["samplerate"] == 4000


def test_probe_detects_truncated_container(fixture_tree):
    with pytest.raises(InputFormatError) as excinfo:
        audio_io.probe(fixture_tree["broken_truncated"])
    assert "truncated" in excinfo.value.message.lower()


def test_probe_rejects_missing_file(tmp_path):
    with pytest.raises(UsageError) as excinfo:
        audio_io.probe(tmp_path / "nope.wav")
    assert excinfo.value.code == "input_missing"


def test_probe_rejects_non_audio(tmp_path):
    bogus = tmp_path / "not_audio.wav"
    bogus.write_bytes(b"this is not a wave file at all")
    with pytest.raises(InputFormatError):
        audio_io.probe(bogus)


def test_interleave_preserves_channel_order():
    planes = [array("d", [0.1, 0.2, 0.3]),
              array("d", [0.4, 0.5, 0.6])]
    packed = audio_io.interleave(planes, 3)
    assert list(packed) == pytest.approx(
        [0.1, 0.4, 0.2, 0.5, 0.3, 0.6])


def test_deinterleave_roundtrip():
    planes = [array("d", [0.1, 0.2]), array("d", [0.3, 0.4])]
    packed = audio_io.interleave(planes, 2)
    restored = audio_io.deinterleave(packed, 2)
    assert list(restored[0]) == pytest.approx([0.1, 0.2])
    assert list(restored[1]) == pytest.approx([0.3, 0.4])


def test_block_reader_reports_zero_padding(fixture_tree):
    info = audio_io.probe(fixture_tree["mono_odd_tail"])
    with audio_io.BlockReader(info, 4096) as reader:
        blocks = list(reader)
        padded = reader.zero_padded_frames
    valid = sum(block.frames for block in blocks)
    assert valid == info.frames
    assert padded > 0
    assert padded == len(blocks) * 4096 - info.frames
    # Every plane is block-sized even though the last block is short.
    assert all(len(block.planes[0]) >= 4096 for block in blocks[:-1])


def test_block_reader_does_not_pad_when_divisible(tmp_path):
    path = tmp_path / "even.wav"
    data = array("d", [0.1] * 8192)
    with sf.SoundFile(str(path), "w", samplerate=48000, channels=1,
                      subtype="PCM_16", format="WAV") as handle:
        handle.buffer_write(data, "float64")
    info = audio_io.probe(path)
    with audio_io.BlockReader(info, 4096) as reader:
        blocks = list(reader)
        assert reader.zero_padded_frames == 0
    assert [block.frames for block in blocks] == [4096, 4096]


def test_block_reader_roundtrips_samples(tmp_path):
    path = tmp_path / "values.wav"
    values = [math.sin(2 * math.pi * i / 64) * 0.5 for i in range(1000)]
    with sf.SoundFile(str(path), "w", samplerate=48000, channels=1,
                      subtype="PCM_24", format="WAV") as handle:
        handle.buffer_write(array("d", values), "float64")
    info = audio_io.probe(path)
    collected: list[float] = []
    with audio_io.BlockReader(info, 128) as reader:
        for block in reader:
            collected.extend(block.planes[0][:block.frames])
    assert len(collected) == len(values)
    for original, restored in zip(values, collected):
        assert restored == pytest.approx(original, abs=1e-6)


def test_nan_samples_are_rejected(fixture_tree):
    info = audio_io.probe(fixture_tree["broken_nan"])
    with pytest.raises(InputFormatError) as excinfo:
        with audio_io.BlockReader(info, 1024) as reader:
            list(reader)
    assert "NaN" in excinfo.value.message


def test_count_clipped_counts_both_polarities():
    assert audio_io.count_clipped([0.5, 1.0, -1.0, 1.5, -0.99]) == 3


def test_write_wav_atomic_replaces_destination(tmp_path):
    destination = tmp_path / "nested" / "out.wav"
    blocks = iter([AudioBlock([array("d", [0.25] * 100)], 100)])
    audio_io.write_wav_atomic(destination, blocks, 48000, 1, "PCM_24")
    assert destination.exists()
    info = audio_io.probe(destination)
    assert info.frames == 100
    assert not list(destination.parent.glob(".*.part"))


def test_write_wav_atomic_leaves_no_partial_file_on_failure(tmp_path):
    destination = tmp_path / "aborted.wav"

    def bad_blocks():
        yield AudioBlock([array("d", [0.1] * 100)], 100)
        raise RuntimeError("simulated failure mid-write")

    with pytest.raises(audio_io.WriteError):
        audio_io.write_wav_atomic(destination, bad_blocks(), 48000, 1, "PCM_24")
    assert not destination.exists()
    assert not [p for p in destination.parent.iterdir() if p.name.startswith(".")]


def test_write_wav_atomic_refuses_zero_frames(tmp_path):
    destination = tmp_path / "empty.wav"
    with pytest.raises(audio_io.WriteError):
        audio_io.write_wav_atomic(destination, iter([]), 48000, 1, "PCM_24")
    assert not destination.exists()


def test_guard_output_path_refuses_overwriting_source(fixture_tree):
    source = fixture_tree["tone_44100_stereo"]
    with pytest.raises(audio_io.WriteError):
        audio_io.guard_output_path(source, source)


def test_ensure_inside_accepts_and_rejects(tmp_path):
    root = tmp_path / "root"
    root.mkdir()
    inside = root / "sub" / "file.wav"
    assert audio_io.ensure_inside(root, inside) == inside.resolve()
    with pytest.raises(UsageError) as excinfo:
        audio_io.ensure_inside(root, tmp_path / "outside.wav")
    assert excinfo.value.code == "path_outside_workspace"


def test_probe_accepts_float_and_double(tmp_path):
    for subtype in ("FLOAT", "DOUBLE"):
        path = tmp_path / f"{subtype}.wav"
        with sf.SoundFile(str(path), "w", samplerate=48000, channels=2,
                          subtype=subtype, format="WAV") as handle:
            handle.buffer_write(array("d", [0.1] * 200), "float64")
        info = audio_io.probe(path)
        assert info.subtype == subtype
