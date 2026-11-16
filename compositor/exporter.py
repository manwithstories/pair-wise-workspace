"""摘要导出器: 编排 renderer + audio + digest, 只落盘 JSONL。

硬约束:
- 像素合成只通过 :class:`~compositor.renderer.FrameRenderer` 完成,
  本模块不出现任何像素级运算;
- 不写 PNG/WAV/中间帧, 输出仅为 JSONL 帧摘要 + 一行全局摘要;
- 逐帧渲染、逐帧写入, 内存占用只随单帧画布像素线性增长。
"""

from __future__ import annotations

from pathlib import Path

from .audio import analyze_audio, frame_start_sample
from .digest import (
    build_frame_digest,
    frame_digest_jsonl,
    summary_json,
    summary_sha256,
)
from .renderer import FrameRenderer
from .types import (
    AudioEvent,
    DigestWriteError,
    FrameDigest,
    RenderSummary,
    Timeline,
    frame_to_second,
    us,
)


def _frame_sync_error(frame_index: int, timeline: Timeline,
                      events: list[AudioEvent]) -> int:
    """该帧起始时刻到帧内首个音频事件起点的偏差(微秒); 无事件则为 0。"""
    frame_start = frame_to_second(frame_index, timeline.fps)
    frame_end = frame_to_second(frame_index + 1, timeline.fps)
    for event in events:
        if frame_start <= event.start < frame_end:
            return us(abs(event.start - frame_start))
    return 0


def build_digests(timeline: Timeline) -> tuple[list[FrameDigest], list]:
    """渲染整条时间线并构造帧摘要 + 音频分析结果(纯内存, 不写盘)。"""
    renderer = FrameRenderer(timeline)
    audio_analysis = analyze_audio(timeline)
    digests: list[FrameDigest] = []
    for frame_index, frame in renderer.render_all():
        digest = build_frame_digest(
            frame=frame,
            frame_index=frame_index,
            audio_start_sample=frame_start_sample(frame_index, timeline),
            av_sync_error_us=_frame_sync_error(
                frame_index, timeline, timeline.audio_events),
        )
        digests.append(digest)
    return digests, audio_analysis


def build_summary(timeline: Timeline, digests: list[FrameDigest],
                  audio_analysis: list) -> RenderSummary:
    """汇总全局统计; 最大对齐误差取所有音频事件分析结果的最大值。"""
    max_error = max((a.av_sync_error_us for a in audio_analysis), default=0)
    return RenderSummary(
        frame_count=len(digests),
        width=timeline.width,
        height=timeline.height,
        fps=timeline.fps,
        sample_rate=timeline.sample_rate,
        summary_sha256=summary_sha256(digests),
        max_av_sync_error_us=max_error,
        total_opaque_pixels=sum(d.opaque_pixels for d in digests),
        total_transparent_pixels=sum(d.transparent_pixels for d in digests),
        audio_event_count=len(audio_analysis),
    )


def export_jsonl(timeline: Timeline, output_path: str | Path
                 ) -> tuple[RenderSummary, list]:
    """渲染并把帧摘要/全局摘要写入 JSONL, 返回 (全局摘要, 音频分析)。"""
    digests, audio_analysis = build_digests(timeline)
    summary = build_summary(timeline, digests, audio_analysis)
    path = Path(output_path)
    try:
        if path.parent and not path.parent.exists():
            path.parent.mkdir(parents=True, exist_ok=True)
        with path.open("w", encoding="utf-8", newline="\n") as handle:
            for digest in digests:
                handle.write(frame_digest_jsonl(digest))
                handle.write("\n")
            handle.write(summary_json(summary))
            handle.write("\n")
    except OSError as exc:
        raise DigestWriteError(f"摘要写入失败 {path}: {exc}") from exc
    return summary, audio_analysis


def preview_digests(timeline: Timeline) -> tuple[list[FrameDigest], RenderSummary, list]:
    """不落盘的预览入口(供测试与程序化调用复用同一套合成逻辑)。"""
    digests, audio_analysis = build_digests(timeline)
    summary = build_summary(timeline, digests, audio_analysis)
    return digests, summary, audio_analysis
