"""统一时间基准下的多轨合成与源块溯源。

职责：
    * 不同采样率块按线性插值重采样到主刻度（中心映射，规则见 timebase）；
    * 按清单显式 channel_map 放置声道，交叠区间确定性求和（增益恒为 1.0）；
    * 同 block_id + 同内容哈希幂等去重，同标识异内容则停止该轨道；
    * 坏块按 bad_block_policy 中止或填充精确 0.0 静音，溯源标记 filled；
    * 每个输出区间记录块哈希、采样区间、重采样/舍入信息与参数指纹。
"""

from dataclasses import dataclass, field
import hashlib
import json
from typing import Callable

import numpy as np

from .errors import MediaError
from .manifest import Manifest, TrackSpec, BlockSpec, POLICY_SILENCE
from .timebase import TimeBase, RESAMPLE_METHOD, FILL_VALUE
from .wav_reader import BlockData, read_block

BlockLoader = Callable[[BlockSpec], BlockData]


@dataclass(frozen=True)
class ProvenanceSpan:
    """一个源块在主时间线上的溯源区间（端点均为主刻度，左闭右开）。"""

    track: str
    block_id: str
    content_hash: str
    master_start: int
    master_end: int
    src_start: int                 # 源块内采样帧区间（左闭右开）
    src_end: int
    filled: bool                   # True 表示静音填充区间
    resample_method: str
    src_rate: int
    rounding_error_ticks: float    # 帧/刻度换算的舍入差异（tick）
    fill_reason: str | None = None
    byte_offset: int | None = None


@dataclass(frozen=True)
class TrackError:
    track: str
    block_id: str
    order: int
    start_tick: int
    error_code: str
    message: str
    byte_offset: int | None = None
    content_hash: str | None = None


@dataclass
class Timeline:
    manifest: Manifest
    timebase: TimeBase
    buffer: np.ndarray                       # (output_length, output_channels) float64
    spans: list[ProvenanceSpan] = field(default_factory=list)
    track_errors: list[TrackError] = field(default_factory=list)
    deduped_blocks: list[str] = field(default_factory=list)
    filled_spans: list[ProvenanceSpan] = field(default_factory=list)

    def spans_for_tick(self, tick: int) -> list[ProvenanceSpan]:
        """整数刻度查找：返回覆盖该 tick 的全部溯源区间。"""
        return [s for s in self.spans if s.master_start <= tick < s.master_end]


def parameter_fingerprint(manifest: Manifest) -> str:
    """清单参数 + 算法版本的稳定指纹（SHA-256, 64 hex）。"""
    payload = {
        "algorithm": "relaymix-algo-1.0.0",
        "resample": RESAMPLE_METHOD,
        "fill_value": FILL_VALUE,
        "master_rate": manifest.master_rate,
        "output_start": manifest.output_start,
        "output_end": manifest.output_end,
        "frame_size": manifest.frame_size,
        "output_channels": manifest.output_channels,
        "bad_block_policy": manifest.bad_block_policy,
        "tracks": [
            {
                "name": t.name,
                "channel_map": list(t.channel_map),
                "blocks": [
                    {
                        "block_id": b.block_id,
                        "path": b.path,
                        "start_tick": b.start_tick,
                        "length_ticks": b.length_ticks,
                        "pcm_rate": b.pcm_rate,
                        "pcm_channels": b.pcm_channels,
                        "pcm_width": b.pcm_width,
                    }
                    for b in t.blocks
                ],
            }
            for t in manifest.tracks
        ],
    }
    blob = json.dumps(payload, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(blob).hexdigest()


def resample_linear(samples: np.ndarray, target_frames: int) -> np.ndarray:
    """线性插值重采样，采样中心映射；目标帧数等于源时直接复制。"""
    x = np.asarray(samples, dtype=np.float64)
    src_frames = x.shape[0]
    if target_frames == src_frames:
        return x.copy()
    if target_frames <= 0:
        raise ValueError("target_frames must be positive")
    pos = (np.arange(target_frames, dtype=np.float64) + 0.5) * (
        src_frames / target_frames
    ) - 0.5
    i0 = np.floor(pos).astype(np.int64)
    frac = (pos - i0)[:, None]
    left_idx = np.clip(i0, 0, src_frames - 1)
    right_idx = np.clip(i0 + 1, 0, src_frames - 1)
    return x[left_idx] * (1.0 - frac) + x[right_idx] * frac


def _default_loader(spec: BlockSpec) -> BlockData:
    return read_block(
        spec.path, spec.block_id,
        pcm_rate=spec.pcm_rate, pcm_channels=spec.pcm_channels, pcm_width=spec.pcm_width,
    )


def _apply_channel_map(track: TrackSpec, resampled: np.ndarray,
                       out_channels: int) -> np.ndarray:
    """按显式映射把 (N, src_ch) 摊到 (N, out_channels)；-1 表示丢弃。"""
    if resampled.shape[1] != len(track.channel_map):
        raise MediaError(
            f"channel count {resampled.shape[1]} != mapping {len(track.channel_map)}",
            error_code="unsupported_layout",
            track=track.name,
        )
    placed = np.zeros((resampled.shape[0], out_channels), dtype=np.float64)
    for src_ch, dst_ch in enumerate(track.channel_map):
        if dst_ch >= 0:
            placed[:, dst_ch] += resampled[:, src_ch]
    return placed


def build_timeline(manifest: Manifest, loader: BlockLoader | None = None) -> Timeline:
    """按清单合成主时间线；loader 可注入以便测试与恢复流程复用。"""
    loader = loader or _default_loader
    tb = TimeBase(manifest.master_rate)
    buffer = np.zeros(
        (manifest.output_length, manifest.output_channels), dtype=np.float64
    )
    timeline = Timeline(manifest=manifest, timebase=tb, buffer=buffer)
    # block_id -> content_hash：跨轨道全局去重注册表。
    registry: dict[str, str] = {}
    stopped: set[str] = set()

    for track in manifest.tracks:
        for spec in track.blocks:
            if track.name in stopped:
                continue
            rel_start = spec.start_tick - manifest.output_start
            if rel_start < 0 or rel_start + spec.length_ticks > manifest.output_length:
                raise MediaError(
                    "block placement outside output range",
                    error_code="range_out_of_bounds",
                    source_block=spec.block_id,
                    track=track.name,
                )
            try:
                block = loader(spec)
            except MediaError as exc:
                err = exc.to_dict()
                if manifest.bad_block_policy == POLICY_SILENCE:
                    span = _fill_silence(
                        timeline, track, spec, rel_start,
                        err.get("error_code", "media_error"),
                        err.get("byte_offset"),
                        err.get("content_hash"),
                    )
                    timeline.track_errors.append(
                        TrackError(
                            track.name, spec.block_id, spec.order, spec.start_tick,
                            err.get("error_code", "media_error"), str(exc),
                            err.get("byte_offset"), err.get("content_hash"),
                        )
                    )
                    continue
                raise
            # 幂等去重：同标识同内容只计入一次；异内容停止该轨道。
            if spec.block_id in registry:
                if registry[spec.block_id] == block.content_hash:
                    timeline.deduped_blocks.append(spec.block_id)
                    continue
                timeline.track_errors.append(
                    TrackError(
                        track.name, spec.block_id, spec.order, spec.start_tick,
                        "block_content_conflict",
                        "same block_id delivered with different content hash",
                        None, block.content_hash,
                    )
                )
                stopped.add(track.name)
                continue
            registry[spec.block_id] = block.content_hash
            _place_block(timeline, track, spec, block, rel_start)
    return timeline


def _fill_silence(timeline: Timeline, track: TrackSpec, spec: BlockSpec,
                  rel_start: int, reason: str, byte_offset: int | None,
                  content_hash: str | None) -> ProvenanceSpan:
    n = spec.length_ticks
    # 静音同样走声道映射，保证布局与正常块一致。
    zeros = np.zeros((n, len(track.channel_map)), dtype=np.float64)
    placed = _apply_channel_map(track, zeros, timeline.manifest.output_channels)
    timeline.buffer[rel_start:rel_start + n] += placed
    span = ProvenanceSpan(
        track=track.name, block_id=spec.block_id,
        content_hash=content_hash or "0" * 64,
        master_start=spec.start_tick, master_end=spec.start_tick + n,
        src_start=0, src_end=0, filled=True,
        resample_method="none", src_rate=0, rounding_error_ticks=0.0,
        fill_reason=reason, byte_offset=byte_offset,
    )
    timeline.spans.append(span)
    timeline.filled_spans.append(span)
    return span


def _place_block(timeline: Timeline, track: TrackSpec, spec: BlockSpec,
                 block: BlockData, rel_start: int) -> None:
    tb = timeline.timebase
    target = spec.length_ticks
    if block.channels != len(track.channel_map):
        raise MediaError(
            f"channel count {block.channels} != mapping {len(track.channel_map)}",
            error_code="unsupported_layout",
            track=track.name, source_block=spec.block_id,
        )
    resampled = resample_linear(block.samples, target)
    expected_src = tb.to_source_frames(target, block.sample_rate)
    rounding_error = abs(float(expected_src - block.actual_frames))
    placed = _apply_channel_map(
        track, resampled, timeline.manifest.output_channels
    )
    timeline.buffer[rel_start:rel_start + target] += placed
    timeline.spans.append(
        ProvenanceSpan(
            track=track.name, block_id=spec.block_id,
            content_hash=block.content_hash,
            master_start=spec.start_tick, master_end=spec.start_tick + target,
            src_start=0, src_end=block.actual_frames, filled=False,
            resample_method=RESAMPLE_METHOD if block.sample_rate != tb.rate else "none",
            src_rate=block.sample_rate, rounding_error_ticks=rounding_error,
        )
    )
