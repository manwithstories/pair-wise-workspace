"""Re-run the shared detection pipeline and export per-segment WAV clips."""

from __future__ import annotations

import json
import os
import re
import struct
from pathlib import Path

from .config import Params
from .errors import DatabaseError, TalkmapError
from .refine import run_pipeline
from .store import Store
from .util import safe_resolve
from .wavio import WavReader, _encode_pcm

_MANIFEST_NAME = "manifest.jsonl"
_SEG_SUFFIX = re.compile(r"_seg\d{3}\.wav$")


def _plan_ranges(starts: list[int], ends: list[int], pre: int, post: int,
                 total: int) -> list[tuple[int, int]]:
    ranges: list[tuple[int, int]] = []
    for idx in range(len(starts)):
        lo = max(0, starts[idx] - pre)
        hi = min(total, ends[idx] + post)
        if ranges and lo < ranges[-1][1]:
            midpoint = (ranges[-1][1] + lo) // 2
            ranges[-1] = (ranges[-1][0], midpoint)
            lo = midpoint
        ranges.append((lo, hi))
    return ranges


def _atomic_write(path: Path, payload: bytes) -> None:
    tmp = path.with_name(f".{path.name}.tmp{os.getpid()}")
    with tmp.open("wb") as fh:
        fh.write(payload)
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, path)



def export_segments(store: Store, out_dir: str, params: Params,
                    pre_roll_ms: float, post_roll_ms: float) -> dict:
    if pre_roll_ms < 0 or post_roll_ms < 0:
        raise TalkmapError("roll margins must be non-negative")
    out = safe_resolve(out_dir)
    out.mkdir(parents=True, exist_ok=True)
    exported = 0
    manifest_rows: list[dict] = []
    for frow in store.active_files():
        stored_params = Params.from_json(frow["params_json"])
        if stored_params != params:
            raise DatabaseError(
                f"params differ from stored analysis for {frow['path']}")
        path = safe_resolve(frow["path"])
        stored = store.segments_for(frow["id"])
        with WavReader(path) as reader:
            segments, total = run_pipeline(reader, params)
            if len(segments) != len(stored):
                raise DatabaseError(
                    f"segment count drift for {path.name}: "
                    f"{len(segments)} now vs {len(stored)} stored")
            rate = reader.info.sample_rate
            pre_s = round(pre_roll_ms / 1000.0 * rate)
            post_s = round(post_roll_ms / 1000.0 * rate)
            starts = [seg.start_sample for seg in segments]
            ends = [seg.end_sample for seg in segments]
            ranges = _plan_ranges(starts, ends, pre_s, post_s, total)
            stem = Path(path).stem
            for idx, (seg, (lo, hi)) in enumerate(zip(segments, ranges)):
                name = f"{stem}_seg{idx + 1:03d}.wav"
                target = out / name
                if target.exists() and not _SEG_SUFFIX.search(target.name):
                    raise DatabaseError(f"refusing to overwrite foreign file {target}")
                samples = reader.read_mono_range(lo, hi)
                payload = _encode_pcm(samples, 16)
                header = _wav_header(rate, len(payload))
                _atomic_write(target, header + payload)
                manifest_rows.append({
                    "source": str(path), "seg_index": idx + 1,
                    "start_sample": seg.start_sample, "end_sample": seg.end_sample,
                    "clip_start_sample": lo, "clip_end_sample": hi,
                    "file": name,
                })
                exported += 1
    manifest_path = out / _MANIFEST_NAME
    lines = "".join(json.dumps(row, ensure_ascii=False) + "\n"
                    for row in manifest_rows)
    _atomic_write(manifest_path, lines.encode("utf-8"))
    return {"exported": exported, "manifest_lines": len(manifest_rows),
            "manifest": str(manifest_path)}


def _wav_header(rate: int, payload_len: int) -> bytes:
    block_align = 2
    return struct.pack("<4sI4s4sIHHIIHH4sI",
                       b"RIFF", 36 + payload_len, b"WAVE", b"fmt ",
                       16, 1, 1, rate, rate * block_align, block_align, 16,
                       b"data", payload_len)

