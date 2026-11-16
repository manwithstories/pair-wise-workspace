"""本地夹具与样例工程生成。

全部素材由确定性公式(固定相位、无 RNG 依赖)合成,只写仓库内目录,
不引用任何外部文件。generate 子命令产出:
- 4 条可混音轨道(44.1k mono、48k stereo、96k 多声道、48k mono 偏移);
- 空文件与损坏 WAV 两个故障夹具;
- project.json(标准工程)与 project_best_effort.json(含坏轨策略演示)。
"""

from __future__ import annotations

import json
from pathlib import Path

import numpy as np
import soundfile as sf

from .types import ErrorPolicy

PROJECT_RATE = 48000
PROJECT_FRAMES = 240_000  # 5 秒 @48k


def _tone(freq_hz: float, frames: int, sample_rate: int, amp: float,
          phase: float = 0.0) -> np.ndarray:
    t = np.arange(frames, dtype=np.float64) / sample_rate
    return (amp * np.sin(2.0 * np.pi * freq_hz * t + phase)).astype(np.float64)


def _write_wav(path: Path, data: np.ndarray, sample_rate: int) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    sf.write(str(path), data, sample_rate, subtype="FLOAT")


def generate_fixtures(out_dir: str | Path) -> dict[str, str]:
    """在 out_dir 下生成全部夹具,返回关键文件相对路径映射。"""

    root = Path(out_dir)
    wav_dir = root / "wav"
    wav_dir.mkdir(parents=True, exist_ok=True)

    # 1) 对白轨:44.1k mono,起始偏移 0,带自动化(淡入后压限)
    dialogue = _tone(220.0, 44100 * 5, 44100, amp=0.6, phase=0.0)
    dialogue_path = wav_dir / "dialogue_44k_mono.wav"
    _write_wav(dialogue_path, dialogue, 44100)

    # 2) 音乐轨:48k stereo,偏移 24000(0.5s),静态增益 0.4
    music_l = _tone(110.0, 48000 * 4, 48000, amp=0.5)
    music_r = _tone(110.0, 48000 * 4, 48000, amp=0.5, phase=np.pi / 2)
    music = np.stack([music_l, music_r], axis=1)
    music_path = wav_dir / "music_48k_stereo.wav"
    _write_wav(music_path, music, 48000)

    # 3) 环境声轨:96k 三声道,偏移 48000(1s),显式 downmix 不丢声道
    amb_frames = 96000 * 3
    amb = np.stack(
        [
            _tone(55.0, amb_frames, 96000, amp=0.35),
            _tone(55.0, amb_frames, 96000, amp=0.30, phase=np.pi / 4),
            _tone(82.0, amb_frames, 96000, amp=0.25),
        ],
        axis=1,
    )
    amb_path = wav_dir / "ambience_96k_3ch.wav"
    _write_wav(amb_path, amb, 96000)

    # 4) 战斗音效:48k mono,偏移 96000(2s),大振幅制造可统计削波
    sfx = _tone(880.0, 48000 * 2, 48000, amp=1.4)
    sfx_path = wav_dir / "sfx_48k_mono.wav"
    _write_wav(sfx_path, sfx, 48000)

    # 故障夹具
    empty_path = wav_dir / "empty_broken.wav"
    empty_path.write_bytes(b"")
    corrupt_path = wav_dir / "corrupt_broken.wav"
    corrupt_path.write_bytes(b"RIFF0000WAVEnot-a-real-wave-file")

    project = _build_project(dialogue_path, music_path, amb_path, sfx_path)
    project_path = root / "project.json"
    project_path.write_text(json.dumps(project, ensure_ascii=False, indent=2),
                            encoding="utf-8")

    be_project = json.loads(json.dumps(project))
    be_project["error_policy"] = ErrorPolicy.BEST_EFFORT.value
    be_project["tracks"].append(
        {
            "name": "broken_track",
            "path": "wav/empty_broken.wav",
            "offset": 0,
            "gain": 1.0,
        }
    )
    be_path = root / "project_best_effort.json"
    be_path.write_text(json.dumps(be_project, ensure_ascii=False, indent=2),
                       encoding="utf-8")

    return {
        "project": str(project_path),
        "project_best_effort": str(be_path),
        "empty": str(empty_path),
        "corrupt": str(corrupt_path),
    }


def _build_project(dialogue: Path, music: Path, amb: Path, sfx: Path) -> dict:
    return {
        "sample_rate": PROJECT_RATE,
        "duration_frames": PROJECT_FRAMES,
        "error_policy": ErrorPolicy.FAIL_FAST.value,
        "clip_threshold": 1.0,
        "block_frames": 1024,
        "tracks": [
            {
                "name": "dialogue",
                "path": f"wav/{dialogue.name}",
                "offset": 0,
                "channels": [0, 1],
                "gain": 1.0,
                # 6 个控制点:淡入、保持、对白段压低音乐之外的轻微起伏
                "automation": [
                    {"time": 0, "value": 0.2},
                    {"time": 4800, "value": 1.0},
                    {"time": 96000, "value": 1.0},
                    {"time": 120000, "value": 0.7},
                    {"time": 192000, "value": 0.9},
                    {"time": 239999, "value": 0.9},
                ],
            },
            {
                "name": "music",
                "path": f"wav/{music.name}",
                "offset": 24000,
                "channels": [0, 1],
                "gain": 0.4,
                "automation": [
                    {"time": 0, "value": 0.8},
                    {"time": 72000, "value": 0.8},
                    {"time": 96000, "value": 0.35},
                    {"time": 144000, "value": 0.35},
                    {"time": 168000, "value": 0.8},
                ],
            },
            {
                "name": "ambience",
                "path": f"wav/{amb.name}",
                "offset": 48000,
                "gain": 0.5,
                "routes": {"0": "L", "1": "R", "2": "L+R"},
                "automation": [
                    {"time": 0, "value": 1.0},
                    {"time": 48000, "value": 1.0},
                    {"time": 96000, "value": 0.6},
                    {"time": 192000, "value": 0.6},
                ],
            },
            {
                "name": "sfx",
                "path": f"wav/{sfx.name}",
                "offset": 96000,
                "channels": [0, 1],
                "gain": 1.0,
                "automation": [
                    {"time": 0, "value": 1.0},
                    {"time": 24000, "value": 1.0},
                    {"time": 48000, "value": 0.9},
                    {"time": 72000, "value": 0.8},
                    {"time": 95999, "value": 0.8},
                ],
            },
        ],
    }
