"""Terminal table and JSON report rendering."""
from __future__ import annotations

import json

RUNG_COLUMNS = (
    ("rung", "rung", 5),
    ("resolution", "resolution", 11),
    ("fps", "fps", 5),
    ("qp", "qp", 4),
    ("kbps", "actual_kbps", 9),
    ("psnr_db", "psnr_db", 8),
    ("ssim", "ssim", 7),
    ("bytes", "bytes", 10),
    ("duration_s", "duration_s", 10),
)


def _format(value) -> str:
    if isinstance(value, float):
        return f"{value:.4f}".rstrip("0").rstrip(".")
    return str(value)


def render_ladder_table(rungs: list[dict]) -> str:
    header = "  ".join(label.ljust(width) for label, _, width in RUNG_COLUMNS)
    lines = [header, "-" * len(header)]
    for index, rung in enumerate(rungs, start=1):
        row = dict(rung)
        row.setdefault("resolution", f"{row.get('width', '?')}x{row.get('height', '?')}")
        row["rung"] = index
        cells = []
        for _, key, width in RUNG_COLUMNS:
            cells.append(_format(row.get(key, "")).ljust(width))
        lines.append("  ".join(cells))
    return "\n".join(lines)


def render_verify_table(rows: list[dict]) -> str:
    columns = (
        ("rung", 5), ("resolution", 11), ("kbps", 9), ("psnr_db", 9),
        ("bitrate+", 10), ("quality>=", 10), ("ratio_ok", 10),
    )
    header = "  ".join(label.ljust(width) for label, width in columns)
    lines = [header, "-" * len(header)]
    for index, row in enumerate(rows, start=1):
        checks = row["monotonic"]
        values = (
            index,
            f"{row['width']}x{row['height']}",
            f"{row['actual_kbps']:.2f}",
            f"{row['psnr_db']:.2f}",
            checks["bitrate_strictly_increasing"],
            checks["quality_nondecreasing"],
            checks["ratio_in_range"],
        )
        lines.append("  ".join(str(value).ljust(width) for value, (_, width) in zip(values, columns)))
    return "\n".join(lines)


def render_stats(stats: dict) -> str:
    order = ("tasks_total", "done", "resumed", "orphaned_reclaimed",
             "duplicates_collapsed", "failed", "interrupted")
    return " ".join(f"{key}={stats[key]}" for key in order if key in stats)


def to_json(payload: dict) -> str:
    return json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=False)
