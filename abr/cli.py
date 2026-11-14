"""命令行入口：gen-trace / simulate / sweep / report。

退出码约定：0 成功；2 参数错误或未知估计器；3 轨迹校验失败；4 无可用分片。
"""

from __future__ import annotations

import argparse
import csv
import sys
from pathlib import Path

from .buffer import BufferLedger
from .decider import Decider, DeciderConfig
from .estimator import ESTIMATOR_REGISTRY, make_estimator
from .ladder import Ladder, LadderValidationError
from .metrics import aggregate
from .report import format_summary, format_table, metrics_to_json
from .simulator import NoUsableSegmentError, simulate
from .trace import REGIMES, TraceValidationError, generate_samples, read_csv, write_csv

EXIT_OK = 0
EXIT_USAGE = 2
EXIT_TRACE_INVALID = 3
EXIT_NO_SEGMENT = 4

SWEEP_UP_FACTORS = (1.10, 1.15, 1.25)
SWEEP_PANIC_BUFFERS = (4.0, 5.0, 6.0)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="abr", description="ABR 离线回放模拟器")
    sub = parser.add_subparsers(dest="command", required=True)

    p_gen = sub.add_parser("gen-trace", help="生成带状态迁移的网络轨迹 CSV")
    p_gen.add_argument("--out", required=True, help="输出 CSV 路径")
    p_gen.add_argument("--seed", type=int, required=True, help="随机种子（同 seed 逐字节复现）")
    p_gen.add_argument("--duration", type=int, default=600, help="时长（秒）")
    p_gen.add_argument("--regimes", default="wifi,4g,3g",
                       help=f"逗号分隔的状态列表，可选: {','.join(REGIMES)}")
    p_gen.add_argument("--outages", type=int, default=0, help="短时中断次数")
    p_gen.set_defaults(func=_cmd_gen_trace)

    p_sim = sub.add_parser("simulate", help="单条轨迹离线回放")
    p_sim.add_argument("--trace", required=True, help="轨迹 CSV 路径")
    p_sim.add_argument("--estimator", required=True, choices=sorted(ESTIMATOR_REGISTRY))
    p_sim.add_argument("--ladder", required=True, help="码率阶梯 JSON 路径")
    p_sim.add_argument("--chunk-sec", type=float, default=4.0, help="分片时长（秒）")
    p_sim.add_argument("--buffer-cap", type=float, default=30.0, help="缓冲上限（秒）")
    p_sim.add_argument("--print-json", action="store_true", help="以 JSON 打印结果")
    p_sim.set_defaults(func=_cmd_simulate)

    p_sweep = sub.add_parser("sweep", help="多轨迹 x 多估计器 x 阈值网格批量回放")
    p_sweep.add_argument("--traces", required=True, help="轨迹目录（扫描 *.csv）")
    p_sweep.add_argument("--estimators", required=True, help="逗号分隔的估计器列表")
    p_sweep.add_argument("--ladder", default="ladders/main.json")
    p_sweep.add_argument("--chunk-sec", type=float, default=4.0)
    p_sweep.add_argument("--buffer-cap", type=float, default=30.0)
    p_sweep.add_argument("--out", required=True, help="结果 CSV 输出路径")
    p_sweep.set_defaults(func=_cmd_sweep)

    p_rep = sub.add_parser("report", help="把 sweep 结果 CSV 渲染为终端表格")
    p_rep.add_argument("--input", required=True, help="sweep 输出 CSV 路径")
    p_rep.add_argument("--top", type=int, default=20, help="只显示前 N 行")
    p_rep.set_defaults(func=_cmd_report)
    return parser


def _cmd_gen_trace(args: argparse.Namespace) -> int:
    regimes = [r.strip() for r in args.regimes.split(",") if r.strip()]
    try:
        samples = generate_samples(seed=args.seed, duration_s=args.duration,
                                   regimes=regimes, outages=args.outages)
    except ValueError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_USAGE
    write_csv(args.out, samples)
    print(f"trace written: {args.out} ({len(samples)}s, seed={args.seed})")
    return EXIT_OK


def _run_one(trace_path, estimator_name, ladder, buffer_cap, config=None):
    samples = read_csv(trace_path)
    buffer = BufferLedger(cap_s=buffer_cap)
    records, startup_ms = simulate(
        samples=samples,
        estimator=make_estimator(estimator_name),
        decider=Decider(ladder, config),
        ladder=ladder,
        buffer=buffer,
    )
    trace_seconds = int(round(samples[-1].ts_ms / 1000.0)) + 1
    return aggregate(records, buffer, trace_seconds, startup_ms)


def _cmd_simulate(args: argparse.Namespace) -> int:
    try:
        ladder = Ladder.from_json(args.ladder, chunk_sec=args.chunk_sec)
    except LadderValidationError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_USAGE
    try:
        metrics = _run_one(args.trace, args.estimator, ladder, args.buffer_cap)
    except TraceValidationError as exc:
        print(f"trace validation failed: {exc}", file=sys.stderr)
        return EXIT_TRACE_INVALID
    except NoUsableSegmentError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_NO_SEGMENT
    if args.print_json:
        print(metrics_to_json(metrics))
    else:
        print(format_summary(metrics))
    return EXIT_OK


def _cmd_sweep(args: argparse.Namespace) -> int:
    estimator_names = [e.strip() for e in args.estimators.split(",") if e.strip()]
    unknown = [e for e in estimator_names if e not in ESTIMATOR_REGISTRY]
    if unknown:
        print(f"error: unknown estimator(s): {','.join(unknown)}", file=sys.stderr)
        return EXIT_USAGE
    trace_dir = Path(args.traces)
    trace_files = sorted(trace_dir.glob("*.csv"))
    if not trace_files:
        print(f"error: no trace CSV found in {trace_dir}", file=sys.stderr)
        return EXIT_USAGE
    try:
        ladder = Ladder.from_json(args.ladder, chunk_sec=args.chunk_sec)
    except LadderValidationError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_USAGE

    rows: list[dict] = []
    for trace_path in trace_files:
        for name in estimator_names:
            for up_factor in SWEEP_UP_FACTORS:
                for panic in SWEEP_PANIC_BUFFERS:
                    config = DeciderConfig(up_factor=up_factor, panic_buffer_s=panic)
                    try:
                        m = _run_one(str(trace_path), name, ladder, args.buffer_cap, config)
                    except (TraceValidationError, NoUsableSegmentError) as exc:
                        print(f"skip {trace_path.name}/{name}: {exc}", file=sys.stderr)
                        continue
                    rows.append({
                        "trace": trace_path.name,
                        "estimator": name,
                        "up_factor": up_factor,
                        "panic_buffer_s": panic,
                        "qoe": m.qoe,
                        "avg_bitrate_kbps": m.avg_bitrate_kbps,
                        "switch_count": m.switch_count,
                        "rebuffer_total_ms": m.rebuffer_total_ms,
                        "rebuffer_events": m.rebuffer_events,
                        "startup_ms": m.startup_ms,
                    })
    if not rows:
        print("error: no usable simulation result", file=sys.stderr)
        return EXIT_NO_SEGMENT
    rows.sort(key=lambda r: r["qoe"], reverse=True)

    out_path = Path(args.out)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    with out_path.open("w", newline="", encoding="utf-8") as fh:
        writer = csv.DictWriter(fh, fieldnames=list(rows[0].keys()))
        writer.writeheader()
        writer.writerows(rows)
    print(f"sweep written: {out_path} ({len(rows)} rows)")
    print(format_table(rows[:10], list(rows[0].keys())))
    return EXIT_OK


def _cmd_report(args: argparse.Namespace) -> int:
    path = Path(args.input)
    if not path.exists():
        print(f"error: file not found: {path}", file=sys.stderr)
        return EXIT_USAGE
    with path.open(newline="", encoding="utf-8") as fh:
        rows = list(csv.DictReader(fh))
    if not rows:
        print("error: empty report file", file=sys.stderr)
        return EXIT_USAGE
    rows.sort(key=lambda r: float(r.get("qoe", 0.0)), reverse=True)
    print(format_table(rows[:args.top], list(rows[0].keys())))
    return EXIT_OK


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
