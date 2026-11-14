"""WAV probing, streaming readers, generated fixtures, and WAV writing."""
from __future__ import annotations

from dataclasses import dataclass
import json
import math
import os
from pathlib import Path
from typing import Any

import numpy as np
import soundfile as sf

from .resample import PolyphaseResampler


class AssetError(OSError):
    """Raised when audio assets cannot be read or validated."""


@dataclass(frozen=True)
class AudioInfo:
    frames: int
    samplerate: int
    channels: int


def probe_audio(path: str | Path) -> AudioInfo:
    audio_path = Path(path)
    if not audio_path.is_file():
        raise AssetError(f"missing asset '{audio_path}'")
    try:
        info = sf.info(str(audio_path))
    except (RuntimeError, OSError) as exc:
        raise AssetError(f"cannot read asset '{audio_path}': {exc}") from exc
    if info.samplerate <= 0:
        raise AssetError(f"asset '{audio_path}' has non-positive sample rate")
    if info.channels not in (1, 2):
        raise AssetError(f"asset '{audio_path}' has invalid channel count {info.channels}; expected 1 or 2")
    if info.frames < 0:
        raise AssetError(f"asset '{audio_path}' has invalid frame count")
    return AudioInfo(int(info.frames), int(info.samplerate), int(info.channels))


def _fit_channels(audio: np.ndarray, channels: int) -> np.ndarray:
    if audio.shape[0] == channels:
        return audio
    if channels == 2 and audio.shape[0] == 1:
        return np.repeat(audio, 2, axis=0)
    if channels == 1 and audio.shape[0] == 2:
        return audio.mean(axis=0, keepdims=True)
    raise AssetError(f"cannot convert {audio.shape[0]} channel(s) to {channels}")


def write_wav(path: str | Path, audio: np.ndarray, sample_rate: int) -> None:
    data = np.asarray(audio, dtype=np.float64)
    if data.ndim == 1:
        data = data[None, :]
    if data.ndim != 2 or data.shape[0] not in (1, 2):
        raise AssetError("WAV data must be one- or two-channel frames")
    if sample_rate <= 0:
        raise AssetError("WAV sample rate must be positive")
    output = Path(path)
    output.parent.mkdir(parents=True, exist_ok=True)
    try:
        sf.write(str(output), data.T, int(sample_rate), subtype="FLOAT", format="WAV")
    except (RuntimeError, OSError) as exc:
        raise AssetError(f"cannot write WAV '{output}': {exc}") from exc

class AudioReader:
    """Read an asset in growing chunks, resampling and adapting channels."""

    def __init__(self, path: str | Path, sample_rate: int, channels: int):
        if sample_rate <= 0:
            raise AssetError("target sample rate must be positive")
        if channels not in (1, 2):
            raise AssetError("target channel count must be 1 or 2")
        self.path = Path(path)
        self.info = probe_audio(self.path)
        self.sample_rate = int(sample_rate)
        self.channels = int(channels)
        try:
            self._file = sf.SoundFile(str(self.path), mode="r")
        except (RuntimeError, OSError) as exc:
            raise AssetError(f"cannot open asset '{self.path}': {exc}") from exc
        self._frames_read = 0
        self._output_consumed = 0
        self._pooled = np.zeros((self.info.channels, 0), dtype=np.float64)
        self._finished = False
        if self.info.samplerate == self.sample_rate:
            self._resampler: PolyphaseResampler | None = None
            self.total_frames = self.info.frames
        else:
            self._resampler = PolyphaseResampler(self.info.samplerate, self.sample_rate, self.info.channels, self.info.frames)
            self.total_frames = self._resampler.output_frames

    def _read_source(self, count: int) -> np.ndarray:
        remaining = self.info.frames - self._frames_read
        count = max(0, min(int(count), remaining))
        if count == 0:
            return np.zeros((self.info.channels, 0), dtype=np.float64)
        try:
            block = self._file.read(count, dtype="float64", always_2d=True)
        except (RuntimeError, OSError) as exc:
            raise AssetError(f"cannot read asset '{self.path}': {exc}") from exc
        self._frames_read += block.shape[0]
        return block.T

    def read(self, frames: int) -> np.ndarray:
        if frames <= 0:
            return np.zeros((self.channels, 0), dtype=np.float64)
        remaining_output = self.total_frames - self._output_consumed
        wanted = min(int(frames), max(0, remaining_output))
        if wanted == 0:
            return np.zeros((self.channels, 0), dtype=np.float64)
        if self._resampler is None:
            source = self._read_source(wanted)
            result = _fit_channels(source, self.channels)
            self._output_consumed += result.shape[1]
            return result
        while self._pooled.shape[1] < wanted and not self._finished:
            missing = wanted - self._pooled.shape[1]
            input_need = math.ceil((missing + 2 * self._resampler.half_width + 2) * self._resampler.down / self._resampler.up)
            source = self._read_source(input_need)
            if source.shape[1]:
                produced = self._resampler.process(source)
            else:
                produced = self._resampler.finish()
                self._finished = True
            if produced.shape[1]:
                self._pooled = np.concatenate((self._pooled, produced), axis=1)
            if self._frames_read >= self.info.frames and not self._finished:
                produced = self._resampler.finish()
                self._finished = True
                if produced.shape[1]:
                    self._pooled = np.concatenate((self._pooled, produced), axis=1)
        take = min(wanted, self._pooled.shape[1])
        result = _fit_channels(self._pooled[:, :take], self.channels)
        self._pooled = self._pooled[:, take:]
        self._output_consumed += take
        return result

    def close(self) -> None:
        self._file.close()

def _normalize_rms(signal: np.ndarray, rms_db: float) -> np.ndarray:
    rms = float(np.sqrt(np.mean(np.square(signal, dtype=np.float64))))
    return signal if rms <= 0.0 else signal * (10.0 ** (rms_db / 20.0) / rms)


def _voice(rate: int, seconds: float, rng: np.random.Generator) -> np.ndarray:
    frames = int(rate * seconds)
    t = np.arange(frames, dtype=np.float64) / rate
    syllables = np.zeros(frames, dtype=np.float64)
    cursor = 0
    while cursor < frames:
        length = int(rng.uniform(0.12, 0.34) * rate)
        gap = int(rng.uniform(0.03, 0.12) * rate)
        position = np.arange(min(length, frames - cursor), dtype=np.float64)
        envelope = np.sin(np.pi * position / max(length, 1)) ** 2
        syllables[cursor : cursor + position.size] += envelope
        cursor += length + gap
    f0 = 155.0 + 24.0 * np.sin(2 * np.pi * 0.37 * t)
    carrier = np.sin(2 * np.pi * np.cumsum(f0) / rate)
    carrier += 0.35 * np.sin(4 * np.pi * np.cumsum(f0) / rate)
    breath = np.convolve(rng.normal(size=frames), np.ones(8) / 8.0, mode="same")
    voice = (0.72 * carrier + 0.28 * breath) * syllables
    return _normalize_rms(voice, -18.0)[None, :]

def _ambience(rate: int, seconds: float, rng: np.random.Generator) -> np.ndarray:
    frames = int(rate * seconds)
    t = np.arange(frames, dtype=np.float64) / rate
    left = np.zeros(frames); right = np.zeros(frames)
    for index, frequency in enumerate((92.0, 138.0, 207.0, 311.0)):
        drift = np.sin(2 * np.pi * (0.07 + index * 0.013) * t + index)
        phase = 2 * np.pi * frequency * t + 0.8 * drift
        amplitude = 0.2 / (index + 1)
        left += amplitude * np.sin(phase)
        right += amplitude * np.sin(phase + 0.25 + index * 0.1)
    texture = np.convolve(rng.normal(size=frames), np.ones(64) / 64.0, mode="same")
    left += 0.35 * texture
    right += 0.35 * np.roll(texture, 37)
    return _normalize_rms(np.stack((left, right)), -18.0)

def _music(rate: int, seconds: float, rng: np.random.Generator) -> np.ndarray:
    frames = int(rate * seconds)
    t = np.arange(frames, dtype=np.float64) / rate
    notes = (196.0, 246.94, 293.66, 392.0, 440.0, 293.66)
    signal = np.zeros((2, frames), dtype=np.float64)
    note_index = cursor = 0
    while cursor < frames:
        length = int(rng.uniform(0.22, 0.42) * rate)
        note = notes[note_index % len(notes)]
        position = np.arange(min(length, frames - cursor), dtype=np.float64)
        attack = np.exp(-4.5 * position / max(length, 1))
        tone = (np.sin(2 * np.pi * note * position / rate) + 0.35 * np.sin(2 * np.pi * note * 2 * position / rate) + 0.15 * np.sin(2 * np.pi * note * 3 * position / rate)) * attack
        signal[0, cursor : cursor + position.size] += tone
        signal[1, cursor : cursor + position.size] += 0.96 * tone
        cursor += length
        note_index += 1
    clicks = np.zeros(frames)
    for at in range(int(0.5 * rate), frames, int(0.5 * rate)):
        clicks[at] = rng.uniform(0.1, 0.2)
    hat = np.convolve(clicks, rng.normal(size=48) * np.hanning(48), mode="same")
    signal[0] += hat
    signal[1] += 0.8 * np.roll(hat, 11)
    return _normalize_rms(signal, -18.0)

def _sfx(rate: int, seconds: float, rng: np.random.Generator) -> np.ndarray:
    frames = int(rate * seconds)
    bed = rng.normal(size=frames)
    bed = np.convolve(bed, np.ones(3) / 3.0, mode="same")
    bed = _normalize_rms(bed, -19.5)
    for at, decay, frequency in ((0.4, 0.18, 900.0), (1.7, 0.35, 420.0), (4.8, 0.25, 1300.0)):
        start = int(at * rate)
        length = min(int(decay * rate), frames - start)
        position = np.arange(length, dtype=np.float64)
        burst = np.sin(2 * np.pi * frequency * position / rate) * np.exp(-8.0 * position / max(length, 1))
        bed[start : start + length] += 0.22 * burst
    pulse_index = int(2.73 * rate)
    bed[pulse_index] = 0.94
    bed[pulse_index + 1] = 0.55
    return bed[None, :]

def generate_fixtures(out_dir: str | Path, seed: int, project_path: str | Path, seconds: float = 6.0) -> dict[str, Any]:
    output = Path(out_dir).resolve()
    project = Path(project_path).resolve()
    output.mkdir(parents=True, exist_ok=True)
    project.parent.mkdir(parents=True, exist_ok=True)
    rng = np.random.default_rng(int(seed))
    assets = {
        "vo": ("vo_44k1_mono.wav", 44100, _voice(44100, seconds, rng)),
        "amb": ("amb_48k_stereo.wav", 48000, _ambience(48000, seconds, rng)),
        "music": ("music_96k_stereo.wav", 96000, _music(96000, seconds, rng)),
        "sfx": ("sfx_44k1_mono.wav", 44100, _sfx(44100, seconds, rng)),
    }
    for filename, rate, audio in assets.values():
        write_wav(output / filename, audio, rate)
    def source(name: str) -> str:
        return Path(os.path.relpath(output / assets[name][0], project.parent)).as_posix()
    data = {
        "name": "audio-tools-demo",
        "sample_rate": 48000,
        "channels": 2,
        "tracks": [
            {
                "name": "vo",
                "source": source("vo"),
                "gain_db": 0.5,
                "sends": [{"bus": "dialogue_bus", "gain_db": 0.0}],
                "automation": {
                    "points": [
                        {"time": 0.0, "gain_db": -3.0, "curve": "linear"},
                        {"time": 0.8, "gain_db": -1.0, "curve": "hold"},
                        {"time": 1.25, "gain_db": -4.0, "curve": "exponential"},
                        {"time": 2.5, "gain_db": -2.0, "curve": "linear"},
                        {"time": 3.5, "gain_db": -1.0, "curve": "exponential"},
                        {"time": 5.0, "gain_db": -3.0, "curve": "hold"},
                    ]
                },
            },
            {
                "name": "amb",
                "source": source("amb"),
                "gain_db": -1.0,
                "sends": [{"bus": "music_bus", "gain_db": 0.0}],
            },
            {
                "name": "music",
                "source": source("music"),
                "gain_db": -1.0,
                "sends": [{"bus": "music_bus", "gain_db": 0.0}],
                "automation": {
                    "points": [
                        {"time": 0.0, "gain_db": 0.0, "curve": "linear"},
                        {"time": 2.0, "gain_db": -3.0, "curve": "exponential"},
                        {"time": 4.0, "gain_db": 0.0, "curve": "linear"},
                    ]
                },
            },
            {
                "name": "sfx",
                "source": source("sfx"),
                "gain_db": 0.0,
                "sends": [
                    {"bus": "dialogue_bus", "gain_db": -9.0},
                    {"bus": "music_bus", "gain_db": -12.0},
                ],
            },
        ],
        "buses": [
            {"name": "dialogue_bus", "gain_db": 0.0, "sends": [{"bus": "master", "gain_db": 0.0}]},
            {"name": "music_bus", "gain_db": -2.0, "sends": [{"bus": "master", "gain_db": 0.0}]},
        ],
        "master": {"name": "master", "gain_db": -1.5},
    }
    with project.open("w", encoding="utf-8") as handle:
        json.dump(data, handle, indent=2, ensure_ascii=False)
        handle.write("\n")
    return data
