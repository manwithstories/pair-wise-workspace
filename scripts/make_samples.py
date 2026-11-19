#!/usr/bin/env python3
"""生成可重复的正常及损坏样例：本地 HLS/DASH 清单与音频分片。

样例清单（均生成在 --out 目录下）：
  hls.m3u8           正常 HLS（6 × 2s 正弦分片）
  dash.mpd           正常 DASH（SegmentList 形式，引用同一批分片）
  hls_dup.m3u8       重复分片：序号回退重投 seg_002（内容一致 → 去重）
  hls_conflict.m3u8  冲突分片：序号 3 两份内容不同 → 隔离区间；另有分片
                     声明时长与实际不符 → 非零对齐误差
  hls_gap.m3u8       缺失区间：引用不存在的分片 → 缺口恢复；另有分片
                     实际时长偏短 → 非零对齐误差
"""
from __future__ import annotations

import argparse
from pathlib import Path

import numpy as np
import soundfile as sf

SEG_DUR = 2.0
SEG_RATE = 44100
COUNT = 6
AMP = 0.1
FREQS = [440.0, 494.0, 523.0, 587.0, 659.0, 698.0]


def tone(seed: int, tag: int, freq: float, dur: float = SEG_DUR,
         rate: int = SEG_RATE, amp: float = AMP) -> np.ndarray:
    rng = np.random.default_rng((seed, tag))
    t = np.arange(int(round(dur * rate))) / rate
    phase = float(rng.uniform(0.0, 2.0 * np.pi))
    return (amp * np.sin(2.0 * np.pi * freq * t + phase)).astype(np.float32)


def write_seg(path: Path, data: np.ndarray, rate: int = SEG_RATE) -> None:
    sf.write(str(path), data, rate, subtype="PCM_16")


def hls(entries: list[tuple], target: int = 2) -> str:
    """entries: ("seg", uri, dur) 或 ("reset", media_sequence)。"""
    lines = ["#EXTM3U", "#EXT-X-VERSION:3",
             f"#EXT-X-TARGETDURATION:{target}", "#EXT-X-MEDIA-SEQUENCE:0"]
    for entry in entries:
        if entry[0] == "reset":
            lines.append(f"#EXT-X-MEDIA-SEQUENCE:{entry[1]}")
        else:
            _, uri, dur = entry
            lines.append(f"#EXTINF:{dur:.3f},")
            lines.append(uri)
    lines.append("#EXT-X-ENDLIST")
    return "\n".join(lines) + "\n"


def dash(names: list[str], dur_ms: int = 2000) -> str:
    urls = "\n        ".join(f'<SegmentURL media="{n}"/>' for n in names)
    total = dur_ms * len(names) / 1000.0
    return f"""<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static"
     mediaPresentationDuration="PT{total:.1f}S">
  <Period>
    <AdaptationSet mimeType="audio/wav">
      <Representation id="a" bandwidth="1411200">
        <SegmentList timescale="1000" duration="{dur_ms}" startNumber="0">
        {urls}
        </SegmentList>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>
"""


def main() -> int:
    parser = argparse.ArgumentParser(description="生成 live_delivery 样例数据")
    parser.add_argument("--out", required=True, help="输出目录")
    parser.add_argument("--seed", type=int, default=17, help="随机种子（可重复）")
    args = parser.parse_args()

    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    names = [f"seg_{i:03d}.wav" for i in range(COUNT)]

    # 正常分片：44.1kHz 单声道正弦，2.0s 整
    for i, name in enumerate(names):
        write_seg(out / name, tone(args.seed, i, FREQS[i]))

    # 冲突样例专用：同序号不同内容 + 时长不符的分片
    write_seg(out / "cseg_003_alt.wav", tone(args.seed, 103, 880.0))
    write_seg(out / "cseg_004.wav", tone(args.seed, 4, FREQS[4], dur=1.99))
    # 缺失样例专用：实际 1.995s 但声明 2.000s
    write_seg(out / "gseg_001.wav", tone(args.seed, 1, FREQS[1], dur=1.995))

    normal = [("seg", n, SEG_DUR) for n in names]
    (out / "hls.m3u8").write_text(hls(normal), encoding="utf-8")
    (out / "dash.mpd").write_text(dash(names), encoding="utf-8")

    dup = normal + [("reset", 2), ("seg", "seg_002.wav", SEG_DUR)]
    (out / "hls_dup.m3u8").write_text(hls(dup), encoding="utf-8")

    conflict = (normal[:3]
                + [("reset", 3), ("seg", "cseg_003_alt.wav", SEG_DUR)]
                + [("seg", "cseg_004.wav", SEG_DUR), ("seg", "seg_005.wav", SEG_DUR)])
    (out / "hls_conflict.m3u8").write_text(hls(conflict), encoding="utf-8")

    gap = [("seg", "seg_000.wav", SEG_DUR),
           ("seg", "gseg_001.wav", SEG_DUR),
           ("seg", "seg_002.wav", SEG_DUR),
           ("seg", "seg_003.wav", SEG_DUR),
           ("seg", "seg_004_lost.wav", SEG_DUR),  # 弱网丢失：文件不存在
           ("seg", "seg_005.wav", SEG_DUR)]
    (out / "hls_gap.m3u8").write_text(hls(gap), encoding="utf-8")

    print(f"样例已生成于 {out}（seed={args.seed}）：")
    for name in ["hls.m3u8", "dash.mpd", "hls_dup.m3u8",
                 "hls_conflict.m3u8", "hls_gap.m3u8"]:
        print(f"  {name}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
