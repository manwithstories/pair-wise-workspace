"""帧与全局摘要: 像素 SHA256、透明像素统计、汇总哈希。

全部输入为 renderer 产出的 uint8 RGBA 数组, 本模块只做哈希与统计,
不参与任何像素合成逻辑(合成统一走 renderer/blend)。
"""

from __future__ import annotations

import hashlib

import numpy as np

from .types import FrameDigest, RenderSummary


def pixel_sha256(frame: np.ndarray) -> str:
    """对一帧的连续 RGBA 字节计算 SHA256, 返回 64 位十六进制字符串。"""
    if frame.dtype != np.uint8:
        raise ValueError("pixel_sha256 只接受 uint8 帧数据")
    return hashlib.sha256(np.ascontiguousarray(frame).tobytes()).hexdigest()


def count_alpha(frame: np.ndarray) -> tuple[int, int]:
    """返回 (不透明像素数, 全透明像素数); 部分透明不计入任一侧。"""
    alpha = frame[..., 3]
    opaque = int(np.count_nonzero(alpha == 255))
    transparent = int(np.count_nonzero(alpha == 0))
    return opaque, transparent


def build_frame_digest(frame: np.ndarray, frame_index: int,
                       audio_start_sample: int, av_sync_error_us: int
                       ) -> FrameDigest:
    """汇总单帧的像素哈希、透明统计与音频对齐信息。"""
    opaque, transparent = count_alpha(frame)
    return FrameDigest(
        frame_index=frame_index,
        pixel_sha256=pixel_sha256(frame),
        opaque_pixels=opaque,
        transparent_pixels=transparent,
        audio_start_sample=audio_start_sample,
        av_sync_error_us=av_sync_error_us,
    )


def summary_sha256(frame_digests: list[FrameDigest]) -> str:
    """对全部帧摘要的规范 JSON 行做链式哈希, 得到全局确定性摘要。"""
    hasher = hashlib.sha256()
    for digest in frame_digests:
        line = frame_digest_jsonl(digest)
        hasher.update(line.encode("utf-8"))
        hasher.update(b"\n")
    return hasher.hexdigest()


def frame_digest_jsonl(digest: FrameDigest) -> str:
    """单帧摘要的规范 JSONL 行(键顺序固定, 无空格, 保证哈希可重复)。"""
    import json

    return json.dumps(
        {
            "kind": "frame",
            "frame_index": digest.frame_index,
            "pixel_sha256": digest.pixel_sha256,
            "opaque_pixels": digest.opaque_pixels,
            "transparent_pixels": digest.transparent_pixels,
            "audio_start_sample": digest.audio_start_sample,
            "av_sync_error_us": digest.av_sync_error_us,
        },
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
    )


def summary_json(summary: RenderSummary) -> str:
    """全局摘要的规范 JSON 行。"""
    import json

    return json.dumps(
        {
            "kind": "summary",
            "frame_count": summary.frame_count,
            "width": summary.width,
            "height": summary.height,
            "fps": summary.fps,
            "sample_rate": summary.sample_rate,
            "summary_sha256": summary.summary_sha256,
            "max_av_sync_error_us": summary.max_av_sync_error_us,
            "total_opaque_pixels": summary.total_opaque_pixels,
            "total_transparent_pixels": summary.total_transparent_pixels,
            "audio_event_count": summary.audio_event_count,
        },
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
    )
