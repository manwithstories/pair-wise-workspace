"""JSON summary report.

The report is the operator-facing contract.  It always contains the five
values the acceptance command greps for:

``damage_offset_frames``
    how far, in frames, our first-fault location could be from the true
    first anomaly (``<= 1``).
``recovery_seconds``
    wall-clock time spent resuming, excluding cold-start work that would
    have happened on a full run anyway.
``reused_nodes``
    nodes taken from the checkpoint instead of recomputed.
``output_sha256``
    hash of the assembled output (64 lowercase hex characters).
``timeline_drift_us``
    worst microsecond rounding residual across all timestamp conversions.

The rest is structured detail: per-track scans, damage kinds, node states,
audio loudness.  Everything is plain JSON (no NaN/Infinity — those are
rendered as ``null`` with a separate boolean) so consumers need no special
parser.
"""

from __future__ import annotations

import json
import math
import os
import tempfile
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .errors import MediaError

__all__ = ["Report", "SCHEMA_VERSION"]

SCHEMA_VERSION = 1


def _clean(value: Any) -> Any:
    """Make a value strictly JSON-serialisable (no NaN/Infinity)."""
    if isinstance(value, float):
        if math.isnan(value) or math.isinf(value):
            return None
        return value
    if isinstance(value, dict):
        return {str(k): _clean(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [_clean(v) for v in value]
    if isinstance(value, MediaError):
        return value.to_dict()
    if isinstance(value, Path):
        return str(value)
    if isinstance(value, (str, int, bool)) or value is None:
        return value
    return str(value)


@dataclass
class Report:
    """Accumulates summary data, then serialises it."""

    command: str = ""
    workdir: str = ""
    status: str = "ok"  # ok | recovered | failed
    exit_code: int = 0
    settings: dict[str, Any] = field(default_factory=dict)
    scans: dict[str, Any] = field(default_factory=dict)
    damage: dict[str, Any] = field(default_factory=dict)
    graph: dict[str, Any] = field(default_factory=dict)
    audio: dict[str, Any] = field(default_factory=dict)
    recovery: dict[str, Any] = field(default_factory=dict)
    errors: list[MediaError] = field(default_factory=list)
    phase_timings: dict[str, float] = field(default_factory=dict)

    # -- headline numbers ---------------------------------------------------
    def damage_offset_frames(self) -> int:
        fault = self.damage.get("first_fault") or {}
        context = fault.get("context") or {}
        return int(context.get("locator_error_frames", 0) or 0)

    def recovery_seconds(self) -> float:
        return float(self.recovery.get("recovery_seconds", 0.0))

    def reused_nodes(self) -> int:
        return int(self.recovery.get("reused_nodes", 0))

    def output_sha256(self) -> str:
        return str(self.recovery.get("output_sha256", ""))

    def timeline_drift_us(self) -> int:
        worst = 0
        for scan in self.scans.values():
            drift = scan.get("drift", {}).get("max_abs_us", 0)
            worst = max(worst, int(drift or 0))
        return worst

    # -- serialisation ------------------------------------------------------
    def to_dict(self) -> dict[str, Any]:
        headline = {
            "damage_offset_frames": self.damage_offset_frames(),
            "recovery_seconds": round(self.recovery_seconds(), 6),
            "reused_nodes": self.reused_nodes(),
            "output_sha256": self.output_sha256(),
            "timeline_drift_us": self.timeline_drift_us(),
        }
        return _clean(
            {
                "schema": SCHEMA_VERSION,
                "command": self.command,
                "workdir": self.workdir,
                "status": self.status,
                "exit_code": self.exit_code,
                **headline,
                "settings": self.settings,
                "scans": self.scans,
                "damage": self.damage,
                "graph": self.graph,
                "audio": self.audio,
                "recovery": self.recovery,
                "errors": [e.to_dict() for e in self.errors],
                "phase_timings": self.phase_timings,
            }
        )

    def to_json(self) -> str:
        return json.dumps(self.to_dict(), indent=2, sort_keys=True) + "\n"

    def write(self, path: str | Path) -> Path:
        """Atomically write the report; a killed run never leaves half a JSON."""
        path = Path(path)
        path.parent.mkdir(parents=True, exist_ok=True)
        fd, tmp = tempfile.mkstemp(prefix=".report-", dir=str(path.parent))
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as fh:
                fh.write(self.to_json())
                fh.flush()
                os.fsync(fh.fileno())
            os.replace(tmp, path)
        except BaseException:
            try:
                os.unlink(tmp)
            except OSError:
                pass
            raise
        return path

    @classmethod
    def load(cls, path: str | Path) -> dict[str, Any]:
        return json.loads(Path(path).read_text(encoding="utf-8"))
