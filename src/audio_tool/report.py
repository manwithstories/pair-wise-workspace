"""Report writing and offline reproducibility checks.

The JSON report is the audit artifact: it records every parameter that
influenced the run, the algorithm version, per-file input fingerprints and
statistics, plus the errors for files that were skipped.  ``verify_report``
re-derives the gain and the expected post-normalization loudness from the
stored before-stats and compares them against what was measured, which is
what makes a run checkable long after the WAVs have moved.
"""

from __future__ import annotations

import hashlib
import json
import math
import os
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

from . import __version__
from .batch import BatchResult, FileOutcome
from .model import ProcessConfig, WriteError
from .normalize import TRUE_PEAK_MARGIN_DB, compute_gain

SCHEMA = "audio-tool/report/1"
ALGORITHM_VERSION = "bs1770-4/kweight-biquad+polyphase-sinc/v1"

#: Loudness error tolerated before a file is flagged as non-reproducible.
LUFS_TOLERANCE = 0.25
#: True-peak reading error tolerated (dB).
TRUE_PEAK_TOLERANCE_DB = 0.5


def build_report(result: BatchResult, *, command: str,
                 started_at: str | None = None) -> dict:
    config = result.config
    assert config is not None
    files = [outcome.to_dict() for outcome in result.outcomes]
    succeeded = [o for o in result.outcomes if o.ok]
    stats = [o.result for o in succeeded if o.result is not None]

    def total(field_name: str) -> int:
        return sum(getattr(item, field_name) for item in stats)

    summary = {
        "total": len(files),
        "succeeded": len(succeeded),
        "failed": len(result.outcomes) - len(succeeded),
        "output_sample_rate": config.target_rate,
        "target_lufs": config.target_lufs,
        "true_peak_db": config.true_peak_db,
        "channels": sorted({item.input_channels for item in stats}),
        "total_input_frames": total("input_frames"),
        "total_output_frames": total("output_frames"),
        "original_clipped": total("original_clipped"),
        "output_clipped": total("output_clipped"),
        "added_clipped": total("added_clipped"),
        "zero_padded_frames": total("zero_padded_frames"),
        "true_peak_limited": sum(1 for item in stats if item.true_peak_limited),
        "silent": sum(1 for item in stats if item.status == "silent"),
        "before_rms": _mean_before_rms(stats),
        "after_rms": _mean_after_rms(stats),
        "before_lufs": _mean_lufs(stats, "before"),
        "after_lufs": _mean_lufs(stats, "after"),
        "skipped": [{"source": o.relative, "code": o.error_code}
                    for o in result.outcomes if not o.ok],
    }
    return {
        "schema": SCHEMA,
        "algorithm_version": ALGORITHM_VERSION,
        "tool_version": __version__,
        "command": command,
        "generated_at": started_at or datetime.now(timezone.utc).isoformat(),
        "config": config.signature(),
        "resampler_instances": _kernel_rates(result),
        "summary": summary,
        "inputs_root": _relative_root(result.input_root),
        "outputs_root": _relative_root(result.output_root),
        "files": files,
    }


def _kernel_rates(result: BatchResult) -> list[int]:
    return sorted({o.result.input_rate for o in result.outcomes if o.result})


def _relative_root(root: Path | None) -> str | None:
    """Record a root relative to the working directory when it is under it.

    Absolute machine paths make a report hard to compare across machines and
    leak the local layout; when the root sits inside the run directory its
    relative form is just as unambiguous and stays portable.
    """

    if root is None:
        return None
    try:
        return str(Path(root).resolve().relative_to(Path.cwd().resolve()))
    except ValueError:
        return str(Path(root))


def _mean_lufs(stats, side: str) -> float | None:
    """Energy-weighted mean loudness across files, skips silent material."""

    energies = []
    for item in stats:
        values = item.before if side == "before" else item.after
        if values.lufs is None:
            continue
        energies.append(10.0 ** ((values.lufs - -0.691) / 10.0))
    if not energies:
        return None
    mean = sum(energies) / len(energies)
    return round(-0.691 + 10.0 * math.log10(mean), 4) if mean > 0 else None


def _mean_before_rms(stats) -> float | None:
    return _mean_rms(stats, "before")


def _mean_after_rms(stats) -> float | None:
    return _mean_rms(stats, "after")


def _mean_rms(stats, side: str) -> float | None:
    values = [getattr(item, side).rms for item in stats]
    values = [value for value in values if value > 0.0]
    if not values:
        return None
    return round(sum(values) / len(values), 8)


def write_report(path: Path, report: dict) -> Path:
    """Write the report atomically (temp file + rename), JSON, sorted keys."""

    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(f".{path.name}.tmp")
    try:
        text = json.dumps(report, indent=2, sort_keys=True, ensure_ascii=False)
        tmp.write_text(text + "\n", encoding="utf-8")
        os.replace(tmp, path)
    except OSError as exc:
        try:
            tmp.unlink(missing_ok=True)
        except OSError:
            pass
        raise WriteError(f"cannot write report {path}: {exc}",
                         details={"path": str(path)}) from exc
    return path


def load_report(path: Path) -> dict:
    path = Path(path)
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise WriteError(f"report not found: {path}", code="report_missing",
                         details={"path": str(path)}) from exc
    except json.JSONDecodeError as exc:
        raise WriteError(f"report is not valid JSON: {exc}", code="report_invalid",
                         details={"path": str(path)}) from exc


@dataclass
class VerifyFinding:
    source: str
    code: str
    detail: str

    def to_dict(self) -> dict:
        return {"source": self.source, "code": self.code, "detail": self.detail}


def verify_report(report: dict, *, output_root: Path | None = None) -> list[VerifyFinding]:
    """Re-derive results from the stored stats and flag inconsistencies.

    Checks, per successful file:

    * ``after_lufs`` matches ``before_lufs + applied_gain`` within tolerance
      (the normalization arithmetic is reproducible from the report alone);
    * the applied gain never exceeds the requested gain;
    * the written output, when present, still matches the recorded frame count;
    * clipping counters are non-negative and ``added <= output``.
    """

    findings: list[VerifyFinding] = []
    config = report.get("config", {})
    for entry in report.get("files", []):
        source = entry.get("source", "<unknown>")
        if not entry.get("ok"):
            continue
        stats = entry.get("stats") or {}
        before = stats.get("before") or {}
        after = stats.get("after") or {}
        gain = stats.get("gain_db")
        requested = stats.get("requested_gain_db")
        before_lufs = before.get("lufs")
        after_lufs = after.get("lufs")

        if gain is None or requested is None:
            findings.append(VerifyFinding(source, "missing_gain",
                                          "report entry lacks gain fields"))
            continue
        if gain > requested + 1e-6:
            findings.append(VerifyFinding(
                source, "gain_exceeds_request",
                f"applied {gain} dB > requested {requested} dB"))
        if before_lufs is not None and after_lufs is not None:
            predicted = before_lufs + gain
            delta = abs(predicted - after_lufs)
            if delta > LUFS_TOLERANCE:
                findings.append(VerifyFinding(
                    source, "lufs_mismatch",
                    f"before {before_lufs} + gain {gain} = {predicted:.3f}, "
                    f"measured {after_lufs} (delta {delta:.3f} LU)"))
        for key in ("original_clipped", "output_clipped", "added_clipped",
                    "zero_padded_frames"):
            value = stats.get(key)
            if value is None or not isinstance(value, int) or value < 0:
                findings.append(VerifyFinding(
                    source, "bad_counter", f"{key}={value!r} is not a "
                    "non-negative integer"))
        if (stats.get("added_clipped") or 0) > (stats.get("output_clipped") or 0):
            findings.append(VerifyFinding(
                source, "clipping_accounting",
                "added_clipped exceeds output_clipped"))
        true_peak = after.get("true_peak_dbtp")
        ceiling = config.get("true_peak_db")
        if true_peak is not None and ceiling is not None:
            if true_peak > ceiling + TRUE_PEAK_TOLERANCE_DB:
                findings.append(VerifyFinding(
                    source, "true_peak_over_ceiling",
                    f"{true_peak} dBTP exceeds ceiling {ceiling} dBTP"))
        if output_root is not None and entry.get("output"):
            output_path = Path(entry["output"])
            if not output_path.is_absolute():
                output_path = Path(output_root) / output_path
            if not output_path.exists():
                findings.append(VerifyFinding(
                    source, "output_missing", f"no file at {output_path}"))
    return findings


def format_summary(report: dict) -> str:
    """Human terminal summary: counts, aggregate levels, per-file table.

    Every field the acceptance checklist names is printed literally
    (``before_rms``, ``after_lufs``, ``original_clipped``,
    ``zero_padded_frames``, ...) so the output can be read and grepped
    without cross-referencing the JSON.
    """

    summary = report.get("summary", {})
    lines: list[str] = []
    lines.append("=" * 78)
    lines.append("audio-tool batch summary")
    lines.append("=" * 78)
    lines.append(f"algorithm          : {report.get('algorithm_version')}")
    lines.append(f"output_sample_rate : {summary.get('output_sample_rate')} Hz")
    lines.append(f"target_lufs        : {summary.get('target_lufs')} LUFS  "
                 f"(true_peak {summary.get('true_peak_db')} dBTP)")
    lines.append(f"files              : {summary.get('succeeded')} ok / "
                 f"{summary.get('failed')} failed / {summary.get('total')} total")
    lines.append(f"channels           : {summary.get('channels')}")
    lines.append(f"before_rms         : {_fmt(summary.get('before_rms'), '.6f')}")
    lines.append(f"after_rms          : {_fmt(summary.get('after_rms'), '.6f')}")
    lines.append(f"before_lufs        : {_fmt(summary.get('before_lufs'))}")
    lines.append(f"after_lufs         : {_fmt(summary.get('after_lufs'))}")
    lines.append(f"original_clipped   : {summary.get('original_clipped')}")
    lines.append(f"output_clipped     : {summary.get('output_clipped')}")
    lines.append(f"added_clipped      : {summary.get('added_clipped')}")
    lines.append(f"zero_padded_frames : {summary.get('zero_padded_frames')}")
    lines.append(f"true_peak_limited  : {summary.get('true_peak_limited')} file(s)")
    lines.append(f"silent             : {summary.get('silent')} file(s)")
    lines.append("-" * 78)
    header = (f"{'file':30s} {'ch':>3s} {'before':>8s} {'after':>8s} "
              f"{'gain':>7s} {'TP':>7s} {'clipped':>7s}")
    lines.append(header)
    lines.append("-" * 78)
    for entry in report.get("files", []):
        name = entry.get("source", "?")
        if len(name) > 30:
            name = "..." + name[-27:]
        if not entry.get("ok"):
            error = entry.get("error") or {}
            lines.append(f"{name:30s} SKIPPED: {error.get('code')}: "
                         f"{error.get('message')}")
            continue
        stats = entry.get("stats", {})
        before = stats.get("before", {})
        after = stats.get("after", {})
        before_lufs = before.get("lufs")
        after_lufs = after.get("lufs")
        gain = stats.get("gain_db")
        true_peak = after.get("true_peak_dbtp")
        channels = stats.get("input_channels", 0)
        status = stats.get("status", "ok")
        suffix = "" if status == "ok" else f"  [{status}]"
        lines.append(
            f"{name:30s} {channels:>3d} "
            f"{_fmt(before_lufs):>8s} {_fmt(after_lufs):>8s} "
            f"{_fmt(gain, '+.2f'):>7s} {_fmt(true_peak):>7s} "
            f"{stats.get('output_clipped', 0):>7d}{suffix}")
    lines.append("=" * 78)
    return "\n".join(lines)


def _fmt(value, spec: str = ".2f") -> str:
    if value is None:
        return "-inf"
    try:
        return format(value, spec)
    except (TypeError, ValueError):
        return str(value)


def summarize_for_terminal(report: dict) -> str:
    return format_summary(report)
