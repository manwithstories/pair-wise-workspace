"""Recursive batch export using the shared detector."""

from __future__ import annotations

from dataclasses import asdict
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import tempfile

from .detector import DetectorParams, detect
from .errors import AudioError, FileSystemError
from .store import Store, parameter_fingerprint
from .wavio import WavReader, sha256_file, write_wav_range_atomic


def _write_json_atomic(path: Path, payload: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fp:
            json.dump(payload, fp, ensure_ascii=False, indent=2)
            fp.write("\n")
            fp.flush()
            os.fsync(fp.fileno())
        os.replace(name, path)
    except OSError as exc:
        try:
            os.unlink(name)
        except OSError:
            pass
        raise FileSystemError("filesystem_error", str(exc), path=str(path)) from exc


def _safe_name(path: Path, digest: str) -> str:
    stem = "__".join(path.with_suffix("").parts)
    stem = re.sub(r"[^A-Za-z0-9_.-]+", "_", stem).strip("_") or "audio"
    return f"{stem}__{digest[:10]}"


def _wav_files(root: Path, excluded: Path | None) -> list[Path]:
    files = []
    for path in sorted(root.rglob("*")):
        if not path.is_file() or path.suffix.lower() != ".wav":
            continue
        if excluded is not None:
            try:
                path.resolve().relative_to(excluded)
                continue
            except ValueError:
                pass
        files.append(path)
    return files


def _segment_payload(segment, index: int, sample_rate: int, file_path: str) -> dict:
    return {
        "index": index,
        "file": file_path,
        "start_sample": segment.start_sample,
        "end_sample": segment.end_sample,
        "duration_ms": round((segment.end_sample - segment.start_sample) * 1000.0 / sample_rate, 3),
        "peak_dbfs": round(segment.peak_dbfs, 3),
    }


def export_directory(
    directory: str | os.PathLike[str],
    db_path: str | os.PathLike[str],
    out_dir: str | os.PathLike[str],
    manifest_path: str | os.PathLike[str],
    params: DetectorParams,
    *,
    allow_multichannel: bool = False,
) -> dict:
    params.validate()
    root = Path(directory)
    if not root.is_dir():
        raise FileSystemError("filesystem_error", "source directory does not exist", path=str(root))
    output = Path(out_dir)
    manifest_target = Path(manifest_path)
    try:
        output.mkdir(parents=True, exist_ok=True)
        manifest_target.parent.mkdir(parents=True, exist_ok=True)
    except OSError as exc:
        raise FileSystemError("filesystem_error", str(exc), path=str(output)) from exc
    source_files = _wav_files(root, output.resolve())
    params_dict = asdict(params)
    entries = []
    total_silence_ms = 0.0
    with Store(db_path) as store:
        batch_id = store.start_batch("export", params_dict)
        try:
            for source in source_files:
                with WavReader(source) as reader:
                    if reader.header.channels > 1 and not allow_multichannel:
                        raise AudioError(
                            "stereo_not_supported",
                            "stereo or multichannel input requires --allow-multichannel",
                            path=str(source),
                        )
                    detection = detect(reader, params)
                    digest = sha256_file(source)
                    file_id = store.record_scan(
                        batch_id=batch_id,
                        source_path=str(source),
                        content_sha256=digest,
                        params=params_dict,
                        detection=detection,
                    )
                    relative = source.relative_to(root)
                    prefix = _safe_name(relative, digest)
                    segment_entries = []
                    for index, segment in enumerate(detection.segments):
                        name = f"{prefix}__seg_{index:04d}.wav"
                        segment_path = output / name
                        write_wav_range_atomic(
                            segment_path,
                            reader.header.sample_rate,
                            reader,
                            segment.start_sample,
                            segment.end_sample,
                        )
                        store.register_export(file_id, index, str(segment_path))
                        manifest_link = os.path.relpath(segment_path, manifest_target.parent)
                        segment_entries.append(
                            _segment_payload(segment, index, reader.header.sample_rate, manifest_link)
                        )
                silence_samples = sum(
                    item.end_sample - item.start_sample for item in detection.silences
                )
                silence_ms = silence_samples * 1000.0 / detection.analysis.sample_rate
                total_silence_ms += silence_ms
                entries.append(
                    {
                        "file": str(relative),
                        "content_sha256": digest,
                        "sample_rate": detection.analysis.sample_rate,
                        "total_samples": detection.analysis.total_samples,
                        "noise_floor_dbfs": round(detection.thresholds.noise_floor_dbfs, 3),
                        "threshold_dbfs": round(detection.thresholds.enter_dbfs, 3),
                        "segment_count": len(detection.segments),
                        "silence_total_ms": round(silence_ms, 3),
                        "silence_ratio": round(silence_samples / detection.analysis.total_samples, 6),
                        "segments": segment_entries,
                    }
                )
            store.finish_batch(batch_id)
        except Exception:
            try:
                store.finish_batch(batch_id, "failed")
            except Exception:
                pass
            raise
    manifest = {
        "version": 1,
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "source_dir": str(root),
        "out_dir": str(output),
        "params": params_dict,
        "param_fingerprint": parameter_fingerprint(params_dict),
        "file_count": len(entries),
        "segment_count": sum(entry["segment_count"] for entry in entries),
        "silence_total_ms": round(total_silence_ms, 3),
        "files": entries,
    }
    _write_json_atomic(manifest_target, manifest)
    return manifest

