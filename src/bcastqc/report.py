"""Per-asset diagnostics and the batch summary.

The JSON contract is what ``bcastqc verify`` gates on, so field names are
stable: ``sync_error_ms``, ``recovery_rate``, ``p95_latency_ms``,
``remux_packet_match_rate`` and ``damaged_samples``.  Numbers are rounded once,
at serialisation time, from exact rational/​float values kept in the dataclasses.
"""

from __future__ import annotations

import json
import math
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable

from .errors import EXIT_MEDIA, EXIT_OK, EXIT_THRESHOLD, QCError, errors_to_dict

STATUS_OK = "ok"
STATUS_RECOVERED = "recovered"
STATUS_PARTIAL = "partial"
STATUS_REFUSED = "refused"
STATUS_FAILED = "failed"

DAMAGED_STATUSES = {STATUS_RECOVERED, STATUS_PARTIAL}

# Default acceptance thresholds -- also the ones documented in the README and
# enforced by ``bcastqc verify``.
THRESHOLDS: dict[str, float] = {
    "sync_error_ms": 20.0,
    "recovery_rate": 0.80,
    "p95_latency_ms": 3000.0,
    "remux_packet_match_rate": 0.99,
    "damaged_samples": 3.0,
}


@dataclass(slots=True)
class AssetResult:
    asset: Path
    strategy: str
    status: str
    duration_s: float | None = None
    tracks: int = 0
    output: Path | None = None
    output_bytes: int = 0
    copied_packets: int = 0
    expected_packets: int = 0
    recovery_rate: float | None = None
    remux_packet_match_rate: float | None = None
    sync_error_ms: float | None = None
    latency_ms: float = 0.0
    codec_pairs: list[str] = field(default_factory=list)
    strategies: list[str] = field(default_factory=list)
    warnings: list[QCError] = field(default_factory=list)
    errors: list[QCError] = field(default_factory=list)
    reasons: list[str] = field(default_factory=list)
    mapping: list[str] = field(default_factory=list)
    cache: str | None = None
    extra: dict[str, Any] = field(default_factory=dict)

    @property
    def damaged(self) -> bool:
        return self.status in DAMAGED_STATUSES

    @property
    def ok(self) -> bool:
        return self.status in {STATUS_OK, STATUS_RECOVERED, STATUS_PARTIAL, STATUS_REFUSED}

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "asset": str(self.asset),
            "status": self.status,
            "strategy": self.strategy,
            "duration_s": _round(self.duration_s),
            "tracks": self.tracks,
            "output": None if self.output is None else str(self.output),
            "output_bytes": self.output_bytes,
            "copied_packets": self.copied_packets,
            "expected_packets": self.expected_packets,
            "recovery_rate": _round(self.recovery_rate),
            "remux_packet_match_rate": _round(self.remux_packet_match_rate),
            "sync_error_ms": _round(self.sync_error_ms),
            "latency_ms": _round(self.latency_ms),
            "codec_pairs": list(self.codec_pairs),
            "strategies": list(self.strategies),
            "mapping": list(self.mapping),
            "damaged": self.damaged,
            "cache": self.cache,
            "warnings": errors_to_dict(self.warnings),
            "errors": errors_to_dict(self.errors),
            "reasons": list(self.reasons),
        }
        payload.update(self.extra)
        return payload


@dataclass(slots=True)
class BatchReport:
    workdir: Path
    results: list[AssetResult]
    started_at: str
    finished_at: str
    fatal_assets: int = 0
    scenario: str = "process"

    @property
    def successful(self) -> list[AssetResult]:
        return [r for r in self.results if r.status in {STATUS_OK, STATUS_RECOVERED}]

    @property
    def damaged_samples(self) -> list[AssetResult]:
        return [r for r in self.results if r.damaged]

    @property
    def refused_samples(self) -> list[AssetResult]:
        return [r for r in self.results if r.status == STATUS_REFUSED]

    def measured_sync_error_ms(self) -> float | None:
        """Worst measured sync error over the whole batch, refused included.

        A refused asset is still measured (it was decoded for markers before
        the copy decision); leaving it out would let a bad refusal hide a real
        synchronization fault.
        """
        values = [r.sync_error_ms for r in self.results if r.sync_error_ms is not None]
        return max(values) if values else None

    @property
    def failed_samples(self) -> list[AssetResult]:
        return [r for r in self.results if r.status == STATUS_FAILED]

    @property
    def partial_samples(self) -> list[AssetResult]:
        return [r for r in self.results if r.status == STATUS_PARTIAL]

    def sync_error_ms(self) -> float | None:
        """Worst successful-asset sync error (successful assets only)."""
        values = [r.sync_error_ms for r in self.successful if r.sync_error_ms is not None]
        return max(values) if values else None

    def recovery_rate(self) -> float:
        """Mean recovery rate across damaged assets, 1.0 when none were damaged."""
        values = [r.recovery_rate for r in self.damaged_samples if r.recovery_rate is not None]
        if not values:
            return 1.0 if not self.damaged_samples else 0.0
        return sum(values) / len(values)

    def remux_packet_match_rate(self) -> float:
        values = [r.remux_packet_match_rate for r in self.results if r.remux_packet_match_rate is not None]
        if not values:
            return 0.0
        return min(values)

    def p95_latency_ms(self) -> float:
        values = sorted(r.latency_ms for r in self.results)
        if not values:
            return 0.0
        index = max(0, math.ceil(0.95 * len(values)) - 1)
        return values[index]

    def sync_measured(self) -> bool:
        """True when at least one asset carried markers to measure sync with."""
        return any(r.sync_error_ms is not None for r in self.successful)

    def thresholds(self) -> dict[str, dict[str, Any]]:
        return {
            "sync_error_ms": _check(
                self.measured_sync_error_ms(),
                THRESHOLDS["sync_error_ms"],
                "max",
                applicable=self.sync_measured(),
            ),
            "recovery_rate": _check(self.recovery_rate(), THRESHOLDS["recovery_rate"], "min"),
            "p95_latency_ms": _check(
                self.p95_latency_ms(), THRESHOLDS["p95_latency_ms"], "max"
            ),
            "remux_packet_match_rate": _check(
                self.remux_packet_match_rate(),
                THRESHOLDS["remux_packet_match_rate"],
                "min",
            ),
            "damaged_samples": _check(
                float(len(self.damaged_samples)),
                THRESHOLDS["damaged_samples"],
                "min",
                # The demo ships three deliberately damaged assets, so the
                # count is part of its acceptance.  A real delivery folder may
                # legitimately contain none.
                applicable=self.scenario == "demo",
            ),
        }

    def all_passed(self) -> bool:
        return all(item["passed"] for item in self.thresholds().values())

    def exit_code(self) -> int:
        """0 all thresholds met, 1 a threshold missed, 3 nothing usable at all."""
        if self.results and self.fatal_assets == len(self.results):
            return EXIT_MEDIA
        if self.partial_samples:
            # Partial output is written and useful, but the status is non-zero.
            return EXIT_THRESHOLD
        return EXIT_OK if self.all_passed() else EXIT_THRESHOLD

    def to_dict(self) -> dict[str, Any]:
        return {
            "tool": "bcastqc",
            "version": _version(),
            "workdir": str(self.workdir),
            "started_at": self.started_at,
            "finished_at": self.finished_at,
            "scenario": self.scenario,
            "assets": [r.to_dict() for r in self.results],
            "summary": {
                "assets": len(self.results),
                "ok": len([r for r in self.results if r.status == STATUS_OK]),
                "recovered": len([r for r in self.results if r.status in DAMAGED_STATUSES]),
                "refused": len(self.refused_samples),
                "failed": len(self.failed_samples),
                "partial": len(self.partial_samples),
                "fatal": self.fatal_assets,
                "damaged_samples": len(self.damaged_samples),
                "sync_error_ms": _round(self.measured_sync_error_ms()),
                "recovery_rate": _round(self.recovery_rate()),
                "p95_latency_ms": _round(self.p95_latency_ms()),
                "remux_packet_match_rate": _round(self.remux_packet_match_rate()),
                "total_latency_ms": _round(sum(r.latency_ms for r in self.results)),
            },
            "thresholds": self.thresholds(),
            "passed": self.all_passed(),
            "exit_code": self.exit_code(),
        }

    def write(self, path: Path) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        write_json_atomic(path, self.to_dict())


def _version() -> str:
    from . import __version__

    return __version__


def _check(
    value: float | None,
    threshold: float,
    mode: str,
    *,
    applicable: bool = True,
) -> dict[str, Any]:
    if not applicable:
        # ``process`` runs on arbitrary delivery folders that carry no sync
        # markers; reporting "not applicable" is honest, failing them is not.
        return {
            "value": None,
            "threshold": threshold,
            "mode": mode,
            "applicable": False,
            "passed": True,
        }
    if value is None:
        return {
            "value": None,
            "threshold": threshold,
            "mode": mode,
            "applicable": True,
            "passed": False,
        }
    passed = value <= threshold if mode == "max" else value >= threshold
    return {
        "value": _round(value),
        "threshold": threshold,
        "mode": mode,
        "applicable": True,
        "passed": bool(passed),
    }


def _round(value: float | None) -> float | None:
    if value is None:
        return None
    return round(float(value), 6)


def write_json_atomic(path: Path, payload: dict[str, Any]) -> None:
    import os
    import tempfile

    path.parent.mkdir(parents=True, exist_ok=True)
    handle, temp = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    try:
        with os.fdopen(handle, "w", encoding="utf-8") as stream:
            json.dump(payload, stream, ensure_ascii=False, indent=2, sort_keys=False)
            stream.write("\n")
        os.replace(temp, path)
    except BaseException:
        try:
            os.unlink(temp)
        except FileNotFoundError:
            pass
        raise


def load_report(path: Path) -> dict[str, Any]:
    return json.loads(path.read_text(encoding="utf-8"))


def verify_thresholds(payload: dict[str, Any]) -> tuple[bool, list[str]]:
    """Re-evaluate the documented thresholds from a written report."""
    summary = payload.get("summary", {})
    checks = payload.get("thresholds", {})
    lines: list[str] = []
    passed = True
    for name, threshold in THRESHOLDS.items():
        item = checks.get(name)
        if item is None:
            passed = False
            lines.append(f"{name}: MISSING (expected {threshold})")
            continue
        if item.get("applicable") is False:
            lines.append(f"{name}: not applicable (no markers supplied) SKIP")
            continue
        value = item.get("value")
        if value is None:
            passed = False
            lines.append(f"{name}: no measurement (threshold {threshold})")
            continue
        ok = item.get("passed")
        if name == "damaged_samples":
            ok = float(summary.get("damaged_samples", 0)) >= threshold
        satisfied = bool(ok)
        passed = passed and satisfied
        lines.append(
            f"{name}: {value} (threshold {item.get('mode')} {threshold}) "
            f"{'PASS' if satisfied else 'FAIL'}"
        )
    return passed, lines
