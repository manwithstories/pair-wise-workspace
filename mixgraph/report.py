"""JSON report creation and concise terminal summaries."""
from __future__ import annotations

import json
from pathlib import Path
import sys
from typing import Any, TextIO



class ReportError(OSError):
    """Raised when a report cannot be written."""


def build_report(metrics: dict[str, Any]) -> dict[str, Any]:
    return {
        "tracks": [metric.as_dict() for metric in metrics["tracks"]],
        "buses": [metric.as_dict() for metric in metrics["buses"]],
        "master": metrics["master"].as_dict(),
    }


def write_report(path: str | Path, report: dict[str, Any]) -> None:
    output = Path(path)
    output.parent.mkdir(parents=True, exist_ok=True)
    try:
        with output.open("w", encoding="utf-8") as handle:
            json.dump(report, handle, indent=2, ensure_ascii=False)
            handle.write("\n")
    except OSError as exc:
        raise ReportError(f"cannot write report '{output}': {exc}") from exc


def _row(metric: dict[str, Any]) -> str:
    return (
        f"{metric['name']:<16} "
        f"{metric['peak_dbfs']:>9.3f} "
        f"{metric['rms_dbfs']:>9.3f} "
        f"{metric['clip_samples']:>8d}"
    )


def print_summary(report: dict[str, Any], stream: TextIO = sys.stdout) -> None:
    print(f"{'node':<16} {'peak dBFS':>9} {'rms dBFS':>9} {'clips':>8}", file=stream)
    for section in ("tracks", "buses"):
        for metric in report[section]:
            print(_row(metric), file=stream)
    print(_row(report["master"]), file=stream)

