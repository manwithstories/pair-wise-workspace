"""清单：对齐清单生成、码率摘要与复核校验。"""

from __future__ import annotations

import json
from pathlib import Path

from .checkpoint import file_digest
from .errors import CheckpointError, ManifestError, MetricsExceededError

BITRATE_DEVIATION_MAX_PCT = 8.0
ALIGNMENT_ERROR_MAX_MS = 1.0

REQUIRED_STATS = (
    "packet_duplicate_count",
    "packet_reorder_count",
    "burst_loss_count",
    "bitrate_target_bps",
    "bitrate_actual_bps",
    "bitrate_deviation_pct",
    "frame_count",
    "frame_psnr_mean_db",
    "audio_rms",
    "manifest_alignment_error_ms",
    "checkpoint_reused_blocks",
)


def build_manifest(task_id: str, ticks_per_second: int, fps: int,
                   chunks: list[dict], frames: list[dict],
                   stats: dict) -> dict:
    return {
        "version": 1,
        "task_id": task_id,
        "timebase_ticks_per_second": ticks_per_second,
        "fps": fps,
        "frame_count": len(frames),
        "chunks": chunks,
        "frames": frames,
        "stats": stats,
    }


def write_manifest(path: Path, manifest: dict) -> None:
    path = Path(path)
    tmp = path.with_suffix(".json.part")
    tmp.write_text(json.dumps(manifest, indent=2, sort_keys=True))
    tmp.replace(path)


def load_manifest(path: Path) -> dict:
    path = Path(path)
    if not path.exists():
        raise ManifestError(f"清单不存在: {path}")
    try:
        data = json.loads(path.read_text())
    except ValueError as exc:
        raise ManifestError(f"清单不是合法 JSON: {exc}")
    if not isinstance(data, dict):
        raise ManifestError("清单顶层必须为对象")
    for key in ("task_id", "chunks", "frames", "stats", "timebase_ticks_per_second"):
        if key not in data:
            raise ManifestError(f"清单缺少字段: {key}")
    missing = [k for k in REQUIRED_STATS if k not in data["stats"]]
    if missing:
        raise ManifestError(f"清单统计缺少字段: {missing}")
    return data


def verify_manifest(path: Path) -> dict:
    """复核清单：结构 → 输出哈希 → 验收指标。返回统计摘要。"""
    manifest = load_manifest(path)
    out_dir = Path(path).parent

    corrupt: list[int] = []
    for chunk in manifest["chunks"]:
        target = out_dir / chunk["file"]
        if not target.exists() or file_digest(target) != chunk["output_hash"]:
            corrupt.append(chunk["index"])
    if corrupt:
        raise CheckpointError(
            f"清单校验发现损坏块: {corrupt}", conflict_blocks=corrupt
        )

    stats = manifest["stats"]
    over: list[str] = []
    if stats["bitrate_deviation_pct"] > BITRATE_DEVIATION_MAX_PCT:
        over.append(
            f"码率偏差 {stats['bitrate_deviation_pct']:.2f}% "
            f"> {BITRATE_DEVIATION_MAX_PCT}%"
        )
    if stats["manifest_alignment_error_ms"] > ALIGNMENT_ERROR_MAX_MS:
        over.append(
            f"对齐误差 {stats['manifest_alignment_error_ms']:.3f}ms "
            f"> {ALIGNMENT_ERROR_MAX_MS}ms"
        )
    if over:
        raise MetricsExceededError("验收指标超限: " + "; ".join(over))
    return stats
