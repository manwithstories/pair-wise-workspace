"""Fixture truth comparison for segmentation and boundary accuracy."""

from __future__ import annotations

import json
import os
from pathlib import Path

from .detector import DetectorParams, detect
from .errors import AudioError, FileSystemError, UsageError
from .wavio import WavReader


def run_selftest(
    fixtures: str | os.PathLike[str], tolerance_samples: int = 48
) -> tuple[dict, int]:
    if tolerance_samples < 0:
        raise UsageError("invalid_arguments", "tolerance must be non-negative")
    root = Path(fixtures)
    if not root.is_dir():
        raise FileSystemError("filesystem_error", "fixture directory does not exist", path=str(root))
    truth_path = root / "truth.json"
    try:
        truth = json.loads(truth_path.read_text(encoding="utf-8"))
    except OSError as exc:
        raise FileSystemError("filesystem_error", str(exc), path=str(truth_path)) from exc
    except json.JSONDecodeError as exc:
        raise AudioError("invalid_fixture_truth", str(exc), path=str(truth_path)) from exc
    expected_files = truth.get("files", [])
    if not expected_files:
        raise AudioError("invalid_fixture_truth", "truth manifest has no files", path=str(truth_path))
    params = DetectorParams(pad_ms=0.0)
    results = []
    failed = False
    for expected in expected_files:
        source = root / expected["file"]
        with WavReader(source) as reader:
            detection = detect(reader, params)
        expected_segments = expected["segments"]
        actual_segments = detection.segments
        max_error = 0
        for actual, truth_segment in zip(actual_segments, expected_segments):
            max_error = max(
                max_error,
                abs(actual.start_sample - int(truth_segment["start_sample"])),
                abs(actual.end_sample - int(truth_segment["end_sample"])),
            )
        delta = len(actual_segments) - len(expected_segments)
        passed = delta == 0 and max_error <= tolerance_samples
        failed = failed or not passed
        results.append(
            {
                "file": expected["file"],
                "expected_segments": len(expected_segments),
                "actual_segments": len(actual_segments),
                "segment_count_delta": delta,
                "max_boundary_error_samples": max_error,
                "noise_floor_dbfs": round(detection.thresholds.noise_floor_dbfs, 3),
                "pass": passed,
            }
        )
    maximum = max((item["max_boundary_error_samples"] for item in results), default=0)
    payload = {
        "status": "FAIL" if failed else "PASS",
        "tolerance_samples": tolerance_samples,
        "max_boundary_error_samples": maximum,
        "file_count": len(results),
        "files": results,
    }
    return payload, 4 if failed else 0

