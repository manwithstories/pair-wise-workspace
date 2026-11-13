"""Terminal quantified table and JSON-line summaries."""
from __future__ import annotations

import json

from .tl_parse import Cue

_HEADERS = ("cue", "track", "final(ms)", "resolved", "moved", "state", "sha256")


def _row(cue: Cue, digest: str | None) -> tuple[str, ...]:
    interval = f"[{cue.final_start},{cue.final_end}]"
    return (
        cue.id,
        str(cue.track),
        interval,
        str(cue.resolved),
        str(cue.moved),
        "drop" if cue.dropped else "keep",
        digest if digest else "-",
    )


def print_table(cues: list[Cue], hashes: dict[str, str]) -> str:
    rows = [_row(cue, hashes.get(cue.id)) for cue in cues]
    widths = [
        max(len(header), *(len(row[i]) for row in rows))
        for i, header in enumerate(_HEADERS)
    ]
    lines = ["  ".join(h.ljust(widths[i]) for i, h in enumerate(_HEADERS))]
    lines.append("  ".join("-" * w for w in widths))
    for row in rows:
        lines.append("  ".join(row[i].ljust(widths[i]) for i in range(len(row))))
    table = "\n".join(lines)
    print(table)
    return table


def json_lines(cues: list[Cue], hashes: dict[str, str]) -> list[str]:
    lines = []
    for cue in cues:
        payload = {
            "cue": cue.id,
            "track": cue.track,
            "final": [cue.final_start, cue.final_end],
            "resolved": cue.resolved,
            "sha256": hashes.get(cue.id),
        }
        if cue.dropped:
            payload["dropped"] = True
        line = json.dumps(payload, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
        print(line)
        lines.append(line)
    return lines


def summary_line(summary: dict) -> str:
    line = (
        f"total {summary['total']}, resolved {summary['resolved']}, "
        f"dropped {summary['dropped']}, frames {summary['frames']}, "
        f"hash_mismatch {summary['hash_mismatch']}"
    )
    print(line)
    return line
