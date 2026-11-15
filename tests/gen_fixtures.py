"""生成样例时间线 fixtures/tl_v1.json 与 fixtures/tl_v2.json。

几何设计（fps=25，确定性、无随机）：
- V1(priority=30): 60 条片段，[i*50, i*50+45)，部分带 0.5x/2x/3x 变速；
- V2(priority=20): 20 条完全落在 V1 片段内（将被整段让位）
  + 36 条填隙 + 4 条零长；
- V3(priority=10): 20 条完全落在 V1 片段内（让位）+ 20 条填隙；
- A1: 20 条音频片段，恰好覆盖 [0, 2995)，与视频对齐。
合计 180 条片段、40 条冲突、4 条零长，消解后 140 条区间、偏差 0 帧。
另生成 fixtures/tl_invalid_negative.json 用于负时长错误路径。
"""
from __future__ import annotations

import json
import sys
from fractions import Fraction
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from src.formats import v1, v2
from src.timeline.model import Clip, TimeRange, Timeline, Track

FIXTURES = Path(__file__).resolve().parent.parent / "fixtures"


def _clip(cid: str, start: int, dur: int, speed: Fraction = Fraction(1),
          src_start: int = 0) -> Clip:
    return Clip(
        clip_id=cid,
        range=TimeRange(Fraction(start), Fraction(dur)),
        source_range=TimeRange(Fraction(src_start), Fraction(dur) * speed),
        speed=speed)


def build_timeline() -> Timeline:
    tl = Timeline(fps=Fraction(25), name="demo",
                  metadata={"project": "docu-conform"})

    v1_track = Track(kind="video", index=0, priority=30)
    for i in range(60):
        speed = Fraction(1)
        if i % 13 == 0:
            speed = Fraction(3)
        elif i % 11 == 0:
            speed = Fraction(1, 2)
        elif i % 7 == 0:
            speed = Fraction(2)
        v1_track.clips.append(
            _clip(f"v1_{i:03d}", i * 50, 45, speed, src_start=i * 200))
    tl.tracks.append(v1_track)

    v2_track = Track(kind="video", index=1, priority=20)
    for i in range(20):  # 完全落在 V1 片段内 -> 整段让位
        v2_track.clips.append(
            _clip(f"v2_drop_{i:03d}", i * 50 + 10, 20, src_start=500 + i * 40))
    for i in range(20, 56):  # 填隙，不冲突
        v2_track.clips.append(
            _clip(f"v2_gap_{i:03d}", i * 50 + 45, 5, src_start=i * 30))
    for i in range(56, 60):  # 零长片段，不参与消解
        v2_track.clips.append(
            _clip(f"v2_zero_{i:03d}", i * 50 + 45, 0, src_start=0))
    tl.tracks.append(v2_track)

    v3_track = Track(kind="video", index=2, priority=10)
    for i in range(20, 40):  # 完全落在 V1 片段内 -> 整段让位
        v3_track.clips.append(
            _clip(f"v3_drop_{i:03d}", i * 50 + 15, 20, src_start=900 + i * 40))
    for i in range(20):  # 填隙，不冲突
        v3_track.clips.append(
            _clip(f"v3_gap_{i:03d}", i * 50 + 45, 5, src_start=i * 70))
    tl.tracks.append(v3_track)

    a1 = Track(kind="audio", index=3, priority=0)
    for i in range(20):
        dur = 150 if i < 19 else 145  # 19*150 + 145 = 2995，与视频对齐
        a1.clips.append(_clip(f"a1_{i:03d}", i * 150, dur, src_start=i * 300))
    tl.tracks.append(a1)
    return tl


def main() -> None:
    FIXTURES.mkdir(exist_ok=True)
    tl = build_timeline()
    v2_data = v2.serialize(tl)
    v1_data = v1.serialize(tl)
    (FIXTURES / "tl_v2.json").write_text(
        json.dumps(v2_data, ensure_ascii=False, indent=2) + "\n", "utf-8")
    (FIXTURES / "tl_v1.json").write_text(
        json.dumps(v1_data, ensure_ascii=False, indent=2) + "\n", "utf-8")

    bad = json.loads(json.dumps(v2_data))
    bad["tracks"][0]["clips"][0]["duration_frames"] = -5
    (FIXTURES / "tl_invalid_negative.json").write_text(
        json.dumps(bad, ensure_ascii=False, indent=2) + "\n", "utf-8")

    print(f"clips={tl.clip_count()} duration={tl.duration} "
          f"-> {FIXTURES}/tl_v1.json, tl_v2.json, tl_invalid_negative.json")


if __name__ == "__main__":
    main()
