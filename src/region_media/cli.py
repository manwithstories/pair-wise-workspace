"""命令行入口：generate / process / verify。

退出码约定：
    0 验收通过（指标达标且无处理失败）
    1 处理或指标失败
    2 参数或输入无效
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from dataclasses import dataclass

from .fixture_writer import generate_all
from .generator import SCENARIOS, build_plan
from .manifest import ManifestError
from .metrics import TARGETS, build_summary
from .model import ErrorKind
from .pipeline import Pipeline, PipelineConfig
from .sync import SyncReport

EXIT_OK = 0
EXIT_PROCESS_FAILED = 1
EXIT_INVALID_INPUT = 2


@dataclass
class VerifyResult:
    scenario: str
    summary: dict
    accepted: bool


def _cmd_generate(args) -> int:
    out = os.path.abspath(args.out)
    metas = generate_all(out, seed=args.seed)
    print(f"已生成 {len(metas)} 个场景到 {out}")
    for name in SCENARIOS:
        m = metas[name]
        print(f"  - {name:10s} 分片={m['written']} "
              f"vfr={m['vfr']} 截断={m['truncated']} "
              f"损坏片={m['corrupt_seg']}")
    print(f"固定入口清单: {os.path.join(out, 'master.m3u8')}")
    return EXIT_OK


def _cmd_process(args) -> int:
    if not os.path.isfile(args.manifest):
        print(f"错误: 清单不存在: {args.manifest}", file=sys.stderr)
        return EXIT_INVALID_INPUT
    if args.chunk_size <= 0:
        print("错误: --chunk-size 必须为正整数", file=sys.stderr)
        return EXIT_INVALID_INPUT
    config = PipelineConfig(
        manifest_path=os.path.abspath(args.manifest),
        index_path=os.path.abspath(args.index),
        chunk_size=args.chunk_size,
        resume=args.resume,
        scenario=args.scenario,
        seed=args.seed)
    try:
        summary = _run_with_summary(config)
    except ManifestError as exc:
        print(f"输入无效: {exc}", file=sys.stderr)
        return EXIT_INVALID_INPUT
    except Exception as exc:  # 处理失败：保留最后有效检查点
        print(f"处理失败: {type(exc).__name__}: {exc}", file=sys.stderr)
        return EXIT_PROCESS_FAILED
    if args.json:
        print(json.dumps(summary, ensure_ascii=False, indent=2))
    else:
        _print_human(summary)
    return EXIT_OK if all(summary["targets_met"].values()) \
        else EXIT_PROCESS_FAILED


def _run_with_summary(config: PipelineConfig) -> dict:
    pipe = Pipeline(config)
    pipe.open()
    try:
        return pipe.run()
    finally:
        pipe.close()


def _cmd_verify(args) -> dict | int:
    fixtures = os.path.abspath(args.fixtures)
    if not os.path.isdir(fixtures):
        print(f"错误: 夹具目录不存在: {fixtures}（先运行 generate）",
              file=sys.stderr)
        return EXIT_INVALID_INPUT
    results: list[VerifyResult] = []
    # 每个场景独立索引，避免相互污染；normal 同时承载验收固定入口
    for scenario in SCENARIOS:
        manifest = os.path.join(fixtures, scenario, "master.m3u8")
        index = os.path.join(fixtures, f".verify-{scenario}.sqlite")
        for stale in (index, index + "-wal", index + "-shm"):
            if os.path.exists(stale):
                os.remove(stale)
        config = PipelineConfig(
            manifest_path=manifest, index_path=index,
            chunk_size=16, resume=True, scenario=scenario)
        try:
            summary = _run_with_summary(config)
        except ManifestError as exc:
            print(f"[{scenario}] 输入无效: {exc}", file=sys.stderr)
            return EXIT_INVALID_INPUT
        accepted = all(summary["targets_met"].values())
        results.append(VerifyResult(scenario, summary, accepted))
    report = _build_verify_report(results)
    if args.json:
        print(json.dumps(report, ensure_ascii=False, indent=2))
    else:
        _print_verify_human(report)
    return EXIT_OK if report["accepted"] else EXIT_PROCESS_FAILED


def _build_verify_report(results: list[VerifyResult]) -> dict:
    normal = next(r.summary for r in results if r.scenario == "normal")
    corrupt = next(r for r in results if r.scenario == "corrupt")
    aggregate = {
        "suite": "generated",
        "scenarios": len(results),
        "accepted": all(r.accepted for r in results),
        # 验收要求的固定字段取自 normal 主流程
        "sync_error_ms_p95": normal["sync_error_ms_p95"],
        "packet_recovery_rate": normal["packet_recovery_rate"],
        "latency_ms_p95": normal["latency_ms_p95"],
        "wrap_events": sum(r.summary["wrap_events"] for r in results),
        "rejected_segments":
            sum(r.summary["rejected_segments"] for r in results),
        "checkpoint_resumed": normal["checkpoint_resumed"],
        "targets": TARGETS,
        "targets_met": normal["targets_met"],
        "per_scenario": {r.scenario: r.summary for r in results},
    }
    # 损坏样例必须有拒绝数
    aggregate["corrupt_rejected_segments"] = \
        corrupt.summary["rejected_segments"]
    return aggregate


def _print_human(s: dict) -> None:
    print("处理摘要:")
    print(f"  同步 P95 偏差 : {s['sync_error_ms_p95']} ms "
          f"(目标 ≤ {TARGETS['sync_error_ms_p95']})")
    print(f"  丢包恢复率    : {s['packet_recovery_rate']} "
          f"(目标 ≥ {TARGETS['packet_recovery_rate']})")
    print(f"  P95 处理延迟  : {s['latency_ms_p95']} ms "
          f"(目标 ≤ {TARGETS['latency_ms_p95']})")
    print(f"  回绕/突变     : {s['wrap_events']} / {s['jump_events']}")
    print(f"  拒绝分片      : {s['rejected_segments']}")
    print(f"  检查点续跑    : {s['checkpoint_resumed']}")


def _print_verify_human(report: dict) -> None:
    print(f"验收套件 {report['suite']}: "
          f"{'通过' if report['accepted'] else '未通过'}")
    for name, s in report["per_scenario"].items():
        print(f"  {name:10s} syncP95={s['sync_error_ms_p95']:6.2f}ms "
              f"recovery={s['packet_recovery_rate']:5.2f} "
              f"latP95={s['latency_ms_p95']:7.2f}ms "
              f"rejected={s['rejected_segments']}")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="region_media",
        description="跨节点 HLS/DASH 传输媒体质量核验工具")
    sub = parser.add_subparsers(dest="command", required=True)

    g = sub.add_parser("generate", help="生成本地样例夹具")
    g.add_argument("--out", required=True, help="输出目录")
    g.add_argument("--seed", type=int, default=13812)
    g.set_defaults(func=_cmd_generate)

    p = sub.add_parser("process", help="处理清单并输出指标摘要")
    p.add_argument("--manifest", required=True)
    p.add_argument("--index", required=True, help="SQLite 检查点路径")
    p.add_argument("--chunk-size", type=int, default=16)
    p.add_argument("--resume", action="store_true",
                   help="从最后有效检查点续跑")
    p.add_argument("--scenario", default="normal", choices=SCENARIOS)
    p.add_argument("--seed", type=int, default=13812)
    p.add_argument("--json", action="store_true")
    p.set_defaults(func=_cmd_process)

    v = sub.add_parser("verify", help="运行生成夹具验收套件")
    v.add_argument("--suite", default="generated", choices=["generated"])
    v.add_argument("--fixtures", default=".tmp-fixtures")
    v.add_argument("--json", action="store_true")
    v.set_defaults(func=_cmd_verify)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        result = args.func(args)
    except ManifestError as exc:
        print(f"输入无效: {exc}", file=sys.stderr)
        return EXIT_INVALID_INPUT
    return result if isinstance(result, int) else EXIT_OK


if __name__ == "__main__":
    raise SystemExit(main())
