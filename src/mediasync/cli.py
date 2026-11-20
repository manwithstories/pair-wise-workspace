"""终端命令适配器：demo 验收命令与退出码映射。

退出码约定：0 成功；2 参数错误；3 输入损坏或无法对齐；4 检查点校验失败。
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

from .errors import ErrorCode
from .pipeline import CancellationToken, JobConfig, Pipeline
from .workspace import generate_workspace


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="mediasync",
        description="离线媒体预处理内核：裸码流扫描、对齐归一与可恢复分片清单",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    demo = sub.add_parser("demo", help="生成夹具并跑完整可恢复流水线")
    demo.add_argument("--work", required=True, help="工作目录（仅允许读写该目录）")
    demo.add_argument("--corrupt-every", type=int, default=0,
                      help="每 N 个单元注入一次头部损坏（0 关闭）")
    demo.add_argument("--interrupt-after", type=int, default=0,
                      help="提交 N 个块后注入写入中断（0 关闭）")
    demo.add_argument("--resume", action="store_true", help="中断后自动续跑")
    demo.add_argument("--representations", type=int, default=3)
    demo.add_argument("--segment-ms", type=int, default=500)
    demo.add_argument("--block-records", type=int, default=2)
    demo.add_argument("--max-deviation-ms", type=int, default=25)
    return parser


def _validate(args: argparse.Namespace) -> str | None:
    if args.representations < 1 or args.representations > 3:
        return "representations 取值范围为 1..3"
    if args.segment_ms <= 0:
        return "segment-ms 必须为正整数"
    if args.corrupt_every < 0 or args.interrupt_after < 0:
        return "corrupt-every 与 interrupt-after 不可为负"
    if args.max_deviation_ms < 0:
        return "max-deviation-ms 不可为负"
    work = Path(args.work)
    # 仅拦截显式向上逃逸（..）；绝对工作目录本身允许，后续 IO 仍受安全边界约束。
    if ".." in Path(work).parts:
        return f"工作目录 {args.work} 含向上跳转，拒绝路径越界"
    return None


def run_demo(args: argparse.Namespace) -> int:
    problem = _validate(args)
    if problem:
        print(f"参数错误: {problem}")
        return 2

    workdir = str(Path(args.work).resolve())
    Path(workdir).mkdir(parents=True, exist_ok=True)
    material = generate_workspace(
        workdir,
        args.representations,
        corrupt_every=args.corrupt_every,
    )

    base_cfg = JobConfig(
        workdir=workdir,
        representations=args.representations,
        segment_ms=args.segment_ms,
        max_deviation_ms=args.max_deviation_ms,
        block_records=args.block_records,
        interrupt_after=args.interrupt_after,
        corrupt_offset=material.detected_offset_bytes,
    )

    first = Pipeline(base_cfg).run(CancellationToken())
    if args.interrupt_after and not first.cancelled:
        # 中断注入在块提交阶段触发；未触发说明块数不足，属参数问题。
        print("参数错误: interrupt-after 超过可用块数，无法注入中断")
        return 2
    if first.cancelled and not args.resume:
        print("已按设定中断（未指定 --resume）。")
        return 0

    if args.resume and first.cancelled:
        resume_cfg = JobConfig(**{**base_cfg.__dict__, "interrupt_after": 0,
                                  "resume": True})
        result = Pipeline(resume_cfg).run(CancellationToken())
    else:
        result = first

    if result.exit_code != 0:
        return _emit_failure(result)
    _emit_summary(result, material)
    return 0


def _emit_summary(result, material) -> None:
    detected = material.detected_offset_bytes
    injected = material.corrupt_offset_bytes
    print("=== mediasync demo 摘要 ===")
    print(f"corrupt_offset_bytes: {detected if detected is not None else 'none'}")
    if injected is not None and detected is not None:
        print(f"corrupt_locate_error_bytes: {abs(detected - injected)}")
    print(f"resume_ms: {result.resume_ms}")
    print(f"segments: {result.segments}")
    print(f"aligned_representations: {result.aligned_representations}")
    print(f"manifest_sha256: {result.manifest_sha256}")
    print(f"index_sha256: {result.index_sha256}")
    if result.quarantined:
        print(f"quarantine: {result.quarantined}")
    if result.errors.summary():
        print(f"error_summary: {result.errors.summary()}")


def _emit_failure(result) -> int:
    code = result.exit_code
    label = {3: "输入损坏或无法对齐", 4: "检查点校验失败"}.get(code, "失败")
    print(f"{label}（退出码 {code}）")
    for err in result.errors.fatal():
        print(f"  - [{err.code}] {err.message} (offset={err.offset})")
    return code


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    if args.command == "demo":
        return run_demo(args)
    parser.error("未知命令")
    return 2


if __name__ == "__main__":
    sys.exit(main())
