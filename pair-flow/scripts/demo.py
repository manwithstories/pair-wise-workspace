#!/usr/bin/env python3
"""独立演示脚本：自生成正常/截断/索引缺页/丢包样例并跑完整流水线。

与 CLI demo 的区别：本脚本把三编码与多种弱网参数集中演示，
逐场景打印关键指标，适合快速了解各模块协作，不写全局状态、可重复运行。

用法（项目根目录、激活 .venv 后）：
    .venv/bin/python scripts/demo.py
"""

from __future__ import annotations

import os
import sys
import tempfile

# 允许从 scripts/ 目录直接运行时定位到 src 布局
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))

from relay_media.bitstream import CODECS, scan_file  # noqa: E402
from relay_media.fixtures import (generate_audio, generate_video,  # noqa: E402
                                  truncate_file, write_index)
from relay_media.pipeline import Pipeline, PipelineConfig  # noqa: E402
from relay_media.report import build_report, format_summary, verify_report  # noqa: E402


def run_scenario(workdir: str, codec: str, duration: float,
                 loss_burst: float, truncate_at: int) -> dict:
    video = os.path.join(workdir, f"source.{codec}")
    audio = os.path.join(workdir, "source.wav")
    index = os.path.join(workdir, "index.json")

    generate_video(video, codec, duration_s=duration, fps=25, gop=10)
    generate_audio(audio, duration_s=duration,
                   silence_ranges_s=((1.0, 2.0), (duration - 1.0, duration)))
    full_bytes = os.path.getsize(video)
    trunc = truncate_at if truncate_at > 0 else None
    if trunc is not None:
        truncate_file(video, trunc)

    scan = scan_file(video, codec, stream_truncated=trunc is not None)
    # 缺页：只写首个关键帧，剩余交给扫描补建
    write_index(index, codec, scan.keyframe_offsets()[:1])

    cfg = PipelineConfig(
        codec=codec, workdir=os.path.join(workdir, "pipeline"),
        loss_burst=loss_burst, truncate_at=trunc,
        transport_bytes=full_bytes)
    result = Pipeline(cfg).run(video, audio, index)
    return build_report(result, source_index_path=index)


def main() -> int:
    scenarios = [
        # codec, duration, loss_burst, truncate_at
        ("h264", 12.0, 0.08, 4096),
        ("h265", 8.0, 0.12, 2048),
        ("av1", 8.0, 0.05, 0),
    ]
    with tempfile.TemporaryDirectory(prefix="relay-demo-") as tmp:
        for idx, (codec, duration, loss, trunc) in enumerate(scenarios):
            workdir = os.path.join(tmp, f"case{idx}")
            os.makedirs(workdir, exist_ok=True)
            report = run_scenario(workdir, codec, duration, loss, trunc)
            verdict = verify_report(report)
            print(f"\n### 场景 {idx + 1}: codec={codec} "
                  f"duration={duration}s loss={loss} trunc={trunc or 'off'}")
            # 仅打印摘要主体，避免三场景连刷完整分隔线
            for line in format_summary(report, verdict).splitlines()[2:-1]:
                print(line)
            print(f"  -> 验证{'通过' if verdict.ok else '未通过'}")
    print("\n演示完成：全部素材均在临时目录自生成并已清理。")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
