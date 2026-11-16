"""命令行界面：trace / simulate / compare 子命令。

退出码约定：
    0  成功
    1  断言/内部不变量失败
    2  参数错误、轨迹非法或模拟失败（所有 ABRError 子类）
纯终端输出，支持人类可读表格（默认）与 --json 机器格式。
"""

from __future__ import annotations

import argparse
import json
import sys

from .metrics import SimulationMetrics
from .policy import PolicyConfig
from .runner import (
    DEFAULT_BITRATES,
    RunSettings,
    build_trace,
    run_simulation,
)
from .trace import TraceConfig, TraceGenerator
from .types import ABRError, ConfigError, TracePattern, validate_bitrates


# ---------------------------------------------------------------------------
# 参数解析辅助
# ---------------------------------------------------------------------------
def _parse_bitrates(text: str) -> tuple[float, ...]:
    try:
        values = tuple(float(x) for x in text.split(","))
    except ValueError as exc:
        raise ConfigError(
            f"码率必须为逗号分隔的数值，得到 {text!r}"
        ) from exc
    # 严格递增校验委托给统一入口，非法时抛 ConfigError
    validate_bitrates(list(values))
    return values


def _parse_pattern(text: str) -> TracePattern:
    try:
        return TracePattern(text)
    except ValueError as exc:
        valid = ", ".join(p.value for p in TracePattern)
        raise ConfigError(
            f"未知轨迹模式 {text!r}，可选: {valid}"
        ) from exc


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="abr",
        description="自适应码率（ABR）决策引擎：轨迹生成、模拟与策略对比",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    # trace -----------------------------------------------------------------
    p_trace = sub.add_parser("trace", help="生成并查看网络轨迹")
    p_trace.add_argument("--duration", type=float, default=60.0)
    p_trace.add_argument("--seed", type=int, default=42)
    p_trace.add_argument("--pattern", type=_parse_pattern,
                         default=TracePattern.MIXED)
    p_trace.add_argument("--base-bandwidth", type=float, default=4000.0)
    p_trace.add_argument("--min-bandwidth", type=float, default=600.0)
    p_trace.add_argument("--json", action="store_true", help="输出 JSON")

    # simulate --------------------------------------------------------------
    p_sim = sub.add_parser("simulate", help="运行一次 ABR 模拟")
    _add_common_sim_args(p_sim)

    # compare ---------------------------------------------------------------
    p_cmp = sub.add_parser("compare", help="在同一轨迹上对比多个策略")
    _add_common_sim_args(p_cmp)
    p_cmp.add_argument(
        "--configs", type=str,
        default="balanced:confirm=2,cooldown=1,margin=1.25;"
                "conservative:confirm=4,cooldown=3,margin=1.5;"
                "aggressive:confirm=1,cooldown=0,margin=1.1",
        help="分号分隔的策略:键=值,... 列表",
    )
    return parser


def _add_common_sim_args(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--duration", type=float, default=120.0)
    parser.add_argument("--segment-duration", type=float, default=2.0)
    parser.add_argument("--seed", type=int, default=42)
    parser.add_argument("--bitrates", type=_parse_bitrates,
                        default=DEFAULT_BITRATES)
    parser.add_argument("--pattern", type=_parse_pattern,
                        default=TracePattern.MIXED)
    parser.add_argument("--base-bandwidth", type=float, default=4000.0)
    parser.add_argument("--min-bandwidth", type=float, default=600.0)
    parser.add_argument("--json", action="store_true", help="输出 JSON")


# ---------------------------------------------------------------------------
# 子命令实现
# ---------------------------------------------------------------------------
def _cmd_trace(args: argparse.Namespace) -> int:
    config = TraceConfig(
        duration_s=args.duration,
        seed=args.seed,
        pattern=args.pattern,
        base_bandwidth_kbps=args.base_bandwidth,
        min_bandwidth_kbps=args.min_bandwidth,
    )
    trace = TraceGenerator(config).generate()
    samples = trace.samples
    if args.json:
        payload = {
            "pattern": trace.pattern.value,
            "seed": trace.seed,
            "duration_s": trace.duration_s,
            "sample_count": len(samples),
            "samples": [
                {"time_s": s.time_s, "bandwidth_kbps": s.bandwidth_kbps,
                 "latency_ms": s.latency_ms, "loss_rate": s.loss_rate}
                for s in samples
            ],
        }
        print(json.dumps(payload, ensure_ascii=False, indent=2))
        return 0
    bws = [s.bandwidth_kbps for s in samples]
    print(f"轨迹模式: {trace.pattern.value}  种子: {trace.seed}")
    print(f"时长: {trace.duration_s:.1f}s  采样数: {len(samples)}")
    print(f"带宽 min/mean/max: "
          f"{min(bws):.0f} / {sum(bws)/len(bws):.0f} / {max(bws):.0f} kbps")
    print(f"零带宽采样点: {sum(1 for b in bws if b == 0.0)}")
    return 0


def _make_settings(args: argparse.Namespace) -> RunSettings:
    return RunSettings(
        duration_s=args.duration,
        segment_duration_s=args.segment_duration,
        seed=args.seed,
        bitrates=tuple(args.bitrates),
        pattern=args.pattern,
        base_bandwidth_kbps=args.base_bandwidth,
        min_bandwidth_kbps=args.min_bandwidth,
    )


def _cmd_simulate(args: argparse.Namespace) -> int:
    settings = _make_settings(args)
    levels = tuple(validate_bitrates(list(settings.bitrates)))
    _, metrics = run_simulation(settings, levels)
    if args.json:
        print(json.dumps(metrics.to_dict(), ensure_ascii=False, indent=2))
    else:
        _print_metrics_table(metrics)
    return 0


def _cmd_compare(args: argparse.Namespace) -> int:
    from .buffer import BufferConfig
    from .controller import SimulationConfig, SimulationController

    settings = _make_settings(args)
    levels = tuple(validate_bitrates(list(settings.bitrates)))
    trace = build_trace(settings)
    rows: list[tuple[str, SimulationMetrics]] = []
    for name, overrides in _parse_configs(args.configs):
        policy = _build_policy(overrides)
        config = SimulationConfig(
            segment_duration_s=settings.segment_duration_s,
            start_level_index=0,
            buffer=BufferConfig(target_buffer_s=8.0,
                                underflow_threshold_s=3.0, max_buffer_s=20.0),
            policy=policy,
        )
        _, metrics = SimulationController(trace, levels, config).run(
            settings.n_segments
        )
        rows.append((name, metrics))

    if args.json:
        print(json.dumps({name: m.to_dict() for name, m in rows},
                         ensure_ascii=False, indent=2))
    else:
        _print_compare_table(rows)
    return 0


def _parse_configs(text: str):
    parsed = []
    for chunk in text.split(";"):
        chunk = chunk.strip()
        if not chunk:
            continue
        name, _, kv = chunk.partition(":")
        overrides: dict[str, float] = {}
        for pair in kv.split(","):
            pair = pair.strip()
            if not pair:
                continue
            key, _, value = pair.partition("=")
            overrides[key.strip()] = float(value)
        parsed.append((name.strip(), overrides))
    if not parsed:
        raise ABRError("compare 需要至少一个策略配置")
    return parsed


def _build_policy(overrides: dict[str, float]) -> PolicyConfig:
    return PolicyConfig(
        confirm_segments=int(overrides.get("confirm", 2)),
        cooldown_segments=int(overrides.get("cooldown", 1)),
        safety_margin=overrides.get("margin", 1.25),
    )


# ---------------------------------------------------------------------------
# 表格输出
# ---------------------------------------------------------------------------
def _print_metrics_table(m: SimulationMetrics) -> None:
    d = m.to_dict()
    print("=" * 52)
    print(" ABR 模拟结果")
    print("=" * 52)
    lines = [
        ("分段数 segments", d["segments"]),
        ("切换次数 switch_count", d["switch_count"]),
        ("  主动升档 upshift", d["upshift_count"]),
        ("  被迫降档 downshift(forced)",
         f"{d['downshift_count']} (forced={d['forced_downshift_count']})"),
        ("下溢时长 rebuffer_seconds", f"{d['rebuffer_seconds']:.2f} s"),
        ("保护时长 protection_seconds", f"{d['protection_seconds']:.2f} s"),
        ("平均缓冲 avg_buffer", f"{d['avg_buffer_seconds']:.2f} s"),
        ("平均码率 avg_bitrate", f"{d['average_bitrate_kbps']:.0f} kbps"),
        ("风险拒绝 risk_reject", d["risk_reject_count"]),
        ("估计样本 estimator_samples", d["estimator_samples"]),
    ]
    for label, value in lines:
        print(f"  {label:<32} {value}")
    print("-" * 52)
    print("  selected_bitrates =")
    print("  ", d["selected_bitrates"])


def _print_compare_table(rows: list[tuple[str, SimulationMetrics]]) -> None:
    header = f"{'策略':<14}{'切换':>5}{'升':>4}{'降':>4}{'下溢s':>8}{'均缓冲s':>9}{'均码率':>8}"
    print(header)
    print("-" * len(header))
    for name, m in rows:
        d = m.to_dict()
        print(f"{name:<14}{d['switch_count']:>5}{d['upshift_count']:>4}"
              f"{d['downshift_count']:>4}{d['rebuffer_seconds']:>8.2f}"
              f"{d['avg_buffer_seconds']:>9.2f}"
              f"{d['average_bitrate_kbps']:>8.0f}")


# ---------------------------------------------------------------------------
# 入口
# ---------------------------------------------------------------------------
def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    # 参数解析也纳入统一错误处理：非法码率/模式等抛 ConfigError -> 退出码 2
    try:
        args = parser.parse_args(argv)
        if args.command == "trace":
            return _cmd_trace(args)
        if args.command == "simulate":
            return _cmd_simulate(args)
        if args.command == "compare":
            return _cmd_compare(args)
        parser.error(f"未知子命令 {args.command!r}")
        return 2
    except ABRError as exc:
        print(f"错误: {exc}", file=sys.stderr)
        return 2
    except AssertionError as exc:  # 内部不变量失败
        print(f"断言失败: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
