"""relaymix 命令行：inspect / render / resume / verify / demo。

退出码：
    0  处理或核验成功（含中断恢复后完成）
    2  清单或参数无效
    3  素材损坏
    4  检查点或溯源校验失败
    1  其他未预期错误
全部终端输出为 JSON 行，字段名固定。
"""

import argparse
import json
import sys
from pathlib import Path

from .errors import RelayError
from .frame_export import iter_windows
from .manifest import parse_manifest
from .pipeline import run_job
from .verify import verify_root

# tests/make_fixtures.py 位于仓库 tests/，通过 sys.path 按需引入。
_FIXTURE_CHOICES = (
    "normal", "truncated_wav", "short_data",
    "illegal_channels", "empty_block", "boundary",
)


def _emit(obj: dict) -> None:
    print(json.dumps(obj, ensure_ascii=False, sort_keys=True))


def _load_fixture_builder():
    tests_dir = Path(__file__).resolve().parents[2] / "tests"
    sys.path.insert(0, str(tests_dir))
    import make_fixtures  # type: ignore
    return make_fixtures


def cmd_inspect(args: argparse.Namespace) -> int:
    manifest = parse_manifest(args.manifest)
    blocks = [
        {
            "track": t.name,
            "block_id": b.block_id,
            "path": b.path,
            "start_tick": b.start_tick,
            "length_ticks": b.length_ticks,
            "order": b.order,
        }
        for t in manifest.tracks for b in t.blocks
    ]
    windows = list(iter_windows(
        manifest.output_start, manifest.output_end, manifest.frame_size
    ))
    _emit({
        "status": "ok",
        "master_rate": manifest.master_rate,
        "output_range": [manifest.output_start, manifest.output_end],
        "frame_size": manifest.frame_size,
        "output_channels": manifest.output_channels,
        "bad_block_policy": manifest.bad_block_policy,
        "tracks": [
            {"name": t.name, "channel_map": list(t.channel_map),
             "block_count": len(t.blocks)} for t in manifest.tracks
        ],
        "overlaps": [o.__dict__ for o in manifest.overlaps],
        "blocks": blocks,
        "window_count": len(windows),
        "last_window": {
            "start_tick": windows[-1].start_tick,
            "end_tick": windows[-1].end_tick,
            "incomplete": (windows[-1].end_tick - windows[-1].start_tick)
            < manifest.frame_size,
        },
    })
    return 0


def cmd_render(args: argparse.Namespace) -> int:
    summary = run_job(args.root, args.manifest,
                      interrupt_after=args.interrupt_after)
    _emit(summary)
    return 0


def cmd_resume(args: argparse.Namespace) -> int:
    root = Path(args.root)
    manifest_path = Path(args.manifest) if args.manifest else \
        root / "manifest.json"
    summary = run_job(root, manifest_path, is_resume=True)
    _emit(summary)
    return 0


def cmd_verify(args: argparse.Namespace) -> int:
    summary = verify_root(args.root)
    _emit(summary)
    return 0 if summary["status"] == "ok" else 4


def cmd_demo(args: argparse.Namespace) -> int:
    fixtures = _load_fixture_builder()
    root = Path(args.root)
    root.mkdir(parents=True, exist_ok=True)
    builder = fixtures.FIXTURES.get(args.fixture)
    if builder is None:
        raise RelayError(
            f"unknown fixture {args.fixture}", error_code="bad_fixture"
        )
    manifest_path = builder(root)
    if args.fixture != "normal":
        # 损坏样例：abort 策略下让结构化错误冒泡（退出码 3）；
        # silence 策略下正常完成，摘要中保留填充溯源。
        summary = run_job(root, manifest_path,
                          interrupt_after=args.interrupt_after)
        _emit(summary)
        return 0
    summary = run_job(root, manifest_path,
                      interrupt_after=args.interrupt_after)
    _emit(summary)
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="relaymix",
                                     description="Offline multi-track PCM/WAV timeline rebuild")
    sub = parser.add_subparsers(dest="command", required=True)

    p = sub.add_parser("inspect", help="validate and summarize a manifest")
    p.add_argument("manifest")
    p.set_defaults(func=cmd_inspect)

    p = sub.add_parser("render", help="build timeline and export frame sequence")
    p.add_argument("--root", required=True)
    p.add_argument("--manifest", required=True)
    p.add_argument("--interrupt-after", type=int, default=None)
    p.set_defaults(func=cmd_render)

    p = sub.add_parser("resume", help="resume an interrupted job under root")
    p.add_argument("--root", required=True)
    p.add_argument("--manifest", default=None)
    p.set_defaults(func=cmd_resume)

    p = sub.add_parser("verify", help="read-only verification of a job root")
    p.add_argument("--root", required=True)
    p.add_argument("--json", action="store_true",
                   help="kept for CLI symmetry; output is always JSON lines")
    p.set_defaults(func=cmd_verify)

    p = sub.add_parser("demo", help="generate fixtures and run an end-to-end job")
    p.add_argument("--root", required=True)
    p.add_argument("--fixture", default="normal", choices=list(_FIXTURE_CHOICES))
    p.add_argument("--interrupt-after", type=int, default=None)
    p.set_defaults(func=cmd_demo)
    return parser


def main(argv: list[str] | argparse.Namespace | None = None) -> int:
    parser = build_parser()
    if isinstance(argv, argparse.Namespace):
        args = argv
    else:
        args = parser.parse_args(argv)
    try:
        return args.func(args)
    except RelayError as exc:
        _emit(exc.to_dict())
        return exc.exit_code
    except FileNotFoundError as exc:
        _emit({"error_code": "file_not_found", "message": str(exc)})
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
