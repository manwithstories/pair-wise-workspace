"""确定性样例夹具生成：正常/截断/坏时间戳/漂移/重复/跨日回绕全覆盖。

所有随机量都来自显式种子，重跑同一参数产出逐字节一致的输入文件。
"""
from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path

from .media import make_audio_fixture, make_video_fixture
from .timecode import format_timecode


@dataclass
class FixtureBundle:
    """一次样例生成的全部产物路径与事实参数。"""

    workdir: Path
    media: Path
    audio: Path
    subtitles: Path
    timecodes: Path
    frames: int
    rate: Fraction
    gop: int
    segment_count: int
    expected_cue_blocks: int
    drift_injected: int = 0
    bad_timecodes: int = 0


def _srt_timestamp(frame: int, rate: Fraction) -> str:
    sec = Fraction(frame, rate)
    hour = int(sec // 3600)
    minute = int((sec % 3600) // 60)
    second = int(sec % 60)
    milli = int((sec - int(sec)) * 1000)
    return f"{hour:02d}:{minute:02d}:{second:02d},{milli:03d}"


def build_fixtures(
    workdir: Path,
    seed: int = 42,
    frames: int = 1200,
    gop: int = 100,
    corrupt_tail: bool = True,
    partial_index: bool = True,
) -> FixtureBundle:
    """在 workdir 下生成媒体、字幕与时间码三类夹具。"""

    workdir.mkdir(parents=True, exist_ok=True)
    rate = Fraction(24)
    media = workdir / "clip.mov"
    audio = workdir / "clip.wav"
    info = make_video_fixture(media, frames=frames, rate_fps=rate, gop=gop)
    make_audio_fixture(audio, frames=frames, rate_fps=rate, seed=seed)
    keyframes = [k.index for k in info.keyframes]
    segment_count = len(keyframes)

    subtitles = workdir / "subtitles.srt"
    cue_blocks = _write_subtitles(subtitles, frames, rate, corrupt_tail)
    timecodes = workdir / "timecodes.txt"
    self_drift, self_bad = _write_timecodes(
        timecodes, keyframes, rate, seed
    )
    bundle = FixtureBundle(
        workdir=workdir, media=media, audio=audio,
        subtitles=subtitles, timecodes=timecodes,
        frames=frames, rate=rate, gop=gop,
        segment_count=segment_count,
        expected_cue_blocks=cue_blocks,
    )
    bundle.drift_injected = self_drift
    bundle.bad_timecodes = self_bad
    return bundle


def _write_subtitles(
    path: Path, frames: int, rate: Fraction, corrupt_tail: bool
) -> int:
    """10 帧间距一条 cue；尾部保留一条时间轴被截断的 cue。"""

    spacing = 10
    cue_frames = list(range(0, frames - spacing, spacing))
    lines: list[str] = []
    blocks = 0
    for seq, start in enumerate(cue_frames, start=1):
        end = min(start + 8, frames - 1)
        lines.append(
            f"{seq}\n"
            f"{_srt_timestamp(start, rate)} --> {_srt_timestamp(end, rate)}\n"
            f"cue {seq:03d} 现场字幕\n"
        )
        blocks += 1
    text = "\n".join(lines)
    if corrupt_tail:
        # 时间轴被截断：缺少结束时间戳，解析器须记错误并隔离
        last_start = _srt_timestamp(cue_frames[-1] + spacing, rate)
        text += f"\n{blocks + 1}\n{last_start} --> 00:00:49,9"
        blocks += 1
    path.write_text(text, encoding="utf-8")
    return blocks


def _write_timecodes(
    path: Path, keyframes: list[int], rate: Fraction, seed: int
) -> tuple[int, int]:
    """每个关键帧一条 SMPTE 记录，注入可恢复漂移。

    返回 (漂移注入条数, 坏码条数=0)。坏时间戳与跨日回绕由测试层单独
    构造，避免改变演示夹具的分段边界与 roundtrip 分母。
    """

    lines = ["# rate=24 现场设备时间码清单", f"# seed={seed}"]
    drift_frames = 0
    for i, frame in enumerate(keyframes):
        adjusted = frame
        if i in (4, 5, 6):  # 可恢复漂移：+1 帧，低于 0.25s 阈值
            adjusted += 1
            drift_frames += 1
        lines.append(format_timecode(adjusted, rate, drop=False))
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    return drift_frames, 0
