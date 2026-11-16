"""命令行入口:generate / mix / verify。

退出码契约:
    0 成功;2 参数或配置错误;3 输入媒体错误;
    4 输出写入失败;5 离线与流式数值不一致。
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

import numpy as np
import soundfile as sf

from mixer.config import load_config
from mixer.fixtures import generate_fixtures
from mixer.offline import run_mix
from mixer.types import ConsistencyError, MixerError


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="cli.py",
        description="多来源素材时间线双声道混音器(纯本地、无服务依赖)",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    p_gen = sub.add_parser("generate", help="生成样例夹具与工程配置")
    p_gen.add_argument("--out", required=True, help="输出目录")

    p_mix = sub.add_parser("mix", help="按工程配置执行混音")
    p_mix.add_argument("--config", required=True, help="project.json 路径")
    p_mix.add_argument("--output", required=True, help="输出 WAV 路径")
    p_mix.add_argument("--report", required=True, help="输出 JSON 统计路径")
    p_mix.add_argument("--mode", choices=("offline", "stream"), required=True)
    p_mix.add_argument("--block-frames", type=int, default=None,
                       help="流式块大小(默认取配置 block_frames)")

    p_ver = sub.add_parser("verify", help="逐采样比对两份 WAV")
    p_ver.add_argument("--left", required=True)
    p_ver.add_argument("--right", required=True)
    p_ver.add_argument("--report", required=True)
    p_ver.add_argument("--tolerance", type=float, default=1e-6)
    return parser


def _cmd_generate(args: argparse.Namespace) -> int:
    paths = generate_fixtures(args.out)
    print(json.dumps({"ok": True, "generated": paths}, ensure_ascii=False, indent=2))
    return 0


def _cmd_mix(args: argparse.Namespace) -> int:
    config = load_config(args.config)
    stats = run_mix(
        config=config,
        config_path=args.config,
        output_wav=args.output,
        report_path=args.report,
        mode=args.mode,
        block_frames=args.block_frames,
    )
    print(json.dumps({"ok": True, "report": stats.to_dict()},
                     ensure_ascii=False, indent=2))
    return 0


def _cmd_verify(args: argparse.Namespace) -> int:
    left_path, right_path = Path(args.left), Path(args.right)
    try:
        with sf.SoundFile(str(left_path)) as lf:
            left = lf.read(dtype="float64", always_2d=True)
            l_rate, l_ch = lf.samplerate, lf.channels
        with sf.SoundFile(str(right_path)) as rf:
            right = rf.read(dtype="float64", always_2d=True)
            r_rate, r_ch = rf.samplerate, rf.channels
    except Exception as exc:
        raise ConsistencyError(
            f"verify 输入无法读取: {exc}",
            details={"reason": "read_failed"},
        ) from exc

    mismatch: dict[str, object] = {
        "left_sample_rate": l_rate,
        "right_sample_rate": r_rate,
        "left_channels": l_ch,
        "right_channels": r_ch,
        "left_frames": int(left.shape[0]),
        "right_frames": int(right.shape[0]),
    }
    if left.shape != right.shape or l_rate != r_rate or l_ch != r_ch:
        report = {"ok": False, "reason": "shape_mismatch", **mismatch}
        _write_verify_report(args.report, report)
        raise ConsistencyError("两份 WAV 形状或采样率不一致", details=report)

    diff = np.abs(left - right)
    max_abs_diff = float(diff.max()) if diff.size else 0.0
    mean_abs_diff = float(diff.mean()) if diff.size else 0.0
    report = {
        "ok": max_abs_diff <= args.tolerance,
        "sample_rate": l_rate,
        "channels": l_ch,
        "frames": int(left.shape[0]),
        "max_abs_diff": max_abs_diff,
        "mean_abs_diff": mean_abs_diff,
        "tolerance": args.tolerance,
    }
    _write_verify_report(args.report, report)
    if not report["ok"]:
        raise ConsistencyError(
            f"离线/流式差异 {max_abs_diff:.3e} 超过容差 {args.tolerance:.0e}",
            details=report,
        )
    print(json.dumps(report, ensure_ascii=False, indent=2))
    return 0


def _write_verify_report(path: str | Path, report: dict) -> None:
    try:
        p = Path(path)
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(json.dumps(report, ensure_ascii=False, indent=2),
                     encoding="utf-8")
    except OSError as exc:
        from mixer.types import OutputError

        raise OutputError(
            f"verify 报告写入失败: {path}: {exc}",
            details={"path": str(path), "reason": "write_failed"},
        ) from exc


def main(argv: list[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    handlers = {
        "generate": _cmd_generate,
        "mix": _cmd_mix,
        "verify": _cmd_verify,
    }
    try:
        return handlers[args.command](args)
    except MixerError as exc:
        payload = {"ok": False, "code": exc.code, "message": exc.message,
                   "details": exc.details}
        print(json.dumps(payload, ensure_ascii=False, indent=2), file=sys.stderr)
        return exc.exit_code
    except argparse.ArgumentError as exc:
        print(json.dumps({"ok": False, "code": "argument_error",
                          "message": str(exc)}, ensure_ascii=False),
              file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
