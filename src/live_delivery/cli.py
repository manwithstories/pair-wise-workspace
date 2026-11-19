"""命令行入口：process（处理并生成交付）/ verify（校验统计阈值）。

退出码：0 全部通过；2 参数或清单无效；3 存在不可恢复媒体错误；4 统计未达阈值。
"""
from __future__ import annotations

import argparse
import math
import sys
from pathlib import Path

import numpy as np
import soundfile as sf

from . import audio, decode
from . import report as report_mod
from .errors import ManifestError, MediaError, PipelineError
from .manifest import load_manifest
from .pipeline import SENTINEL, Pipeline
from .timeline import Timeline
from .transport import Gap, ingest

EXIT_OK = 0
EXIT_USAGE = 2
EXIT_MEDIA = 3
EXIT_THRESHOLD = 4

SILENCE_CHUNK_S = 0.5   # 静音填充分块（秒），限制单块内存
QUEUE_MAX_ITEMS = 8


def _build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        prog="live_delivery", description="多机位现场音频交付流水线")
    sub = p.add_subparsers(dest="command", required=True)
    proc = sub.add_parser("process", help="处理清单并生成交付文件与统计")
    proc.add_argument("--manifest", required=True, help="本地 HLS(.m3u8)/DASH(.mpd) 清单")
    proc.add_argument("--out", required=True, help="输出 WAV 文件路径")
    proc.add_argument("--rate", type=int, default=48000, help="目标采样率")
    proc.add_argument("--layout", choices=sorted(audio.TARGET_LAYOUTS), default="stereo")
    proc.add_argument("--target-lufs", type=float, default=-16.0, help="目标综合响度")
    proc.add_argument("--max-peak-dbfs", type=float, default=-1.0, help="峰值上限")
    proc.add_argument("--max-queue-mib", type=float, default=8.0, help="单队列字节上限")
    proc.add_argument("--gap-policy", choices=["silence", "fail"], default="silence",
                      help="缺失区间策略：静音填充或标记不可恢复")
    proc.add_argument("--lateness-window", type=float, default=None,
                      help="迟到分片补入窗口（秒），默认 2 倍目标分片时长")
    proc.add_argument("--json", default=None, help="统计 JSON 输出路径")
    ver = sub.add_parser("verify", help="校验交付统计是否达标")
    ver.add_argument("--report", required=True, help="process 生成的统计 JSON")
    return p


def _silence_chunks(start: float, duration: float, rate: int, channels: int,
                    layout: str):
    """按输出采样位置精确切分静音块，保证缺口时长准确。"""
    p0 = round(start * rate)
    p1 = round((start + duration) * rate)
    step = max(1, int(SILENCE_CHUNK_S * rate))
    pos = p0
    while pos < p1:
        n = min(step, p1 - pos)
        yield decode.AudioChunk(
            np.zeros((channels, n), dtype=np.float32), rate, layout, -1, pos / rate)
        pos += n


def _execute(args, items, errors: list[MediaError], gain_lin: float,
             emit_gaps: bool, on_chunk) -> int:
    """运行一条 源 → 变换 → 汇聚 流水线，返回峰值缓存字节数。"""
    channels = audio.TARGET_LAYOUTS[args.layout]
    max_bytes = max(1, int(args.max_queue_mib * 1024 * 1024))
    pipe = Pipeline()
    q1 = pipe.queue(QUEUE_MAX_ITEMS, max_bytes)
    q2 = pipe.queue(QUEUE_MAX_ITEMS, max_bytes)
    cancel = pipe.cancelled

    def emit_silence(start: float, duration: float) -> None:
        for chunk in _silence_chunks(start, duration, args.rate, channels, args.layout):
            if cancel.is_set() or not q1.put(chunk, chunk.nbytes):
                break

    def source() -> None:
        for _start, kind, obj in items:
            if cancel.is_set():
                break
            if kind == "gap":
                if not emit_gaps:
                    continue
                if args.gap_policy == "fail":
                    errors.append(MediaError(
                        "transport", f"不可恢复缺口（{obj.reason}）", None,
                        (obj.start, obj.end), recoverable=False))
                    cancel.set()
                    break
                emit_silence(obj.start, obj.duration)
                continue
            try:
                for chunk in decode.decode(obj):
                    if cancel.is_set() or not q1.put(chunk, chunk.nbytes):
                        break
            except decode.DecodeError as exc:
                recoverable = args.gap_policy == "silence"
                errors.append(MediaError(
                    "decode", str(exc), obj.ref.uri,
                    (obj.ref.start, obj.ref.end), recoverable=recoverable))
                if not recoverable:
                    cancel.set()
                    break
                if emit_gaps:
                    emit_silence(obj.ref.start, obj.ref.duration)
        q1.put(SENTINEL)

    def transform() -> None:
        while True:
            item = q1.get()
            if item is None:
                if cancel.is_set():
                    break
                continue
            if item is SENTINEL:
                q2.put(SENTINEL)
                break
            data = audio.resample(item.data, item.rate, args.rate)
            data = audio.normalize_layout(data, item.layout, args.layout)
            if gain_lin != 1.0:
                data = (data * gain_lin).astype(np.float32)
            out = decode.AudioChunk(data, args.rate, args.layout,
                                    item.sequence, item.media_start)
            if not q2.put(out, out.nbytes):
                break

    def sink() -> None:
        while True:
            item = q2.get()
            if item is None:
                if cancel.is_set():
                    break
                continue
            if item is SENTINEL:
                break
            on_chunk(item)

    pipe.add_stage("source", source)
    pipe.add_stage("transform", transform)
    pipe.add_stage("sink", sink)
    pipe.run()
    errors.extend(pipe.errors)
    return pipe.peak_buffer_bytes


def _work_items(result) -> list[tuple[float, str, object]]:
    items = [(d.ref.start, "segment", d) for d in result.deliveries]
    items += [(g.start, "gap", g) for g in result.gaps]
    items.sort(key=lambda x: (x[0], x[1]))
    return items


def _round(value: float | None, ndigits: int = 3) -> float | None:
    if value is None or not math.isfinite(value):
        return None
    return round(value, ndigits)


def _cmd_process(args) -> int:
    try:
        manifest = load_manifest(args.manifest)
    except ManifestError as exc:
        print(f"清单无效: {exc}", file=sys.stderr)
        return EXIT_USAGE
    if args.rate <= 0:
        print("参数无效: --rate 必须为正数", file=sys.stderr)
        return EXIT_USAGE

    result = ingest(manifest, args.lateness_window)
    errors: list[MediaError] = list(result.errors)
    items = _work_items(result)
    channels = audio.TARGET_LAYOUTS[args.layout]
    if not result.deliveries:
        errors.append(MediaError("transport", "没有可用分片", recoverable=False))

    # 第一遍：流式测量响度与峰值（仅真实分片，缺口静音不参与响度）
    meter1 = audio.LoudnessMeter(args.rate, channels)
    pre_peak = [-math.inf]

    def measure(chunk) -> None:
        meter1.update(chunk.data)
        pre_peak[0] = max(pre_peak[0], audio.true_peak_dbfs(chunk.data))

    try:
        peak1 = _execute(args, items, errors, 1.0, emit_gaps=False, on_chunk=measure)
    except PipelineError as exc:
        print(f"流水线失败: {exc}", file=sys.stderr)
        return EXIT_MEDIA

    measured = meter1.integrated()
    gain = audio.gain_db(measured, args.target_lufs, pre_peak[0], args.max_peak_dbfs)
    gain_lin = 10.0 ** (gain / 20.0)

    # 第二遍：变换 + 增益 + 写出 + 统计
    timeline = Timeline(args.rate)
    meter2 = audio.LoudnessMeter(args.rate, channels)
    state = {"written": 0, "silent": 0, "peak": -math.inf}
    seen_seqs: set[int] = set()
    out_path = Path(args.out)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    writer = sf.SoundFile(str(out_path), "w", samplerate=args.rate,
                          channels=channels, subtype="PCM_16")

    def deliver(chunk) -> None:
        if chunk.sequence >= 0 and chunk.sequence not in seen_seqs:
            seen_seqs.add(chunk.sequence)
            timeline.record(chunk.sequence, chunk.media_start, state["written"])
        writer.write(chunk.data.T)
        meter2.update(chunk.data)
        state["peak"] = max(state["peak"], audio.true_peak_dbfs(chunk.data))
        if audio.is_silence(chunk.data):
            state["silent"] += chunk.samples
        state["written"] += chunk.samples

    try:
        peak2 = _execute(args, items, errors, gain_lin, emit_gaps=True, on_chunk=deliver)
    except PipelineError as exc:
        writer.close()
        print(f"流水线失败: {exc}", file=sys.stderr)
        return EXIT_MEDIA
    writer.close()

    written = state["written"]
    duration_out = written / args.rate
    file_bytes = out_path.stat().st_size if out_path.is_file() else 0
    target_bitrate = args.rate * channels * 16
    if duration_out > 0 and file_bytes > 0:
        actual_bitrate: float | None = file_bytes * 8.0 / duration_out
        delta_pct: float | None = (actual_bitrate - target_bitrate) / target_bitrate * 100.0
        basis = "output_file"
    else:
        # 缺少有效负载样本：标记估算依据，不伪报精确测量值
        actual_bitrate = None
        delta_pct = None
        basis = "insufficient_samples"

    unrecoverable = [e for e in errors if not e.recoverable]
    decode_recovered = sum(1 for e in errors
                           if e.stage == "decode" and e.recoverable)
    recovered_gaps = (len(result.gaps) + decode_recovered
                      if args.gap_policy == "silence" else 0)
    rep = {
        "manifest": str(manifest.path),
        "kind": manifest.kind,
        "output": str(out_path),
        "rate": args.rate,
        "layout": args.layout,
        "target_lufs": args.target_lufs,
        "integrated_lufs": _round(meter2.integrated()),
        "true_peak_dbfs": _round(state["peak"]),
        "silence_ratio": round(state["silent"] / written, 6) if written else 0.0,
        "target_bitrate_bps": target_bitrate,
        "actual_bitrate_bps": _round(actual_bitrate, 1),
        "bitrate_delta_pct": _round(delta_pct, 4),
        "bitrate_basis": basis,
        "manifest_alignment_ms": round(timeline.max_error_ms(), 3),
        "duration_delta_ms": round((duration_out - manifest.total_duration) * 1000.0, 3),
        "duplicate_segments": result.duplicates,
        "isolated_segments": result.conflicts,
        "late_dropped_segments": result.late_dropped,
        "missing_segments": result.missing,
        "recovered_gaps": recovered_gaps,
        "unrecoverable_errors": len(unrecoverable),
        "peak_buffer_bytes": max(peak1, peak2),
        "gain_db": round(gain, 3),
        "gaps": [{"start": g.start, "duration": g.duration, "reason": g.reason}
                 for g in result.gaps],
        "errors": [e.as_dict() for e in errors],
    }
    report_mod.print_summary(rep)
    if args.json:
        report_mod.write_json(rep, args.json)
    for e in errors:
        level = "不可恢复" if not e.recoverable else "已恢复"
        print(f"[{e.stage}] {level}: {e.message}", file=sys.stderr)
    if unrecoverable:
        return EXIT_MEDIA
    return EXIT_OK


def _cmd_verify(args) -> int:
    try:
        rep = report_mod.load_json(args.report)
    except Exception as exc:
        print(f"报告无效: {exc}", file=sys.stderr)
        return EXIT_USAGE
    failures = report_mod.verify(rep)
    if failures:
        for f in failures:
            print(f"未达标: {f}")
        return EXIT_THRESHOLD
    print("全部校验通过")
    return EXIT_OK


def main(argv: list[str] | None = None) -> int:
    args = _build_parser().parse_args(argv)
    if args.command == "process":
        return _cmd_process(args)
    return _cmd_verify(args)


if __name__ == "__main__":
    sys.exit(main())
