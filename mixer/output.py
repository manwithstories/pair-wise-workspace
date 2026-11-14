"""Atomic output publication and deterministic WAV metadata handling."""

from __future__ import annotations

import json
import os
import tempfile
from typing import Any

from .errors import MixerIOError


def prepare_parent(path: str) -> None:
    parent = os.path.dirname(os.path.abspath(path))
    try:
        os.makedirs(parent, exist_ok=True)
        handle, probe = tempfile.mkstemp(prefix=".mixer-write-", dir=parent)
        os.close(handle)
        os.unlink(probe)
    except OSError as exc:
        raise MixerIOError(
            f"output directory is not writable: {exc}",
            details={"path": path, "directory": parent},
        ) from exc


def temp_file(target: str, suffix: str) -> str:
    parent = os.path.dirname(os.path.abspath(target))
    try:
        handle, path = tempfile.mkstemp(prefix=".mixer-", suffix=suffix, dir=parent)
        os.close(handle)
        return path
    except OSError as exc:
        raise MixerIOError(
            f"cannot create temporary output: {exc}",
            details={"target": target},
        ) from exc


def write_json_file(path: str, payload: dict[str, Any]) -> None:
    try:
        with open(path, "w", encoding="utf-8") as handle:
            json.dump(payload, handle, ensure_ascii=False, allow_nan=False, separators=(",", ":"))
            handle.write("\n")
    except (OSError, TypeError, ValueError) as exc:
        raise MixerIOError(f"cannot write report: {exc}", details={"path": path}) from exc


def publish_pair(
    wav_temp: str | None,
    wav_target: str | None,
    report_temp: str | None,
    report_target: str | None,
) -> None:
    published_wav = False
    try:
        if wav_temp and wav_target:
            os.replace(wav_temp, wav_target)
            published_wav = True
        if report_temp and report_target:
            os.replace(report_temp, report_target)
    except OSError as exc:
        if published_wav and wav_target:
            try:
                os.unlink(wav_target)
            except OSError:
                pass
        raise MixerIOError(f"cannot publish output: {exc}", details={"report": report_target}) from exc


def normalize_wav_metadata(path: str) -> None:
    """Zero libsndfile's wall-clock PEAK timestamp for byte determinism."""
    try:
        with open(path, "r+b") as handle:
            header = handle.read(12)
            if len(header) < 12 or header[:4] != b"RIFF" or header[8:12] != b"WAVE":
                return
            while True:
                chunk_header = handle.read(8)
                if len(chunk_header) < 8:
                    break
                chunk_id = chunk_header[:4]
                chunk_size = int.from_bytes(chunk_header[4:8], "little")
                data_start = handle.tell()
                if chunk_id == b"PEAK" and chunk_size >= 8:
                    handle.seek(data_start + 4)
                    handle.write(b"\x00\x00\x00\x00")
                handle.seek(data_start + chunk_size + (chunk_size & 1))
    except OSError as exc:
        raise MixerIOError(f"cannot normalize WAV metadata: {exc}", details={"path": path}) from exc


def cleanup(*paths: str | None) -> None:
    for path in paths:
        if path:
            try:
                os.unlink(path)
            except FileNotFoundError:
                pass
            except OSError:
                pass
