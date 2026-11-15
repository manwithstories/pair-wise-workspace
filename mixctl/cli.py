"""命令行入口：fixtures / render / compare 三个子命令与固定退出码。"""

from __future__ import annotations

import argparse
import json
import sys

from .errors import EXIT_DIVERGENCE, EXIT_OK, EXIT_USAGE, MixctlError
from .fixtures import generate_fixtures
from .pipeline import compare_wav, run_render


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="mixctl",
        description="本地多轨混音流水线（重采样、自动化、离线/流式渲染）",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    fixtures = sub.add_parser("fixtures", help="生成演示工程与源 WAV")
    fixtures.add_argument("--out", required=True, help="输出目录")

    render = sub.add_parser("render", help="按工程渲染主输出 WAV 与报告")
    render.add_argument("--project", required=True, help="工程 JSON 路径")
    render.add_argument("--out", required=True, help="输出 WAV 路径")
    render.add_argument("--report", default=None, help="报告 JSON 输出路径")
    render.add_argument(
        "--engine", choices=("offline", "stream"), default="offline",
        help="渲染引擎，默认 offline",
    )
    render.add_argument(
        "--chunk", type=int, default=777,
        help="流式引擎的输出块长（采样数），默认 777",
    )

    compare = sub.add_parser("compare", help="逐样本比较两个 WAV")
    compare.add_argument("--a", required=True, help="第一个 WAV")
    compare.add_argument("--b", required=True, help="第二个 WAV")
    compare.add_argument("--json", action="store_true", help="只输出 JSON")
    return parser


def _cmd_fixtures(args: argparse.Namespace) -> int:
    project_path = generate_fixtures(args.out)
    print(f"fixtures 已生成: {project_path}")
    return EXIT_OK


def _cmd_render(args: argparse.Namespace) -> int:
    if args.chunk <= 0:
        print(f"错误: --chunk 必须为正整数，得到 {args.chunk}", file=sys.stderr)
        return EXIT_USAGE
    report = run_render(
        project_path=args.project,
        out_wav=args.out,
        report_path=args.report,
        engine=args.engine,
        chunk=args.chunk,
    )
    master = report["master"]
    print(
        "render 完成: engine={engine} samples={samples} "
        "peak={peak} rms={rms} clips={clips}".format(
            engine=report["engine"],
            samples=report["samples"],
            peak=master["peak_dbfs"],
            rms=master["rms_dbfs"],
            clips=master["clip_samples"],
        )
    )
    return EXIT_OK


def _cmd_compare(args: argparse.Namespace) -> int:
    result = compare_wav(args.a, args.b)
    if args.json:
        print(json.dumps(result, ensure_ascii=False))
    else:
        print(
            "identical={identical} max_abs_diff={max_abs_diff} "
            "samples={samples}".format(**result)
        )
    return EXIT_OK if result["identical"] else EXIT_DIVERGENCE


_HANDLERS = {
    "fixtures": _cmd_fixtures,
    "render": _cmd_render,
    "compare": _cmd_compare,
}


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    handler = _HANDLERS[args.command]
    try:
        return handler(args)
    except MixctlError as exc:
        print(f"错误[{exc.exit_code}]: {exc}", file=sys.stderr)
        return exc.exit_code
    except argparse.ArgumentTypeError as exc:  # pragma: no cover
        print(f"错误[{EXIT_USAGE}]: {exc}", file=sys.stderr)
        return EXIT_USAGE


if __name__ == "__main__":
    sys.exit(main())
