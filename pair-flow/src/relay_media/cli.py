"""命令行入口：demo（自生成素材并跑全链路）与 verify（复核报告）。

退出码约定：
  0 验证通过；2 输入或参数错误；3 存在不可恢复损坏；4 指标未达标。
优先级：参数错误(2) > 不可恢复损坏(3) > 指标未达标(4) > 通过(0)。
"""

from __future__ import annotations

import argparse
import json
import os
import sys

from . import report as report_mod
from .fixtures import (generate_audio, generate_video, truncate_file,
                       write_index)
from .bitstream import CODECS, scan_file
from .keyframe_index import IndexDocument
from .pipeline import Pipeline, PipelineConfig
from .timeline import ErrorKind

EXIT_OK = 0
EXIT_USAGE = 2
EXIT_UNRECOVERABLE = 3
EXIT_METRIC = 4


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="relay-media",
        description="边缘媒体片段可靠转发验证工具链（纯本地、终端形态）")
    sub = parser.add_subparsers(dest="command", required=True)

    demo = sub.add_parser("demo", help="自生成素材并运行完整流水线")
    demo.add_argument("--codec", choices=CODECS, default="h264")
    demo.add_argument("--duration", type=float, default=12.0,
                      help="生成素材时长（秒）")
    demo.add_argument("--loss-burst", type=float, default=0.08,
                      help="突发丢包比例，取值 [0,1]")
    demo.add_argument("--truncate-at", type=int, default=4096,
                      help="在该字节处截断视频码流，<=0 表示不截断")
    demo.add_argument("--output", required=True, help="报告 JSON 输出路径")
    demo.add_argument("--workdir", default=None, help="检查点工作目录")
    demo.add_argument("--seed", type=int, default=12789)
    demo.add_argument("--resume", action="store_true",
                      help="从检查点阶段边界恢复")

    verify = sub.add_parser("verify", help="复核已生成的报告与校验值")
    verify.add_argument("report", help="demo 产出的报告 JSON 路径")
    return parser


def _validate_demo_args(args) -> str | None:
    if args.duration <= 0:
        return "--duration 必须为正数"
    if not 0.0 <= args.loss_burst <= 1.0:
        return "--loss-burst 必须落在 [0,1]"
    output_dir = os.path.dirname(os.path.abspath(args.output))
    if not os.path.isdir(output_dir):
        return f"输出目录不存在：{output_dir}"
    return None


def _run_demo(args) -> int:
    problem = _validate_demo_args(args)
    if problem:
        print(f"参数错误：{problem}", file=sys.stderr)
        return EXIT_USAGE

    workdir = args.workdir or os.path.join(
        os.path.dirname(os.path.abspath(args.output)), ".relay-demo-work")
    os.makedirs(workdir, exist_ok=True)
    video_path = os.path.join(workdir, f"source.{args.codec}")
    audio_path = os.path.join(workdir, "source.wav")
    index_path = os.path.join(workdir, "index.json")

    generate_video(video_path, args.codec, duration_s=args.duration,
                   fps=25, gop=10, seed=args.seed)
    generate_audio(audio_path, duration_s=args.duration, seed=args.seed)

    # 完整流规模用于传输评估；截断只影响码流解析与索引侧。
    full_stream_bytes = os.path.getsize(video_path)
    truncate_at = args.truncate_at if args.truncate_at > 0 else None
    if truncate_at is not None:
        truncate_file(video_path, truncate_at)

    # 缺页索引：只保留首个关键帧，其余交给流水线扫描补建
    scan = scan_file(video_path, args.codec,
                     stream_truncated=truncate_at is not None)
    keyframes = scan.keyframe_offsets()
    write_index(index_path, args.codec, keyframes[:1])

    cfg = PipelineConfig(
        codec=args.codec, workdir=workdir,
        loss_burst=args.loss_burst, truncate_at=truncate_at,
        transport_bytes=full_stream_bytes,
        seed=args.seed)
    result = Pipeline(cfg).run(video_path, audio_path, index_path,
                               resume=args.resume)
    report = report_mod.build_report(result, source_index_path=index_path)
    report_mod.save_report(report, args.output)

    verdict = report_mod.verify_report(report)
    print(report_mod.format_summary(report, verdict))
    print(f"报告已写入：{args.output}")
    return _exit_from_verdict(report, verdict)


def _exit_from_verdict(report: dict,
                       verdict: report_mod.VerificationVerdict) -> int:
    if not verdict.checksum_ok:
        return EXIT_UNRECOVERABLE
    if report_mod.has_unrecoverable(report):
        return EXIT_UNRECOVERABLE
    if not verdict.metric_ok:
        return EXIT_METRIC
    return EXIT_OK


def _run_verify(args) -> int:
    if not os.path.exists(args.report):
        print(f"参数错误：报告文件不存在：{args.report}", file=sys.stderr)
        return EXIT_USAGE
    try:
        report = report_mod.load_report(args.report)
    except (json.JSONDecodeError, KeyError) as exc:
        print(f"参数错误：报告无法解析：{exc}", file=sys.stderr)
        return EXIT_USAGE

    verdict = report_mod.verify_report(report)
    print(report_mod.format_summary(report, verdict))
    return _exit_from_verdict(report, verdict)


def main(argv: list[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "demo":
            return _run_demo(args)
        if args.command == "verify":
            return _run_verify(args)
    except (ValueError, FileNotFoundError) as exc:
        print(f"输入错误：{exc}", file=sys.stderr)
        return EXIT_USAGE
    parser.error(f"未知命令：{args.command}")
    return EXIT_USAGE


if __name__ == "__main__":
    raise SystemExit(main())
