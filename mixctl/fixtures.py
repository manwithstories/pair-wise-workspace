"""生成演示用的 5 轨工程与源 WAV（全部位于输出目录内，不引用外部文件）。

轨道构成:
* dialogue 44.1 kHz 单声道
* music    48 kHz 立体声
* sfx      96 kHz 立体声（含一处瞬时超幅，用于削波统计）
* ambience 48 kHz 立体声（全程静音，验证 RMS 为 null）
* foley    44.1 kHz 立体声
工程时长 60 秒，目标采样率 48 kHz，两级总线（子总线 -> master）。
"""

from __future__ import annotations

import json
from pathlib import Path

import numpy as np
import soundfile as sf

DURATION = 60.0
TARGET_SR = 48000


def _tone(frequencies, length, sample_rate, amplitudes, phase_seed=0.0):
    timeline = np.arange(length, dtype=np.float64) / sample_rate
    signal = np.zeros(length, dtype=np.float64)
    for index, (frequency, amplitude) in enumerate(zip(frequencies, amplitudes)):
        phase = phase_seed + index * 0.7
        signal += amplitude * np.sin(
            2.0 * np.pi * frequency * timeline + phase
        )
    return signal.astype(np.float64)


def _stereo(mono: np.ndarray, width: float = 0.12) -> np.ndarray:
    left = mono
    right = mono * (1.0 - width) + np.roll(mono, 37) * width
    return np.stack([left, right], axis=1).astype(np.float64)


def _write_wav(path: Path, data: np.ndarray, sample_rate: int) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    sf.write(str(path), data, sample_rate, subtype="FLOAT")


def generate_fixtures(out_dir: str | Path) -> Path:
    root = Path(out_dir)
    wav_dir = root / "wav"
    root.mkdir(parents=True, exist_ok=True)

    # dialogue: 44.1k 单声道，人声基频附近的复合音，整体 -12 dB 量级
    sr_d = 44100
    dialogue = _tone(
        [180.0, 360.0, 720.0],
        int(DURATION * sr_d), sr_d, [0.12, 0.06, 0.025],
    )
    _write_wav(wav_dir / "dialogue.wav", dialogue, sr_d)

    # music: 48k 立体声，和声铺底，整体较安静
    sr_m = 48000
    music_mono = _tone(
        [110.0, 220.0, 330.0, 440.0],
        int(DURATION * sr_m), sr_m, [0.07, 0.045, 0.025, 0.02], phase_seed=1.3,
    )
    _write_wav(wav_dir / "music.wav", _stereo(music_mono), sr_m)

    # sfx: 96k 立体声，中后段一次瞬时冲击超过 1.0
    sr_s = 96000
    sfx_mono = _tone(
        [1200.0, 2400.0], int(DURATION * sr_s), sr_s, [0.12, 0.06]
    )
    impact_center = int(30.0 * sr_s)
    envelope = np.exp(
        -np.abs(np.arange(sfx_mono.shape[0]) - impact_center) / (0.05 * sr_s)
    )
    sfx_mono = sfx_mono + 0.48 * envelope
    _write_wav(wav_dir / "sfx.wav", _stereo(sfx_mono, 0.2), sr_s)

    # ambience: 48k 立体声，全程静音
    _write_wav(
        wav_dir / "ambience.wav",
        np.zeros((int(DURATION * sr_m), 2), dtype=np.float64), sr_m,
    )

    # foley: 44.1k 立体声，高频细节
    foley_mono = _tone(
        [2500.0, 5200.0, 7800.0],
        int(DURATION * sr_d), sr_d, [0.05, 0.03, 0.02], phase_seed=2.1,
    )
    _write_wav(wav_dir / "foley.wav", _stereo(foley_mono, 0.3), sr_d)

    project = {
        "target_sr": TARGET_SR,
        "duration": DURATION,
        "hop": 64,
        "clip_threshold": 1.0,
        "master_bus": "master",
        "buses": [
            {"name": "vo", "output": "master"},
            {"name": "bed", "output": "master"},
            {"name": "master", "output": None},
        ],
        "tracks": [
            {
                "name": "dialogue", "file": "wav/dialogue.wav", "channels": 1,
                "bus": "vo", "gain_db": -2.0, "muted": False,
                "automation": [
                    {"time": 0.0, "db": -6.0},
                    {"time": 10.0, "db": -1.0},
                    {"time": 30.0, "db": -1.0},
                    {"time": 45.0, "db": -8.0},
                    {"time": 45.0, "db": -4.0},
                    {"time": 60.0, "db": -6.0},
                ],
            },
            {
                "name": "music", "file": "wav/music.wav", "channels": 2,
                "bus": "bed", "gain_db": -6.0, "muted": False,
                "automation": [
                    {"time": 0.0, "db": -3.0},
                    {"time": 20.0, "db": 0.0},
                    {"time": 50.0, "db": -5.0},
                ],
            },
            {
                "name": "sfx", "file": "wav/sfx.wav", "channels": 2,
                "bus": "bed", "gain_db": 0.0, "muted": False,
                "automation": [
                    {"time": 29.5, "db": -6.0},
                    {"time": 29.98, "db": -6.0},
                    {"time": 30.0, "db": 2.6},
                    {"time": 30.02, "db": -6.0},
                    {"time": 30.5, "db": -6.0},
                ],
            },
            {
                "name": "ambience", "file": "wav/ambience.wav", "channels": 2,
                "bus": "bed", "gain_db": 0.0, "muted": False,
                "automation": [{"time": 0.0, "db": -2.0}],
            },
            {
                "name": "foley", "file": "wav/foley.wav", "channels": 2,
                "bus": "bed", "gain_db": -8.0, "muted": False,
                "automation": [
                    {"time": 0.0, "db": 0.0},
                    {"time": 60.0, "db": -10.0},
                ],
            },
        ],
    }
    project_path = root / "demo.mix.json"
    with project_path.open("w", encoding="utf-8") as handle:
        json.dump(project, handle, ensure_ascii=False, indent=2)
        handle.write("\n")
    return project_path
