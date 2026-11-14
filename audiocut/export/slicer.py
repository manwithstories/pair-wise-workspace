"""Slice the original WAV into per-segment files.

Re-reads the source file and cuts raw frames, so the original channel
layout and bit depth are preserved and output bytes are deterministic
for a given input and segment list. A manifest.json is rewritten after
every file: completed entries survive a failure and are skipped on the
next run (resume). File names carry index and start/end sample points.
"""
from __future__ import annotations

import json
import os
import struct

from ..store import db as store
from ..wavio import reader


class ExportError(Exception):
    """Export pipeline failure (CLI exit code 5)."""


def _fmt_chunk(info: reader.WavInfo) -> bytes:
    byte_rate = info.sample_rate * info.block_align
    return b"fmt " + struct.pack(
        "<IHHIIHH", 16, info.audio_format, info.channels,
        info.sample_rate, byte_rate, info.block_align,
        info.bits_per_sample)


def _wrap(data: bytes, info: reader.WavInfo) -> bytes:
    fmt = _fmt_chunk(info)
    pad = b"\x00" if len(data) & 1 else b""
    riff_size = 4 + len(fmt) + 8 + len(data) + len(pad)
    return (b"RIFF" + struct.pack("<I", riff_size) + b"WAVE" + fmt
            + b"data" + struct.pack("<I", len(data)) + data + pad)


def _write_manifest(path: str, run: dict, info: reader.WavInfo,
                    items: list[dict]) -> None:
    payload = {
        "run_id": run["id"],
        "input": run["input_path"],
        "sample_rate": info.sample_rate,
        "channels": info.channels,
        "bits_per_sample": info.bits_per_sample,
        "items": sorted(items, key=lambda it: it["index"]),
    }
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(payload, fh, indent=2)
        fh.write("\n")
    os.replace(tmp, path)


def export_run(db_path: str, out_dir: str) -> list[dict]:
    run = store.latest_run(db_path)
    if run is None:
        raise ExportError(f"{db_path}: no detection run found")
    try:
        info = reader.read_info(run["input_path"])
    except reader.WavError as exc:
        raise ExportError(str(exc)) from exc
    os.makedirs(out_dir, exist_ok=True)
    manifest_path = os.path.join(out_dir, "manifest.json")
    done: dict[int, dict] = {}
    if os.path.exists(manifest_path):
        try:
            with open(manifest_path, encoding="utf-8") as fh:
                prev = json.load(fh)
            if prev.get("run_id") == run["id"]:
                done = {it["index"]: it for it in prev.get("items", [])
                        if it.get("done")}
        except (ValueError, OSError):
            done = {}
    stem = os.path.splitext(os.path.basename(run["input_path"]))[0]
    items: list[dict] = []
    try:
        with open(run["input_path"], "rb") as fh:
            for seg in run["segments"]:
                idx = seg["idx"]
                start, end = seg["start_sample"], seg["end_sample"]
                n_bytes = (end - start) * info.block_align
                name = f"{stem}_{idx:03d}_{start}-{end}.wav"
                path = os.path.join(out_dir, name)
                item = {"index": idx, "file": name,
                        "start_sample": start, "end_sample": end,
                        "frames": end - start, "done": True}
                if (idx in done and os.path.exists(path)
                        and os.path.getsize(path)
                        == 44 + n_bytes + (n_bytes & 1)):
                    items.append(done[idx])
                    continue
                fh.seek(info.data_offset + start * info.block_align)
                data = fh.read(n_bytes)
                if len(data) != n_bytes:
                    raise ExportError(
                        f"segment {idx}: source ended at sample "
                        f"{start + len(data) // info.block_align}")
                tmp = path + ".tmp"
                with open(tmp, "wb") as out:
                    out.write(_wrap(data, info))
                os.replace(tmp, path)
                items.append(item)
                _write_manifest(manifest_path, run, info, items)
    except ExportError:
        _write_manifest(manifest_path, run, info, items)
        raise
    except OSError as exc:
        _write_manifest(manifest_path, run, info, items)
        raise ExportError(str(exc)) from exc
    _write_manifest(manifest_path, run, info, items)
    return sorted(items, key=lambda it: it["index"])
