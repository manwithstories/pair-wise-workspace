"""Recompute report metrics from delivered audio files."""

from __future__ import annotations

from dataclasses import dataclass
import json
from pathlib import Path

from .io_block import BlockReader
from .kernel import design_kernel
from .loudness import LoudnessMeter, RmsMeter
from .truepeak import ClipCounter, TruePeakMeter


COMPARE_TOLERANCE_DB = 0.1


@dataclass(frozen=True)
class VerifyCheck:
    name: str
    expected: object
    actual: object
    passed: bool


@dataclass(frozen=True)
class VerifyResult:
    checked: int
    skipped: int
    checks: tuple[VerifyCheck, ...]

    @property
    def ok(self) -> bool:
        return all(check.passed for check in self.checks)


def _metric_checks(record: dict, metrics: dict) -> list[VerifyCheck]:
    checks: list[VerifyCheck] = []
    for name in ("lufs_after", "rms_dbfs_after", "true_peak_dbtp"):
        expected = record.get(name)
        actual = metrics[name]
        if expected is None:
            passed = False
        else:
            passed = abs(float(expected) - actual) <= COMPARE_TOLERANCE_DB
        checks.append(VerifyCheck(name, expected, round(actual, 6), bool(passed)))
    expected_clips = record.get("clipped_after")
    checks.append(
        VerifyCheck(
            "clipped_after",
            expected_clips,
            metrics["clipped"],
            expected_clips == metrics["clipped"],
        )
    )
    return checks


def _measure(path: Path, block_size: int) -> tuple[dict, int, int]:
    loudness = None
    rms = None
    peak = None
    clips = None
    with BlockReader(path) as reader:
        loudness = LoudnessMeter(reader.rate, 2)
        rms = RmsMeter(2)
        peak = TruePeakMeter(2)
        clips = ClipCounter()
        for block in reader.blocks(block_size):
            if block.shape[1] != 2:
                raise ValueError("output is not stereo")
            loudness.process(block)
            rms.process(block)
            peak.process(block)
            clips.process(block)
        rate, channels = reader.rate, reader.channels
    return (
        {
            "lufs_after": loudness.finalize(),
            "rms_dbfs_after": rms.finalize(),
            "true_peak_dbtp": peak.finalize(),
            "clipped": clips.finalize(),
        },
        rate,
        channels,
    )


def verify_report(
    report_path: Path,
    output_dir: Path,
    block_size: int = 4096,
) -> VerifyResult:
    with open(report_path, "r", encoding="utf-8") as stream:
        report = json.load(stream)
    records = report.get("records")
    if not isinstance(records, list):
        raise ValueError("report has no records array")
    checks: list[VerifyCheck] = []
    checked = 0
    skipped = 0
    root = output_dir.resolve()
    for record in records:
        if record.get("status") != "ok":
            skipped += 1
            continue
        checked += 1
        relative = Path(str(record.get("output", "")))
        path = (root / relative).resolve()
        if root not in path.parents or not path.is_file():
            checks.append(VerifyCheck(f"{relative}: file", True, False, False))
            continue
        try:
            metrics, rate, channels = _measure(path, block_size)
        except Exception as exc:
            checks.append(VerifyCheck(f"{relative}: read", "ok", str(exc), False))
            continue
        checks.append(
            VerifyCheck(
                f"{relative}: rate",
                record.get("out_rate"),
                rate,
                record.get("out_rate") == rate,
            )
        )
        checks.append(
            VerifyCheck(
                f"{relative}: channels",
                record.get("out_channels"),
                channels,
                record.get("out_channels") == channels,
            )
        )
        if record.get("in_rate") is not None and record.get("out_rate") is not None:
            source_rate = int(record["in_rate"])
            target_rate = int(record["out_rate"])
            fingerprint = (
                "bypass"
                if source_rate == target_rate
                else design_kernel(
                    source_rate, target_rate, report.get("quality", "high")
                ).fingerprint
            )
            checks.append(
                VerifyCheck(
                    f"{relative}: kernel",
                    record.get("kernel_fingerprint"),
                    fingerprint,
                    record.get("kernel_fingerprint") == fingerprint,
                )
            )
        checks.extend(
            VerifyCheck(
                f"{relative}: {item.name}", item.expected, item.actual, item.passed
            )
            for item in _metric_checks(record, metrics)
        )
    return VerifyResult(checked, skipped, tuple(checks))
