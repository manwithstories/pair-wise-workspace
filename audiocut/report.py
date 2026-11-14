"""Aggregate metrics, consistency check, and label export."""

from __future__ import annotations

import json
from pathlib import Path


def build_report(store) -> dict:
    detail = []
    total_segments = 0
    total_silence_samples = 0
    total_samples_all = 0
    for file_id, path, sample_rate, total_samples, _status, _err in store.files():
        segments = [{"start_sample": s, "end_sample": e}
                    for s, e in store.intervals("segments", file_id)]
        silences = store.intervals("silences", file_id)
        seg_sum = sum(s["end_sample"] - s["start_sample"] for s in segments)
        sil_sum = sum(e - s for s, e in silences)
        if seg_sum + sil_sum != total_samples:
            raise ValueError(
                f"coverage mismatch in {path}: {seg_sum}+{sil_sum} != {total_samples}")
        total_segments += len(segments)
        total_silence_samples += sil_sum
        total_samples_all += total_samples
        detail.append({
            "path": path,
            "sample_rate": sample_rate,
            "total_samples": total_samples,
            "segments": segments,
            "silence_samples": sil_sum,
        })
    total_silence_ms = sum(
        round(d["silence_samples"] * 1000 / d["sample_rate"]) for d in detail)
    return {
        "files": len(detail),
        "segments": total_segments,
        "total_silence_ms": total_silence_ms,
        "total_samples": total_samples_all,
        "detail": detail,
    }


def export_labels(store, profile: dict, out_path) -> int:
    """Write one JSON object per segment (jsonl), ordered by path/idx."""
    count = 0
    with open(out_path, "w", encoding="utf-8") as fh:
        for file_id, path, sample_rate, _total, _status, _err in store.files():
            if sample_rate != profile["sample_rate"]:
                continue
            for idx, (start, end) in enumerate(
                    store.intervals("segments", file_id)):
                fh.write(json.dumps({
                    "path": path,
                    "segment_index": idx,
                    "start_sample": start,
                    "end_sample": end,
                    "start_ms": round(start * 1000 / sample_rate, 3),
                    "end_ms": round(end * 1000 / sample_rate, 3),
                }, sort_keys=True) + "\n")
                count += 1
    return count
