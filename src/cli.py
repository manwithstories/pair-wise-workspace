"""命令行入口：generate-fixture / reconcile / verify。

退出码约定：
  0 成功且无业务错误；2 存在业务校验错误；3 输入格式或参数错误；4 输出写入失败。
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path
from typing import Optional, Sequence

from .errors import ExitCode, FormatError
from .fixture import generate_fixture
from .import_decision import import_decision
from .import_manifest import import_manifest
from .reconcile import reconcile
from .report import (
    load_report_dict, render_terminal, render_verify, verify_report, write_json,
)
from .resolver import is_within


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="conform", description="影视套底清单生成与核验工具")
    sub = parser.add_subparsers(dest="command", required=True)

    gen = sub.add_parser("generate-fixture", help="在指定目录生成确定性样例数据")
    gen.add_argument("--dir", required=True, dest="dir")

    rec = sub.add_parser("reconcile", help="解析决策表与素材清单并生成套底报告")
    rec.add_argument("--decision", required=True)
    rec.add_argument("--manifest", required=True)
    rec.add_argument("--root", required=True, help="媒体根目录（沙箱，拒绝越界访问）")
    rec.add_argument("--json-out", required=True)
    rec.add_argument("--strict", action="store_true",
                     help="严格模式：任一业务错误阻止写出结果")

    ver = sub.add_parser("verify", help="复核已落盘的 JSON 报告")
    ver.add_argument("--report", required=True)
    return parser


def _cmd_generate(args: argparse.Namespace) -> int:
    try:
        decision, manifest, media_root = generate_fixture(Path(args.dir))
    except OSError as exc:
        print(f"fixture 写入失败: {exc}", file=sys.stderr)
        return ExitCode.WRITE_ERROR
    print(f"已生成 fixture: {decision}")
    print(f"                  {manifest}")
    print(f"                  媒体根目录 {media_root}")
    return ExitCode.OK


def _cmd_reconcile(args: argparse.Namespace) -> int:
    decision_path = Path(args.decision)
    manifest_path = Path(args.manifest)
    root_path = Path(args.root)
    out_path = Path(args.json_out)

    for p in (decision_path, manifest_path):
        if not p.is_file():
            print(f"输入文件不存在: {p}", file=sys.stderr)
            return ExitCode.FORMAT_ERROR
    if not root_path.is_dir():
        print(f"媒体根目录不存在或不是目录: {root_path}", file=sys.stderr)
        return ExitCode.FORMAT_ERROR
    if not is_within(root_path, root_path):
        print("媒体根目录解析异常", file=sys.stderr)
        return ExitCode.FORMAT_ERROR

    try:
        decision = import_decision(decision_path)
        manifest = import_manifest(manifest_path)
    except FormatError as exc:
        print(f"格式错误: {exc}", file=sys.stderr)
        return ExitCode.FORMAT_ERROR

    report = reconcile(manifest, decision, root_path)
    print(render_terminal(report))

    if args.strict and report.has_error:
        print("严格模式：存在业务错误，阻止写出结果", file=sys.stderr)
        return ExitCode.BUSINESS_ERROR
    try:
        write_json(report, out_path)
    except OSError as exc:
        print(f"报告写出失败: {exc}", file=sys.stderr)
        return ExitCode.WRITE_ERROR
    print(f"\nJSON 报告已写出: {out_path}")
    return ExitCode.BUSINESS_ERROR if report.has_error else ExitCode.OK


def _cmd_verify(args: argparse.Namespace) -> int:
    report_path = Path(args.report)
    if not report_path.is_file():
        print(f"报告文件不存在: {report_path}", file=sys.stderr)
        return ExitCode.FORMAT_ERROR
    try:
        data = load_report_dict(report_path)
    except (OSError, ValueError) as exc:
        print(f"报告读取失败: {exc}", file=sys.stderr)
        return ExitCode.FORMAT_ERROR
    ok, problems = verify_report(data)
    print(render_verify(data, ok, problems))
    if not ok:
        return ExitCode.FORMAT_ERROR
    return ExitCode.BUSINESS_ERROR if data.get("has_error") else ExitCode.OK


def main(argv: Optional[Sequence[str]] = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    table = {
        "generate-fixture": _cmd_generate,
        "reconcile": _cmd_reconcile,
        "verify": _cmd_verify,
    }
    return table[args.command](args)


if __name__ == "__main__":
    raise SystemExit(main())
