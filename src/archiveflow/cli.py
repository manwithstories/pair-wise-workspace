"""Batch acceptance CLI and recovery orchestration.

Data flow: synthetic/captured packets -> RTP/RTCP parsing -> clock
normalisation -> reorder/repair -> bounded-queue pipeline -> decode with
freeze-frame fill -> aligned multi-bitrate segmentation -> JSON manifest
and statistics.
"""

from __future__ import annotations

import argparse
import math
import sys
from dataclasses import dataclass
from pathlib import Path

import av
import numpy as np
import soundfile as sf

from .clock import ClockDomain
from .errors import CapabilityError, ParameterError, UnrecoverableError
from .ladder import (SegmentResult, alignment_error_ms, build_ladder,
                     encode_segment, select_representations,
                     simulate_bandwidth)
from .manifest import (MANIFEST_VERSION, atomic_write_json, load_manifest,
                       safe_segment_name, validate_manifest)
from .metrics import bitrate_error_pct, psnr_db, ssim, summarize_quality
from .repair import FrameReassembler, RepairStats, ReorderBuffer
from .rtp import (RTCP_PT_SR, PacketOrganizer, parse_rtcp_sr, parse_rtp)
from .segment import plan_segments
from .stream import Pipeline
from .synthetic import Bundle, SyntheticParams, generate, synth_frame

AUDIO_CHUNK = 960  # 20 ms at 48 kHz
AUDIO_RATE = 48000


@dataclass
class RecoverConfig:
    out_dir: Path
    targets: list[int]
    reorder_window: int = 64
    queue_size: int = 256
    segment_ms: float = 4000.0
    fps: int = 15


def _parse_stream(bundle: Bundle, queue_size: int):
    """Push raw packets through the bounded pipeline, parsing RTP/RTCP."""
    organizer = PacketOrganizer()
    domain = ClockDomain()
    pipeline = Pipeline(queue_size)

    def producer(q):
        for raw in bundle.packets:
            q.put(raw)

    def consumer(items):
        for i, raw in enumerate(items):
            if len(raw) >= 2 and raw[1] == RTCP_PT_SR:
                sr = parse_rtcp_sr(raw)
                ext = organizer.extend_ts(sr.ssrc, sr.rtp_timestamp)
                domain.add_sender_report(sr, ext)
            else:
                organizer.push(parse_rtp(raw, arrival=i))

    stats = pipeline.run(producer, consumer)
    return organizer, domain, stats


def _reassemble(organizer: PacketOrganizer, domain: ClockDomain, ssrc: int,
                window: int):
    """Reorder one SSRC and rebuild frames with the shared error model."""
    stats = RepairStats()
    buf = ReorderBuffer(window, stats)
    reasm = FrameReassembler()
    clock = domain.clock_for(ssrc)
    pkts = organizer.streams().get(ssrc, [])
    if pkts:
        clock.set_origin(min(p.timestamp for p in pkts))
    emitted = []
    for pkt in pkts:
        emitted.extend(buf.push(pkt))
    emitted.extend(buf.flush())
    frames = {}
    for em in emitted:
        frame = reasm.push(em)
        if frame is not None:
            frames[frame.index] = frame
    for frame in reasm.finish():  # partial frames at stream end
        frames.setdefault(frame.index, frame)
    return frames, stats


def _decode_video(frames: dict) -> dict[int, np.ndarray]:
    try:
        ctx = av.codec.CodecContext.create("mpeg4", "r")
    except Exception as exc:
        raise CapabilityError(f"mpeg4 decoder unavailable: {exc}") from exc
    decoded: dict[int, np.ndarray] = {}
    for idx in sorted(frames):
        frame = frames[idx]
        if frame.confidence < 1.0:
            continue  # partial frame: treat as lost, freeze-fill later
        try:
            for vf in ctx.decode(av.Packet(frame.data)):
                decoded[idx] = vf.to_ndarray(format="rgb24")
        except Exception:
            continue  # undecodable frame counts as lost
    return decoded


def _build_timeline(decoded: dict[int, np.ndarray], declared_frames: int):
    if not decoded:
        raise UnrecoverableError("no decodable video frames in input")
    max_idx = max(decoded)
    truncated = max_idx < declared_frames - 1
    timeline: list[tuple[int, np.ndarray]] = []
    last = None
    freeze = unrecoverable = 0
    for idx in range(max_idx + 1):
        frame = decoded.get(idx)
        if frame is None:
            if last is None:
                unrecoverable += 1  # no valid frame yet: cannot fill
                continue
            frame = last
            freeze += 1
        else:
            last = frame
        timeline.append((idx, frame))
    return timeline, truncated, freeze, unrecoverable


def _encode_ladder(timeline, segments, reps, fps, out_dir: Path):
    by_seg: dict[int, list] = {s.index: [] for s in segments}
    idx_by_seg: dict[int, list[int]] = {s.index: [] for s in segments}
    for idx, frame in timeline:
        t = idx * 1000.0 / fps
        for s in segments:
            if s.start_ms <= t < s.end_ms:
                by_seg[s.index].append(frame)
                idx_by_seg[s.index].append(idx)
                break
    results: list[SegmentResult] = []
    for s in segments:
        frames = by_seg[s.index]
        if not frames:
            continue
        for rep in reps:
            name = safe_segment_name(f"seg_{rep.name}_{s.index:03d}.mp4")
            size, dur = encode_segment(frames, rep, fps, str(out_dir / name))
            actual = size * 8.0 / dur if dur > 0 else 0.0
            results.append(SegmentResult(
                segment=s, representation=rep, path=name,
                actual_bps=actual,
                bitrate_error_pct=bitrate_error_pct(actual, rep.target_bps)))
    return results, idx_by_seg


def _recover_audio(frames: dict, total_samples: int, out_dir: Path):
    n_chunks = max(1, math.ceil(total_samples / AUDIO_CHUNK))
    out = np.zeros(n_chunks * AUDIO_CHUNK, dtype=np.int16)
    missing = 0
    for ci in range(n_chunks):
        expected = min(AUDIO_CHUNK, total_samples - ci * AUDIO_CHUNK)
        frame = frames.get(ci)
        arr = None
        if frame is not None and frame.confidence >= 1.0:
            candidate = np.frombuffer(frame.data, dtype=np.int16)
            if candidate.size == expected:
                arr = candidate
        if arr is None:
            missing += 1  # silence placeholder already zeroed
            continue
        out[ci * AUDIO_CHUNK:ci * AUDIO_CHUNK + expected] = arr
    sf.write(str(out_dir / "audio_recovered.wav"), out[:total_samples],
             AUDIO_RATE, subtype="PCM_16")
    return missing, n_chunks


def _quality_for_rep(bundle: Bundle, rep, rep_results, idx_by_seg, out_dir):
    psnrs, ssims = [], []
    for res in rep_results:
        container = av.open(str(out_dir / res.path))
        try:
            frames = [f.to_ndarray(format="rgb24")
                      for f in container.decode(video=0)]
        finally:
            container.close()
        for pos, arr in enumerate(frames):
            indices = idx_by_seg.get(res.segment.index, [])
            if pos >= len(indices):
                break
            ref = synth_frame(indices[pos], bundle.width, bundle.height,
                              bundle.seed)
            ref = av.VideoFrame.from_ndarray(ref, format="rgb24") \
                .reformat(width=rep.width, height=rep.height) \
                .to_ndarray(format="rgb24")
            psnrs.append(psnr_db(ref, arr))
            ssims.append(ssim(ref, arr))
    return summarize_quality(psnrs, ssims)


def recover(bundle: Bundle, cfg: RecoverConfig) -> dict:
    """Run the full recovery pipeline and write an atomic manifest."""
    if not bundle.packets:
        raise UnrecoverableError("input contains no packets (empty capture)")
    out_dir = Path(cfg.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    organizer, domain, pstats = _parse_stream(bundle, cfg.queue_size)
    video_frames, vstats = _reassemble(organizer, domain, bundle.video_ssrc,
                                       cfg.reorder_window)
    audio_frames, astats = _reassemble(organizer, domain, bundle.audio_ssrc,
                                       cfg.reorder_window)
    if not video_frames:
        raise UnrecoverableError("no video frames recoverable from input")
    decoded = _decode_video(video_frames)
    timeline, truncated, freeze, unrecoverable = _build_timeline(
        decoded, bundle.n_frames)
    fps = cfg.fps
    kf_times = [idx * 1000.0 / fps for idx, f in video_frames.items()
                if f.keyframe and idx in decoded]
    media_end_ms = (timeline[-1][0] + 1) * 1000.0 / fps
    segments = plan_segments(media_end_ms, kf_times, cfg.segment_ms,
                             truncated,
                             declared_end_ms=bundle.duration_s * 1000.0)
    if not segments:
        raise UnrecoverableError("no segments could be planned")
    reps = build_ladder(cfg.targets)
    results, idx_by_seg = _encode_ladder(timeline, segments, reps, fps,
                                         out_dir)
    bandwidth = simulate_bandwidth(len(segments), seed=bundle.seed)
    choices = select_representations(reps, len(segments), bandwidth)
    for r in results:
        if r.representation is reps[choices[r.segment.index]]:
            r.selected_for_delivery = True
    missing_chunks, n_chunks = _recover_audio(audio_frames,
                                              len(bundle.audio), out_dir)
    quality = {rep.name: _quality_for_rep(
        bundle, rep, [r for r in results if r.representation is rep],
        idx_by_seg, out_dir) for rep in reps}
    seg_entries = [{
        "index": r.segment.index,
        "file": r.path,
        "representation": r.representation.name,
        "start_ms": round(r.segment.start_ms, 3),
        "end_ms": round(r.segment.end_ms, 3),
        "duration_ms": round(r.segment.duration_ms, 3),
        "target_bps": r.representation.target_bps,
        "actual_bps": round(r.actual_bps, 1),
        "bitrate_error_pct": round(r.bitrate_error_pct, 2),
        "flags": list(r.segment.flags),
        "selected": r.selected_for_delivery,
    } for r in results]
    return {
        "version": MANIFEST_VERSION,
        "duration_ms": round(media_end_ms, 3),
        "declared_duration_ms": round(bundle.duration_s * 1000.0, 3),
        "truncated": truncated,
        "representations": [{"name": r.name, "target_bps": r.target_bps,
                             "width": r.width, "height": r.height}
                            for r in reps],
        "segments": seg_entries,
        "quality": quality,
        "alignment_error_ms": round(alignment_error_ms(results), 3),
        "audio_file": "audio_recovered.wav",
        "recovery": {
            "duplicates": vstats.duplicates + astats.duplicates,
            "late_packets": vstats.late_packets + astats.late_packets,
            "lost_packets": vstats.lost_packets + astats.lost_packets,
            "loss_bursts": vstats.loss_bursts + astats.loss_bursts,
            "video_freeze_frames": freeze,
            "video_unrecoverable_frames": unrecoverable,
            "audio_placeholder_chunks": missing_chunks,
            "audio_placeholder_ratio": round(missing_chunks / n_chunks, 4),
            "recovered_ms": round(media_end_ms, 1),
        },
        "clock": {"uncertainty_ms": round(domain.max_uncertainty_ms(), 1)},
        "pipeline": {"peak_queue_items": pstats.peak_queue_items,
                     "queue_cap": cfg.queue_size},
    }


def _write_and_summarize(manifest: dict, out_dir: Path) -> None:
    atomic_write_json(out_dir / "manifest.json", manifest)
    rec = manifest["recovery"]
    print(f"duration_ms: {manifest['duration_ms']}")
    print(f"alignment_error_ms: {manifest['alignment_error_ms']}")
    for rep in manifest["representations"]:
        name = rep["name"]
        segs = [s for s in manifest["segments"]
                if s["representation"] == name]
        actual = sum(s["actual_bps"] for s in segs) / max(len(segs), 1)
        err = sum(abs(s["bitrate_error_pct"]) for s in segs) \
            / max(len(segs), 1)
        q = manifest["quality"][name]
        print(f"[{name}] target_bps: {rep['target_bps']}")
        print(f"[{name}] actual_bps: {actual:.0f}")
        print(f"[{name}] bitrate_error_pct: {err:.2f}")
        print(f"[{name}] psnr_db: {q['psnr_db']}")
        print(f"[{name}] ssim: {q['ssim']}")
    for key in ("duplicates", "late_packets", "loss_bursts",
                "audio_placeholder_ratio"):
        print(f"{key}: {rec[key]}")
    print(f"peak_queue_items: {manifest['pipeline']['peak_queue_items']}")
    print(f"manifest: {out_dir / 'manifest.json'}")


MAX_DURATION_S = 600.0
MAX_TARGETS = 8
MAX_QUEUE = 4096
MAX_SEGMENT_MS = 60_000.0

CORRUPT_PRESETS = {
    "none": {},
    "duplicates": {"duplicate_prob": 0.15},
    "reorder-overflow": {"reorder_span": 128},
    "loss-burst": {"loss_burst_prob": 0.8},
    "rtcp-conflict": {"rtcp_conflict": True},
    "truncated": {"truncate_tail": 0.15},
    "empty": {"empty": True},
}


def _parse_targets(text: str) -> list[int]:
    try:
        targets = [int(t) for t in text.split(",") if t.strip()]
    except ValueError as exc:
        raise ParameterError(f"invalid --targets list: {text!r}") from exc
    if not 1 <= len(targets) <= MAX_TARGETS or any(t <= 0 for t in targets):
        raise ParameterError("targets must be 1..8 positive bitrates")
    return targets


def _check_finite(name: str, value: float, lo: float, hi: float) -> None:
    if not math.isfinite(value) or not lo <= value <= hi:
        raise ParameterError(f"{name} must be finite and within "
                             f"[{lo}, {hi}], got {value}")


def _cmd_demo(args) -> int:
    targets = _parse_targets(args.targets)
    _check_finite("--duration", args.duration, 0.5, MAX_DURATION_S)
    _check_finite("--loss-burst", args.loss_burst, 0.0, 10.0)
    _check_finite("--segment-ms", args.segment_ms, 250.0, MAX_SEGMENT_MS)
    if not 1 <= args.reorder_window <= 1024:
        raise ParameterError("--reorder-window must be within [1, 1024]")
    if not 1 <= args.queue_size <= MAX_QUEUE:
        raise ParameterError(f"--queue-size must be within [1, {MAX_QUEUE}]")
    params = SyntheticParams(
        duration_s=args.duration, seed=args.seed,
        loss_burst_prob=args.loss_burst, reorder_span=16,
        duplicate_prob=0.02,
        **CORRUPT_PRESETS[args.corrupt])
    if args.corrupt == "reorder-overflow":
        params.reorder_span = 2 * args.reorder_window
    bundle = generate(params)
    cfg = RecoverConfig(out_dir=Path(args.out), targets=targets,
                        reorder_window=args.reorder_window,
                        queue_size=args.queue_size,
                        segment_ms=args.segment_ms, fps=params.fps)
    manifest = recover(bundle, cfg)
    _write_and_summarize(manifest, Path(args.out))
    return 0


def _cmd_verify(args) -> int:
    path = Path(args.manifest)
    data = load_manifest(path)
    problems = validate_manifest(data, path.parent)
    print(f"segments: {len(data.get('segments', []))}")
    print(f"representations: {len(data.get('representations', []))}")
    print(f"duration_ms: {data.get('duration_ms', 0)}")
    ok = not problems
    print(f"manifest_ok: {str(ok).lower()}")
    for problem in problems:
        print(f"problem: {problem}")
    return 0 if ok else 1


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="archiveflow",
        description="RTP/RTCP archive recovery pipeline")
    sub = parser.add_subparsers(dest="command", required=True)
    demo = sub.add_parser("demo", help="generate a synthetic capture, "
                                       "recover it and write a manifest")
    demo.add_argument("--duration", type=float, default=24.0)
    demo.add_argument("--loss-burst", type=float, default=0.0,
                      help="loss bursts per second of media")
    demo.add_argument("--reorder-window", type=int, default=64)
    demo.add_argument("--targets", default="500000,1200000,2500000")
    demo.add_argument("--out", required=True)
    demo.add_argument("--segment-ms", type=float, default=4000.0)
    demo.add_argument("--queue-size", type=int, default=256)
    demo.add_argument("--seed", type=int, default=12772)
    demo.add_argument("--corrupt", choices=sorted(CORRUPT_PRESETS),
                      default="none", help="damaged-sample preset")
    demo.set_defaults(func=_cmd_demo)
    verify = sub.add_parser("verify", help="validate a written manifest")
    verify.add_argument("--manifest", required=True)
    verify.set_defaults(func=_cmd_verify)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return args.func(args)
    except ParameterError as exc:
        print(f"parameter error: {exc}", file=sys.stderr)
        return 2
    except (UnrecoverableError, CapabilityError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
