"""确定性夹具生成器：成功、损坏及边界素材，全部本地生成，无外部文件。

固定种子 relaymix.DETERMINISTIC_SEED。可作为模块导入，也可独立运行：
    python tests/make_fixtures.py <root> [fixture_name]
fixture 取值：normal | truncated_wav | short_data | illegal_channels |
              empty_block | raw_pcm | boundary
"""

import json
import math
from pathlib import Path
import sys

import numpy as np
import soundfile as sf

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from relaymix import DETERMINISTIC_SEED  # noqa: E402

WAV_DIR = "wav"
MANIFEST_NAME = "manifest.json"

# 正常作业：主基准 48kHz、双声道、2400 采样/窗口（50ms），共 6 个窗口。
MASTER_RATE = 48000
FRAME_SIZE = 2400
OUTPUT_END = 14400


def _tone(rate: int, frames: int, freq: float, phase: float = 0.0) -> np.ndarray:
    rng = np.random.default_rng(DETERMINISTIC_SEED + int(rate) + int(freq))
    t = np.arange(frames, dtype=np.float64) / rate
    # 低幅度正弦 + 确定性微噪声，避免求和削波。
    return 0.35 * np.sin(2 * math.pi * freq * t + phase) + 0.01 * rng.standard_normal(frames)


def _write_wav(path: Path, samples: np.ndarray, rate: int,
               subtype: str = "PCM_16") -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    sf.write(str(path), samples, rate, subtype=subtype)


def _stereo(rate: int, frames: int, freq: float) -> np.ndarray:
    left = _tone(rate, frames, freq, 0.0)
    right = _tone(rate, frames, freq + 37.0, math.pi / 2)
    return np.stack([left, right], axis=1)


def _normal_wavs(root: Path) -> list[dict]:
    wav = root / WAV_DIR
    specs = []
    # 轨道 A：48kHz 原生速率，两段，中间交叠。
    a0 = _stereo(48000, 4800, 440.0)
    _write_wav(wav / "a0.wav", a0, 48000)
    specs.append(dict(block_id="A0", path=f"{WAV_DIR}/a0.wav",
                      start_tick=0, length_ticks=4800))
    a1 = _stereo(48000, 4800, 523.25)
    _write_wav(wav / "a1.wav", a1, 48000)
    specs.append(dict(block_id="A1", path=f"{WAV_DIR}/a1.wav",
                      start_tick=4000, length_ticks=4800))
    # 轨道 B：24kHz 重采样 + 单声道映射到右声道。
    b0_m = _tone(24000, 3000, 660.0)
    _write_wav(wav / "b0.wav", b0_m[:, None], 24000)
    specs.append(dict(block_id="B0", path=f"{WAV_DIR}/b0.wav",
                      start_tick=1200, length_ticks=6000))
    # 裸 16bit 小端 PCM，32kHz 双声道，清单显式声明格式。
    c0 = _stereo(32000, 2000, 880.0)
    pcm16 = np.clip(c0, -1.0, 1.0)
    pcm16 = np.rint(pcm16 * 32767).astype("<i2")
    (wav / "c0.pcm").write_bytes(pcm16.tobytes())
    specs.append(dict(block_id="C0", path=f"{WAV_DIR}/c0.pcm",
                      start_tick=9000, length_ticks=3000,
                      pcm_rate=32000, pcm_channels=2, pcm_width=2))
    return specs


def _manifest(tracks: list[dict], *, policy: str,
              output_end: int = OUTPUT_END, frame_size: int = FRAME_SIZE,
              output_channels: int = 2, overlaps: list[dict] | None = None) -> dict:
    return {
        "master_rate": MASTER_RATE,
        "output_start": 0,
        "output_end": output_end,
        "frame_size": frame_size,
        "output_channels": output_channels,
        "bad_block_policy": policy,
        "tracks": tracks,
        "overlaps": overlaps or [],
    }


def build_normal(root: str | Path, *, policy: str = "abort",
                 duplicate: bool = True) -> Path:
    """正常素材：含交叠、异速率、裸 PCM，并默认重复投递同内容块。"""
    root = Path(root)
    specs = _normal_wavs(root)
    by_id = {s["block_id"]: s for s in specs}
    tracks = [
        {"name": "voice", "channel_map": [0, 1],
         "blocks": [by_id["A0"], by_id["A1"]]},
        {"name": "music", "channel_map": [1],
         "blocks": [by_id["B0"]]},
        {"name": "remote", "channel_map": [0, 1],
         "blocks": [by_id["C0"]]},
    ]
    if duplicate:
        # 重复投递：同 block_id 同内容，应被幂等去重而非重复计入。
        dup = dict(by_id["A1"])
        dup = {**dup, "start_tick": 8800, "length_ticks": 4800}
        tracks[0]["blocks"].append(dup)
    overlaps = [
        {"track_a": "voice", "track_b": "music",
         "start_tick": 1200, "length_ticks": 2800},
    ]
    manifest = _manifest(tracks, policy=policy, overlaps=overlaps)
    (root / MANIFEST_NAME).write_text(
        json.dumps(manifest, indent=2, ensure_ascii=False), encoding="utf-8"
    )
    return root / MANIFEST_NAME


def _truncate(path: Path, keep_bytes: int) -> int:
    raw = path.read_bytes()
    path.write_bytes(raw[:keep_bytes])
    return keep_bytes


def build_truncated_wav(root: str | Path, *, policy: str = "abort") -> Path:
    """WAV 数据区截断：头部（含 data chunk 声明）完整，采样帧被切掉。

    头部仍可解析，故归类为 truncated_wav（声明帧数与实际可读帧数不符，
    错误中带 source_block 与 byte_offset）。
    """
    root = Path(root)
    build_normal(root, policy=policy, duplicate=False)
    target = root / WAV_DIR / "a1.wav"
    raw = target.read_bytes()
    data_offset = raw.find(b"data") + 8
    # 保留头部，仅留 2 个数据字节：不足一个完整多声道帧，归类 truncated_wav。
    _truncate(target, min(len(raw) - 1, data_offset + 2))
    return root / MANIFEST_NAME


def build_short_data(root: str | Path, *, policy: str = "silence") -> Path:
    """数据区短于头部声明：保留完整 WAV 头，截掉尾部采样帧。"""
    root = Path(root)
    build_normal(root, policy=policy, duplicate=False)
    target = root / WAV_DIR / "b0.wav"
    raw = target.read_bytes()
    _truncate(target, len(raw) - 1200)
    return root / MANIFEST_NAME


def build_illegal_channels(root: str | Path, *, policy: str = "silence") -> Path:
    """非法声道数：手工把 WAV 头 nChannels 改成 0。"""
    root = Path(root)
    build_normal(root, policy=policy, duplicate=False)
    target = root / WAV_DIR / "a0.wav"
    raw = bytearray(target.read_bytes())
    raw[22] = 0
    raw[23] = 0
    target.write_bytes(bytes(raw))
    return root / MANIFEST_NAME


def build_empty_block(root: str | Path, *, policy: str = "silence") -> Path:
    """空块：只写 WAV 头、data 长度为 0。"""
    root = Path(root)
    build_normal(root, policy=policy, duplicate=False)
    target = root / WAV_DIR / "a0.wav"
    empty = _stereo(48000, 0, 440.0)
    sf.write(str(target), empty, 48000)
    return root / MANIFEST_NAME


def build_boundary(root: str | Path) -> Path:
    """边界素材：总长不是 frame_size 整数倍，末尾产生不完整窗口。"""
    root = Path(root)
    wav = root / WAV_DIR
    frames = 5000  # 48000 下 5000 采样，frame_size=2400 -> 2 完整 + 1 不完整
    samples = _stereo(48000, frames, 330.0)
    _write_wav(wav / "edge.wav", samples, 48000)
    tracks = [
        {"name": "edge", "channel_map": [0, 1],
         "blocks": [dict(block_id="E0", path=f"{WAV_DIR}/edge.wav",
                         start_tick=0, length_ticks=frames)]}
    ]
    manifest = _manifest(tracks, policy="abort", output_end=frames)
    (root / MANIFEST_NAME).write_text(
        json.dumps(manifest, indent=2), encoding="utf-8"
    )
    return root / MANIFEST_NAME


FIXTURES = {
    "normal": lambda root: build_normal(root),
    "truncated_wav": lambda root: build_truncated_wav(root),
    "short_data": lambda root: build_short_data(root),
    "illegal_channels": lambda root: build_illegal_channels(root),
    "empty_block": lambda root: build_empty_block(root),
    "boundary": build_boundary,
}


def main(argv: list[str]) -> int:
    if len(argv) < 2:
        print(__doc__)
        return 2
    root = Path(argv[1])
    name = argv[2] if len(argv) > 2 else "normal"
    if name not in FIXTURES:
        print(f"unknown fixture: {name}; choices={sorted(FIXTURES)}")
        return 2
    path = FIXTURES[name](root)
    print(json.dumps({"fixture": name, "manifest": str(path)}, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
