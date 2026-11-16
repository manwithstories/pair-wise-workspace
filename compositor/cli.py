"""终端入口: generate-fixture / render 两个子命令。

退出码契约:
- 0: 全部成功;
- 2: 输入或时间线校验失败(JSON 损坏、字段缺失、alpha 超界等);
- 3: 渲染或音频处理失败;
- 4: 摘要写入失败。

所有机器可读结果均为 JSON(stdout 单行 envelope, 帧摘要走 JSONL 文件)。
"""

from __future__ import annotations

import argparse
import json
import sys

from .audio import analyze_audio
from .exporter import export_jsonl
from .timeline import load_timeline
from .types import (
    EXIT_DIGEST_WRITE,
    EXIT_OK,
    EXIT_RENDER,
    EXIT_VALIDATION,
    CompositorError,
    DigestWriteError,
)


def _emit(payload: dict) -> None:
    sys.stdout.write(json.dumps(payload, ensure_ascii=False,
                                separators=(",", ":")))
    sys.stdout.write("\n")


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="python -m compositor.cli",
        description="确定性离线帧序列合成与摘要工具",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    gen = sub.add_parser("generate-fixture",
                         help="生成确定性时间线夹具 JSON")
    gen.add_argument("--out", required=True, help="夹具 JSON 输出路径")

    render = sub.add_parser("render", help="渲染时间线并输出帧摘要 JSONL")
    render.add_argument("--timeline", required=True, help="时间线 JSON 路径")
    render.add_argument("--fps", type=int, choices=(24, 30, 60),
                        default=None, help="覆盖时间线 fps")
    render.add_argument("--sample-rate", type=int, default=None,
                        help="覆盖整数采样率")
    render.add_argument("--output", required=True, help="帧摘要 JSONL 路径")
    return parser


def _cmd_generate_fixture(args: argparse.Namespace) -> int:
    try:
        from fixtures.generate import build_fixture, write_fixture

        write_fixture(args.out)
    except OSError as exc:
        _emit({"ok": False, "command": "generate-fixture",
               "error": {"code": "digest_write_failed",
                         "message": f"夹具写入失败: {exc}"}})
        return EXIT_DIGEST_WRITE
    _emit({"ok": True, "command": "generate-fixture",
           "data": {"path": args.out, **{
               key: build_fixture()["timeline"][key]
               for key in ("fps", "sample_rate", "frame_count")}}})
    return EXIT_OK


def _cmd_render(args: argparse.Namespace) -> int:
    timeline, report = load_timeline(args.timeline,
                                     fps_override=args.fps,
                                     sample_rate_override=args.sample_rate)
    if timeline is None:
        _emit({"ok": False, "command": "render",
               "error": {"code": "validation_failed",
                         "details": report.to_dict()}})
        return EXIT_VALIDATION

    try:
        # 音频分析先行: 采样范围问题在写任何摘要前暴露。
        audio_analysis = analyze_audio(timeline)
        summary, _ = export_jsonl(timeline, args.output)
    except DigestWriteError as exc:
        _emit({"ok": False, "command": "render",
               "error": {"code": "digest_write_failed", "message": str(exc)}})
        return EXIT_DIGEST_WRITE
    except CompositorError as exc:
        _emit({"ok": False, "command": "render",
               "error": {"code": exc.error_type.value, "message": str(exc)}})
        return EXIT_RENDER

    payload = {
        "ok": True,
        "command": "render",
        "data": {
            "output": args.output,
            "frame_count": summary.frame_count,
            "summary_sha256": summary.summary_sha256,
            "max_av_sync_error_us": summary.max_av_sync_error_us,
            "total_opaque_pixels": summary.total_opaque_pixels,
            "total_transparent_pixels": summary.total_transparent_pixels,
            "audio_event_count": summary.audio_event_count,
            "recoverable_entries": report.recoverable_entries,
            "audio_events": [
                {
                    "event_id": a.event_id,
                    "first_sample": a.first_sample,
                    "last_sample": a.last_sample,
                    "end_frame": a.end_frame,
                    "av_sync_error_us": a.av_sync_error_us,
                }
                for a in audio_analysis
            ],
        },
    }
    _emit(payload)
    print(f"summary_sha256={summary.summary_sha256}", file=sys.stderr)
    return EXIT_OK


def main(argv: list[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    if args.command == "generate-fixture":
        return _cmd_generate_fixture(args)
    if args.command == "render":
        return _cmd_render(args)
    parser.error(f"未知子命令: {args.command}")
    return EXIT_RENDER  # pragma: no cover - argparse 会先退出


if __name__ == "__main__":
    raise SystemExit(main())
