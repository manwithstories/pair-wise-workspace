"""Terminal table and JSON report rendering (first output path)."""

from __future__ import annotations

import json
import os

FIELD_ORDER = [
    "frames_decoded",
    "i_count",
    "p_count",
    "b_count",
    "ts_repaired",
    "max_jump_ms",
    "duration_ms",
    "warnings",
]


def _format_value(value) -> str:
    if isinstance(value, list):
        return "[" + ",".join(str(item) for item in value) + "]"
    return str(value)


def render_table(title: str, summary: dict, fields=None) -> str:
    fields = fields or FIELD_ORDER
    rows = [(name, _format_value(summary[name])) for name in fields if name in summary]
    width = max(len(name) for name, _ in rows) if rows else 0
    lines = [title]
    lines.extend(f"  {name:<{width}}  {value}" for name, value in rows)
    return "\n".join(lines)


def write_json(summary: dict, json_path: str) -> None:
    directory = os.path.dirname(os.path.abspath(json_path))
    os.makedirs(directory, exist_ok=True)
    with open(json_path, "w", encoding="utf-8") as handle:
        json.dump(summary, handle, indent=2)
        handle.write("\n")


def emit_report(summary: dict, input_path: str, json_path: str | None = None) -> str:
    table = render_table(f"plateqc report: {input_path}", summary)
    print(table)
    if json_path:
        write_json(summary, json_path)
    return table
