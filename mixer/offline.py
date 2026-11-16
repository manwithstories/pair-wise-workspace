"""离线/流式执行编排与 WAV、JSON 落盘。

两条路径共用 BlockEngine:
- offline:默认大块(或配置 block_frames),FileSource 惰性读取;
- stream:  调用方指定小块,源经 PrefetchSource 有界预取,内存占用恒定。

错误策略:
- fail_fast:  任一轨道媒体错误立即抛出 MediaError(退出码 3);
- best_effort:跳过坏轨道,在报告 skipped_tracks 记录 code/reason,
              其余轨道照常混音。
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Optional

import numpy as np
import soundfile as sf

from .metrics import to_dbfs
from .sources import FileSource, PrefetchSource, StreamSource
from .stream import BlockEngine, EngineResult
from .types import (
    ErrorPolicy,
    MediaError,
    OutputError,
    ProjectConfig,
    StatsResult,
    TrackSpec,
    TrackStat,
)


def build_sources(
    config: ProjectConfig,
    base_dir: Path,
    prefetch: bool,
) -> tuple[dict[str, StreamSource], list[TrackStat]]:
    """按配置打开全部轨道源,best-effort 下把坏轨记入跳过列表。"""

    sources: dict[str, StreamSource] = {}
    skipped: list[TrackStat] = []
    for spec in config.tracks:
        path = Path(spec.path)
        if not path.is_absolute():
            path = base_dir / path
        try:
            source: StreamSource = FileSource(path)
            if prefetch:
                source = PrefetchSource(source)
        except MediaError as exc:
            if config.error_policy is ErrorPolicy.FAIL_FAST:
                raise
            skipped.append(
                TrackStat(
                    name=spec.name,
                    error=exc.code,
                    reason=exc.details.get("reason", "unknown"),
                )
            )
            continue
        sources[spec.name] = source
    return sources, skipped


def _build_stats(
    config: ProjectConfig,
    result: EngineResult,
    mode: str,
    block_frames: int,
    extra_skipped: list[TrackStat],
) -> StatsResult:
    snap = result.master.snapshot()
    skipped = list(result.skipped) + list(extra_skipped)
    return StatsResult(
        sample_rate=config.sample_rate,
        channels=2,
        frames=result.project_frames,
        tracks=len(config.tracks),
        automation_samples=result.automation_samples,
        peak_dbfs=snap["peak_dbfs"],
        rms_dbfs=snap["rms_dbfs"],
        effective_rms_dbfs=snap["effective_rms_dbfs"],
        clipped_samples=snap["clipped_samples"],
        clip_threshold=config.clip_threshold,
        mode=mode,
        block_frames=block_frames,
        error_policy=config.error_policy.value,
        track_stats=result.track_stats,
        skipped_tracks=skipped,
    )


def write_wav(path: str | Path, signal: np.ndarray, sample_rate: int) -> None:
    """写 32-bit float WAV;任何 IO 失败统一成 OutputError(退出码 4)。"""

    p = Path(path)
    try:
        p.parent.mkdir(parents=True, exist_ok=True)
        sf.write(str(p), signal, sample_rate, subtype="FLOAT")
    except (OSError, ValueError, sf.LibsndfileError) as exc:
        raise OutputError(
            f"输出 WAV 写入失败: {p}: {exc}",
            details={"path": str(p), "reason": "write_failed"},
        ) from exc


def write_report(path: str | Path, stats: StatsResult) -> None:
    """写 JSON 统计报告;失败不留半成品成功结果。"""

    p = Path(path)
    try:
        p.parent.mkdir(parents=True, exist_ok=True)
        with p.open("w", encoding="utf-8") as fh:
            json.dump(stats.to_dict(), fh, ensure_ascii=False, indent=2)
    except OSError as exc:
        raise OutputError(
            f"统计报告写入失败: {p}: {exc}",
            details={"path": str(p), "reason": "write_failed"},
        ) from exc


def run_mix(
    config: ProjectConfig,
    config_path: str | Path,
    output_wav: str | Path,
    report_path: str | Path,
    mode: str,
    block_frames: Optional[int] = None,
) -> tuple[np.ndarray, StatsResult]:
    """执行完整混音并落盘,返回 (总线信号, 统计)。mode 仅用于报告标注。"""

    if mode not in ("offline", "stream"):
        raise OutputError(f"未知执行模式: {mode}")
    block = block_frames or config.block_frames
    if block <= 0:
        raise OutputError("block_frames 必须为正整数")

    base_dir = Path(config_path).resolve().parent
    prefetch = mode == "stream"
    sources, open_skipped = build_sources(config, base_dir, prefetch=prefetch)
    engine = BlockEngine(config, sources)
    try:
        result = engine.run(block)
        signal = np.concatenate([mb.buffer for mb in result.blocks], axis=0)
    finally:
        engine.close()

    stats = _build_stats(config, result, mode, block, open_skipped)
    write_wav(output_wav, signal, config.sample_rate)
    write_report(report_path, stats)
    return signal, stats
