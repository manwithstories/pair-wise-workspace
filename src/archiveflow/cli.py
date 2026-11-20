"""Batch acceptance CLI and recovery orchestration.

Data flow: synthetic/captured packets -> RTP/RTCP parsing -> clock
normalisation -> reorder/repair -> bounded-queue pipeline -> decode with
freeze-frame fill -> aligned multi-bitrate segmentation -> JSON manifest
and statistics.
"""

from __future__ import annotations

import math
from dataclasses import dataclass
from pathlib import Path

import av
import numpy as np
import soundfile as sf

from .clock import ClockDomain
from .errors import CapabilityError, UnrecoverableError
from .ladder import (SegmentResult, alignment_error_ms, build_ladder,
                     encode_segment, select_representations,
                     simulate_bandwidth)
from .manifest import (MANIFEST_VERSION, atomic_write_json,
                       safe_segment_name)
from .metrics import bitrate_error_pct, psnr_db, ssim, summarize_quality
from .repair import FrameReassembler, RepairStats, ReorderBuffer
from .rtp import (RTCP_PT_SR, PacketOrganizer, parse_rtcp_sr, parse_rtp)
from .segment import plan_segments
from .stream import Pipeline
from .synthetic import Bundle, synth_frame

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
