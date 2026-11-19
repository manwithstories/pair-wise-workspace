"""命令行入口：demo / verify，统一退出码与终端/JSON 输出。

退出码：0 正常，1 内部错误，2 输入无效，3 素材不可恢复。
"""
from __future__ import annotations

import argparse
import hashlib
import json
import sys
from pathlib import Path

from . import report as report_mod
from .fixtures import generate_demo
from .model import (ErrorCategory, ExitCode, Manifest, PipelineError,
                    read_json)
from .pipeline import Pipeline
from .report import Metrics


def _canonical_hash(doc: dict) -> str:
    return hashlib.sha256(json.dumps(
        doc, sort_keys=True, ensure_ascii=False).encode("utf-8")).hexdigest()


def load_manifest(path: str) -> Manifest:
    p = Path(path).resolve()
    if not p.is_file():
        raise PipelineError(
            ErrorCategory.MANIFEST_MISSING_FIELD,
            f"清单文件不存在: {p}")
    try:
        doc = read_json(p)
    except json.JSONDecodeError as exc:
        raise PipelineError(
            ErrorCategory.MANIFEST_BAD_VALUE,
            f"清单 JSON 解析失败: {exc}") from exc
    root = str(p.parent)
    manifest = Manifest.from_dict(doc, root)
    manifest.source_hash = _canonical_hash(doc)
    return manifest


def cmd_demo(args: argparse.Namespace) -> ExitCode:
    out = str(Path(args.out).resolve())
    manifest = generate_demo(out, args.seed)
    n_video = sum(1 for t in manifest.tracks.values() if t.kind.value == "video")
    n_audio = len(manifest.tracks) - n_video
    print(f"demo 样例已生成: {out}")
    print(f"  项目: {manifest.project}")
    print(f"  视频轨 {n_video} 条（含突发丢包/截断/编码器重启回绕样例）")
    print(f"  音频轨 {n_audio} 条（含空音轨边界样例）")
    print(f"  清单: {Path(out) / 'manifest.json'}")
    print("下一步: "
          f".venv/bin/python -m fielddeliver verify --manifest {out}/manifest.json --json")
    return ExitCode.OK


def cmd_verify(args: argparse.Namespace) -> ExitCode:
    manifest = load_manifest(args.manifest)
    work_dir = args.work_dir or str(
        Path(manifest.root) / ".fielddeliver-work")
    pipeline = Pipeline(manifest, work_dir)
    report, exit_code = pipeline.run()
    metrics = Metrics(**report["metrics"])

    if args.json:
        print(report_mod.to_json_stdout(report, exit_code))
    else:
        print(report_mod.render_terminal(metrics, exit_code))
        print(f"\n完整报告: {Path(work_dir) / 'report.json'}")
    return exit_code


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="fielddeliver",
        description="音乐节多机位代理/索引/同步交付流水线")
    sub = parser.add_subparsers(dest="command", required=True)

    p_demo = sub.add_parser("demo", help="生成多机位成功/损坏/边界自测样例")
    p_demo.add_argument("--seed", type=int, default=37)
    p_demo.add_argument("--out", required=True)
    p_demo.set_defaults(func=cmd_demo)

    p_verify = sub.add_parser("verify", help="校验清单并运行交付流水线")
    p_verify.add_argument("--manifest", required=True)
    p_verify.add_argument("--json", action="store_true",
                          help="向 stdout 输出完整 JSON 报告")
    p_verify.add_argument("--work-dir", default=None)
    p_verify.set_defaults(func=cmd_verify)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        code = args.func(args)
        return int(code)
    except PipelineError as exc:
        print(f"[错误 {int(exc.exit_code)}] {exc.category.code}: "
              f"{exc.message}", file=sys.stderr)
        if exc.track and exc.track != "-":
            print(f"  轨道: {exc.track}"
                  + (f"  字节偏移: {exc.byte_offset}"
                     if exc.byte_offset is not None else ""),
                  file=sys.stderr)
        return int(exc.exit_code)
    except Exception as exc:  # noqa: BLE001 - 内部错误兜底
        print(f"[内部错误 1] {type(exc).__name__}: {exc}", file=sys.stderr)
        return int(ExitCode.INTERNAL)


if __name__ == "__main__":
    raise SystemExit(main())
