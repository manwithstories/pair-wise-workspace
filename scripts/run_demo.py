#!/usr/bin/env python3
"""端到端演示：生成轨迹并运行完整 ABR 模拟，打印验收字段。

示例：
    python scripts/run_demo.py --duration 120 --segment-duration 2 \
        --seed 42 --bitrates 300,800,1500,2500,4000,6000

输出包含 segments / selected_bitrates / switch_count / rebuffer_seconds /
avg_buffer_seconds / downshift_count / upshift_count / estimator_samples。
退出码：成功 0，参数/轨迹/模拟错误 2，断言失败 1。
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

# 允许从仓库根直接运行脚本，无需安装包或设置 PYTHONPATH
sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "src"))

from abr.runner import RunSettings, run_simulation  # noqa: E402
from abr.types import ABRError, TracePattern, validate_bitrates  # noqa: E402


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="ABR 自适应码率决策引擎完整演示"
    )
    parser.add_argument("--duration", type=float, default=120.0,
                        help="模拟媒体时长（秒），默认 120")
    parser.add_argument("--segment-duration", type=float, default=2.0,
                        help="分段时长（秒），默认 2")
    parser.add_argument("--seed", type=int, default=42, help="随机种子")
    parser.add_argument(
        "--bitrates",
        type=lambda s: tuple(float(x) for x in s.split(",")),
        default=(300.0, 800.0, 1500.0, 2500.0, 4000.0, 6000.0),
        help="逗号分隔的码率档位（kbps），需严格递增",
    )
    parser.add_argument("--pattern",
                        type=TracePattern, default=TracePattern.MIXED,
                        choices=list(TracePattern),
                        help="网络轨迹模式，默认 mixed")
    parser.add_argument("--json", action="store_true",
                        help="以 JSON 输出完整指标")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    try:
        # 严格递增与有限值校验（非法时抛 ConfigError，退出码 2）
        levels = tuple(validate_bitrates(list(args.bitrates)))
        settings = RunSettings(
            duration_s=args.duration,
            segment_duration_s=args.segment_duration,
            seed=args.seed,
            bitrates=tuple(args.bitrates),
            pattern=args.pattern,
        )
        _, metrics = run_simulation(settings, levels)
        data = metrics.to_dict()

        if args.json:
            print(json.dumps(data, ensure_ascii=False, indent=2))
            return 0

        print("=" * 60)
        print(" ABR 自适应码率决策引擎 — 完整演示")
        print(f" 时长={args.duration}s  分段={args.segment_duration}s  "
              f"种子={args.seed}  模式={args.pattern.value}")
        print("=" * 60)
        # 验收要求的字段（命名与命令契约一致）
        print(f"segments={data['segments']}")
        print(f"selected_bitrates={data['selected_bitrates']}")
        print(f"switch_count={data['switch_count']}")
        print(f"rebuffer_seconds={data['rebuffer_seconds']}")
        print(f"avg_buffer_seconds={data['avg_buffer_seconds']}")
        print(f"downshift_count={data['downshift_count']} "
              f"(forced={data['forced_downshift_count']})")
        print(f"upshift_count={data['upshift_count']} "
              f"(opportunistic={data['opportunistic_upshift_count']})")
        print(f"protection_seconds={data['protection_seconds']}")
        print(f"average_bitrate_kbps={data['average_bitrate_kbps']}")
        print(f"risk_reject_count={data['risk_reject_count']}")
        print(f"estimator_samples={data['estimator_samples']}")
        print("=" * 60)
        return 0
    except ABRError as exc:
        print(f"错误: {exc}", file=sys.stderr)
        return 2
    except AssertionError as exc:
        print(f"断言失败: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
