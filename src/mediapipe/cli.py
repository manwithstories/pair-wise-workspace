"""命令行入口：纯终端 JSON 行输出，退出码遵循验收约定。

0 处理与校验通过；2 输入/映射无效；3 可定位媒体损坏；
4 续跑状态不兼容；5 内部写出/验证失败。
"""
from __future__ import annotations

import argparse
import json
import sys
from dataclasses import asdict
from typing import Any

from .demux import Demuxer
from .errors import (
    EXIT_INTERNAL,
    EXIT_OK,
    MediaError,
)
from .mapping import default_mapping
from .pipeline import Pipeline, PipelineConfig


def _emit(payload: dict[str, Any]) -> None:
    print(json.dumps(payload, ensure_ascii=False, sort_keys=True), flush=True)


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="mediapipe", description="无损 MP4/MOV 重封装流水线")
    sub = parser.add_subparsers(dest="command", required=True)

    remux = sub.add_parser("remux", help="解复用→映射→重封装→回读验证")
    remux.add_argument("--input", required=True)
    remux.add_argument("--output", required=True)
    remux.add_argument("--checkpoint", required=True)
    remux.add_argument("--queue-bytes", type=int, default=1 << 20)
    remux.add_argument("--queue-packets", type=int, default=None)
    remux.add_argument("--slow-consumer-ms", type=float, default=0.0)
    remux.add_argument("--checkpoint-every", type=int, default=50)
    remux.add_argument("--backpressure-abort", action="store_true")
    remux.add_argument("--verify", action="store_true")
    remux.set_defaults(func=_cmd_remux)

    inspect = sub.add_parser("inspect", help="仅解复用并打印流报告")
    inspect.add_argument("--input", required=True)
    inspect.set_defaults(func=_cmd_inspect)
    return parser


def _cmd_inspect(args: argparse.Namespace) -> int:
    with Demuxer(args.input) as demuxer:
        records = list(demuxer.iter_packets())
        _emit(
            {
                "event": "inspect",
                "input_bytes": demuxer.file_size,
                "streams": [i.to_dict() for i in demuxer.stream_infos],
                "skipped_streams": [i.to_dict() for i in demuxer.skipped_streams],
                "packets": len(records),
                "negative_priming": demuxer.negative_priming,
            }
        )
    return EXIT_OK


def _cmd_remux(args: argparse.Namespace) -> int:
    with Demuxer(args.input) as demuxer:
        mappings = default_mapping(demuxer.stream_infos)
    config = PipelineConfig(
        input_path=args.input,
        output_path=args.output,
        checkpoint_path=args.checkpoint,
        queue_bytes=args.queue_bytes,
        queue_packets=args.queue_packets,
        slow_consumer_ms=args.slow_consumer_ms,
        checkpoint_every=args.checkpoint_every,
        verify=args.verify,
        backpressure_abort=args.backpressure_abort,
    )
    result = Pipeline(config, mappings).run()
    payload = asdict(result)
    payload["event"] = "remux"
    # 扁平化验收关注的关键字段，便于一眼核对数量级
    payload["input_bytes"] = result.input_bytes
    payload["queue_peak_bytes"] = result.queue["queue_peak_bytes"]
    if result.verification:
        payload["packet_sha256_match"] = result.verification[
            "all_packet_sha256_match"
        ]
    if result.resumed:
        payload["resume"] = {
            "resumed_from_sequence": result.resumed_from_sequence,
            "resume_ms": result.resume_ms,
        }
    _emit(payload)
    return EXIT_OK


def main(argv: list[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except MediaError as exc:
        _emit({"event": "error", **exc.to_dict()})
        return exc.exit_code
    except (OSError, ValueError, RuntimeError) as exc:
        _emit(
            {
                "event": "error",
                "error": type(exc).__name__,
                "message": str(exc),
                "exit_code": EXIT_INTERNAL,
            }
        )
        return EXIT_INTERNAL


if __name__ == "__main__":
    sys.exit(main())
