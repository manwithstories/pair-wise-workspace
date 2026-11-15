"""Batch orchestration, resumable reports, and verification."""

from __future__ import annotations

from dataclasses import dataclass
import json
import math
import os
from pathlib import Path
import tempfile

from .io_block import (
    AudioFileError,
    BlockReader,
    ERROR_OUTPUT,
    probe_output_directory,
)
from .kernel import KernelLimitError, design_kernel
from .loudness import DB_FLOOR
from .normalize import DEFAULT_TARGET_LUFS, compute_gain
from .pipeline import first_pass, second_pass


SCHEMA_VERSION = 1
ERROR_PROCESSING = 15
AUDIO_SUFFIXES = {".wav", ".wave", ".aif", ".aiff", ".flac", ".au", ".ogg"}
REPORT_FLOAT_DIGITS = 6


@dataclass(frozen=True)
class BatchConfig:
    input_dir: Path
    output_dir: Path
    target_rate: int = 48000
    target_lufs: float = DEFAULT_TARGET_LUFS
    block_size: int = 4096
    quality: str = "high"
    cache_dir: Path | None = None
    report_path: Path | None = None
    resume: bool = False


def _number(value: float, digits: int = REPORT_FLOAT_DIGITS) -> float:
    if value is None or not math.isfinite(value):
        return DB_FLOOR
    return round(float(value), digits)


def _empty_record(relative: Path, output: str = "") -> dict:
    return {
        "input": relative.as_posix(),
        "output": output,
        "status": "ok",
        "error_code": None,
        "error": None,
        "in_rate": None,
        "out_rate": None,
        "out_channels": None,
        "rms_dbfs_before": None,
        "rms_dbfs_after": None,
        "lufs_before": None,
        "lufs_after": None,
        "true_peak_dbtp": None,
        "clipped_before": None,
        "clipped_after": None,
        "kernel_fingerprint": None,
        "gain_db": None,
        "target_lufs": None,
        "normalization_rounds": [],
    }


def _fail(record: dict, code: int, message: str) -> dict:
    record["status"] = "failed"
    record["error_code"] = int(code)
    record["error"] = message
    return record


def discover_audio(input_dir: Path) -> list[Path]:
    if not input_dir.is_dir():
        raise ValueError(f"input directory does not exist: {input_dir}")
    return sorted(
        (
            path
            for path in input_dir.rglob("*")
            if path.is_file() and path.suffix.lower() in AUDIO_SUFFIXES
        ),
        key=lambda path: path.relative_to(input_dir).as_posix(),
    )


def _process_file(
    source_path: Path,
    relative: Path,
    config: BatchConfig,
    cache_dir: Path,
) -> dict:
    record = _empty_record(relative, relative.with_suffix(".wav").as_posix())
    descriptor, name = tempfile.mkstemp(prefix="audio-", suffix=".f64", dir=cache_dir)
    os.close(descriptor)
    temp_path = Path(name)
    output_path = config.output_dir / record["output"]
    try:
        with BlockReader(source_path) as reader:
            record["in_rate"] = reader.rate
            record["out_rate"] = config.target_rate
            record["out_channels"] = 2
            frames, before = first_pass(
                reader,
                config.target_rate,
                config.block_size,
                config.quality,
                temp_path,
            )
        plan = compute_gain(
            before["lufs"],
            before["true_peak"],
            target_lufs=config.target_lufs,
        )
        after = second_pass(
            temp_path,
            frames,
            plan.gain_db,
            output_path,
            config.target_rate,
            config.block_size,
        )
        record.update(
            rms_dbfs_before=_number(before["rms"]),
            rms_dbfs_after=_number(after["rms"]),
            lufs_before=_number(before["lufs"]),
            lufs_after=_number(after["lufs"]),
            true_peak_dbtp=_number(after["true_peak"]),
            clipped_before=int(before["clipped"]),
            clipped_after=int(after["clipped"]),
            kernel_fingerprint=before["fingerprint"],
            gain_db=_number(plan.gain_db),
            target_lufs=_number(config.target_lufs),
            normalization_rounds=[
                {
                    "round": item.round,
                    "gain_db": _number(item.gain_db),
                    "predicted_true_peak_dbtp": _number(item.predicted_true_peak_dbtp),
                    "action": item.action,
                }
                for item in plan.rounds
            ],
        )
        return record
    except (AudioFileError, KernelLimitError) as exc:
        output_path.unlink(missing_ok=True)
        return _fail(
            record,
            getattr(exc, "code", getattr(exc, "exit_code", ERROR_PROCESSING)),
            str(exc),
        )
    except Exception as exc:
        output_path.unlink(missing_ok=True)
        return _fail(record, ERROR_PROCESSING, f"{type(exc).__name__}: {exc}")
    finally:
        temp_path.unlink(missing_ok=True)


def scan_groups(config: BatchConfig) -> tuple[list[dict], list[dict]]:
    groups: dict[tuple[int, int, str], list[str]] = {}
    failures: list[dict] = []
    for source in discover_audio(config.input_dir):
        relative = source.relative_to(config.input_dir)
        try:
            with BlockReader(source) as reader:
                groups.setdefault(
                    (reader.rate, config.target_rate, config.quality), []
                ).append(relative.as_posix())
        except AudioFileError as exc:
            failures.append(_fail(_empty_record(relative), exc.code, str(exc)))
    result = []
    for (source_rate, target_rate, quality), files in sorted(groups.items()):
        kernel = (
            None
            if source_rate == target_rate
            else design_kernel(source_rate, target_rate, quality)
        )
        result.append(
            {
                "in_rate": source_rate,
                "target_rate": target_rate,
                "quality": quality,
                "kernel_fingerprint": "bypass"
                if kernel is None
                else kernel.fingerprint,
                "files": sorted(files),
            }
        )
    return result, failures


def run_batch(config: BatchConfig) -> dict:
    files = discover_audio(config.input_dir)
    report = {
        "schema_version": SCHEMA_VERSION,
        "target_rate": int(config.target_rate),
        "target_lufs": _number(config.target_lufs),
        "quality": config.quality,
        "groups": [],
        "records": [],
        "summary": {"total": len(files), "ok": 0, "failed": 0},
    }
    cache_dir = config.cache_dir or (config.output_dir / ".cache")
    try:
        probe_output_directory(config.output_dir)
        cache_dir.mkdir(parents=True, exist_ok=True)
    except (AudioFileError, OSError) as exc:
        for source in files:
            relative = source.relative_to(config.input_dir)
            report["records"].append(
                _fail(_empty_record(relative), ERROR_OUTPUT, str(exc))
            )
        report["summary"]["failed"] = len(report["records"])
        return report

    groups, group_failures = scan_groups(config)
    report["groups"] = [
        {
            key: group[key]
            for key in ("in_rate", "target_rate", "quality", "kernel_fingerprint")
        }
        for group in groups
    ]
    records = {record["input"]: record for record in group_failures}
    if config.resume and config.report_path and config.report_path.is_file():
        try:
            previous = json.loads(config.report_path.read_text(encoding="utf-8"))
            compatible = (
                previous.get("schema_version") == SCHEMA_VERSION
                and previous.get("target_rate") == config.target_rate
                and previous.get("target_lufs") == _number(config.target_lufs)
                and previous.get("quality") == config.quality
            )
            if compatible:
                for record in previous.get("records", []):
                    output = config.output_dir / str(record.get("output", ""))
                    if record.get("status") == "ok" and output.is_file():
                        records[record["input"]] = record
        except (OSError, ValueError, TypeError):
            pass
    for source in files:
        relative = source.relative_to(config.input_dir)
        key = relative.as_posix()
        if key in records and records[key].get("status") == "ok":
            continue
        records[key] = _process_file(source, relative, config, cache_dir)
        if config.report_path is not None:
            _fill_summary(report, records, len(files))
            write_report(report, config.report_path)
    _fill_summary(report, records, len(files))
    return report


def _fill_summary(report: dict, records: dict[str, dict], total: int) -> None:
    report["records"] = [records[key] for key in sorted(records)]
    report["summary"]["total"] = total
    report["summary"]["ok"] = sum(r["status"] == "ok" for r in report["records"])
    report["summary"]["failed"] = sum(
        r["status"] == "failed" for r in report["records"]
    )


def write_report(report: dict, path: Path) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    with open(temporary, "w", encoding="utf-8") as stream:
        json.dump(
            report,
            stream,
            ensure_ascii=False,
            indent=2,
            sort_keys=True,
            allow_nan=False,
        )
        stream.write("\n")
    temporary.replace(path)
