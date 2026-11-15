"""命令行入口：import / resolve / render / diff 子命令，只做编排。

退出码：0 正常；1 有未消解冲突或时长偏差超阈；2 输入非法（结构化错误码）。
"""
from __future__ import annotations

import argparse
import json
import sys
from fractions import Fraction
from pathlib import Path

from .formats import otio_bridge, v1, v2
from .render.plan import render_plan
from .timeline.model import ModelError, Timeline, parse_fps
from .timeline.resolve import resolve_timeline
from .timeline.scan import audio_video_deviation, scan_timeline

AUDIO_TOLERANCE_FRAMES = 2


def _load_json(path: str) -> dict:
    try:
        with open(path, "r", encoding="utf-8") as fh:
            return json.load(fh)
    except FileNotFoundError as exc:
        raise ModelError("IO_ERROR", f"文件不存在: {path}") from exc
    except json.JSONDecodeError as exc:
        raise ModelError("INVALID_FORMAT", f"{path} 不是合法 JSON: {exc}") from exc


def _detect_format(data: dict) -> str:
    if "OTIO_SCHEMA" in str(data.get("schema", "")):
        return "otio"
    version = data.get("version")
    if version == 2:
        return "v2"
    if version == 1:
        return "v1"
    raise ModelError("INVALID_FORMAT", "无法识别的格式（缺少 version 字段）")


def _parse_timeline(data: dict, fmt: str) -> Timeline:
    if fmt == "v1":
        return v1.parse(data)
    if fmt == "v2":
        return v2.parse(data)
    if fmt == "otio":
        import opentimelineio as otio
        ot = otio.schema.Timeline.from_json_string(json.dumps(data))
        return otio_bridge.from_otio(ot)
    raise ModelError("INVALID_FORMAT", f"未知格式: {fmt}")


def _serialize(timeline: Timeline, fmt: str) -> dict | str:
    if fmt == "v1":
        return v1.serialize(timeline)
    if fmt == "v2":
        return v2.serialize(timeline)
    if fmt == "otio":
        return otio_bridge.to_otio(timeline).to_json_string()
    raise ModelError("INVALID_FORMAT", f"未知格式: {fmt}")


def _write(path: str, payload: dict | str) -> None:
    text = payload if isinstance(payload, str) else json.dumps(
        payload, ensure_ascii=False, indent=2)
    Path(path).write_text(text + ("" if text.endswith("\n") else "\n"),
                          encoding="utf-8")


def _emit(payload: dict, as_json: bool) -> None:
    if as_json:
        print(json.dumps(payload, ensure_ascii=False))
    else:
        for key, val in payload.items():
            print(f"{key}: {val}")


def _num(frames: Fraction):
    return frames.numerator if frames.denominator == 1 else float(frames)


def cmd_import(args) -> int:
    data = _load_json(args.input)
    fmt = args.format or _detect_format(data)
    timeline = _parse_timeline(data, fmt)
    target = args.to or ("v2" if fmt == "v1" else fmt)
    payload = _serialize(timeline, target)
    if args.out:
        _write(args.out, payload)
    _emit({"imported_format": fmt, "exported_format": target,
           "clips": timeline.clip_count(),
           "out": args.out or "-"}, args.json)
    return 0


def cmd_resolve(args) -> int:
    data = _load_json(args.input)
    fmt = args.format or _detect_format(data)
    timeline = _parse_timeline(data, fmt)
    duration_before = timeline.duration
    result = resolve_timeline(timeline, policy=args.policy)
    duration_after = timeline.duration
    report = scan_timeline(timeline)
    deviation = audio_video_deviation(timeline)

    intervals = [
        {"track": t.index, "start_frame": _num(c.range.start),
         "end_frame": _num(c.range.end), "clip_id": c.clip_id}
        for t in timeline.tracks for c in t.sorted_clips()
    ]
    out_fmt = args.format or fmt
    if args.out:
        _write(args.out, _serialize(timeline, out_fmt))
    payload = {
        "conflicts_detected": result.conflicts_detected,
        "conflicts_resolved": result.conflicts_resolved,
        "zero_length_clips": result.zero_length_clips,
        "rounds": result.rounds,
        "intervals": intervals,
        "duration_before_frames": _num(duration_before),
        "duration_after_frames": _num(duration_after),
        "duration_delta_frames": _num(duration_after - duration_before),
        "audio_deviation_frames": _num(deviation),
    }
    if deviation > AUDIO_TOLERANCE_FRAMES:
        payload["warning"] = (f"音频与视频总长偏差 {_num(deviation)} 帧，"
                              f"超过 {AUDIO_TOLERANCE_FRAMES} 帧阈值")
    _emit(payload, args.json)
    if report.conflict_count > 0 or deviation > AUDIO_TOLERANCE_FRAMES:
        return 1
    return 0


def cmd_render(args) -> int:
    data = _load_json(args.input)
    timeline = _parse_timeline(data, _detect_format(data))
    if args.fps is not None and parse_fps(args.fps) != timeline.fps:
        raise ModelError(
            "FPS_CONFLICT",
            f"请求帧率 {args.fps} 与文件帧率 {float(timeline.fps)} 不一致")
    plan = render_plan(timeline)
    if args.out:
        _write(args.out, plan)
    _emit({"frame_count": plan["frame_count"],
           "plan_sha256": plan["plan_sha256"],
           "out": args.out or "-"}, args.json)
    return 0


def cmd_diff(args) -> int:
    target = args.format
    timelines = []
    for path in (args.a, args.b):
        data = _load_json(path)
        timelines.append(_parse_timeline(data, _detect_format(data)))
    serialized = [_serialize(tl, target) for tl in timelines]
    if target == "otio":
        equal = serialized[0] == serialized[1]
        diff_count = 0 if equal else 1
    else:
        a_items = json.dumps(serialized[0], sort_keys=True)
        b_items = json.dumps(serialized[1], sort_keys=True)
        equal = a_items == b_items
        diff_count = 0 if equal else _count_diffs(serialized[0], serialized[1])
    _emit({"round_trip_equal": equal, "diff_count": diff_count}, args.json)
    return 0 if equal else 1


def _count_diffs(a, b) -> int:
    """逐字段比对两个序列化结构，统计不一致的叶子字段数。"""
    if isinstance(a, dict) and isinstance(b, dict):
        return sum(_count_diffs(a.get(k), b.get(k))
                   for k in set(a) | set(b))
    if isinstance(a, list) and isinstance(b, list):
        count = abs(len(a) - len(b))
        count += sum(_count_diffs(x, y) for x, y in zip(a, b))
        return count
    return 0 if a == b else 1


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="timeline", description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    p = sub.add_parser("import", help="导入并转换格式")
    p.add_argument("--input", required=True)
    p.add_argument("--format", choices=["v1", "v2", "otio"])
    p.add_argument("--to", choices=["v1", "v2", "otio"])
    p.add_argument("--out")
    p.add_argument("--json", action="store_true")
    p.set_defaults(func=cmd_import)

    p = sub.add_parser("resolve", help="消解重叠")
    p.add_argument("--input", required=True)
    p.add_argument("--format", choices=["v1", "v2", "otio"])
    p.add_argument("--policy", default="priority")
    p.add_argument("--out")
    p.add_argument("--json", action="store_true")
    p.set_defaults(func=cmd_resolve)

    p = sub.add_parser("render", help="生成帧计划")
    p.add_argument("--input", required=True)
    p.add_argument("--fps")
    p.add_argument("--out")
    p.add_argument("--json", action="store_true")
    p.set_defaults(func=cmd_render)

    p = sub.add_parser("diff", help="两版本语义比对")
    p.add_argument("--a", required=True)
    p.add_argument("--b", required=True)
    p.add_argument("--format", choices=["v1", "v2", "otio"], default="v2")
    p.add_argument("--json", action="store_true")
    p.set_defaults(func=cmd_diff)
    return parser


def main(argv=None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return args.func(args)
    except ModelError as exc:
        print(json.dumps({"error": {"code": exc.code, "message": exc.message}},
                         ensure_ascii=False), file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
