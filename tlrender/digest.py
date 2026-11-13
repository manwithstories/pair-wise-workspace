"""Deterministic first-frame hashing and manifest persistence."""
from __future__ import annotations

import hashlib
import json
from pathlib import Path

from .compose import RenderedFrame
from .tl_parse import Cue

SHA_PREFIX = 16


def frame_sha256(frame: RenderedFrame) -> str:
    digest = hashlib.sha256(frame.rgb.astype("uint8").tobytes()).hexdigest()
    return digest[:SHA_PREFIX]


def full_sha256(png_bytes: bytes) -> str:
    return hashlib.sha256(png_bytes).hexdigest()


def write_pngs(frames: list[RenderedFrame], out_dir: str | Path) -> dict[str, Path]:
    out_dir = Path(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    paths: dict[str, Path] = {}
    for frame in frames:
        path = out_dir / f"{frame.cue_id}.png"
        path.write_bytes(frame.png_bytes)
        paths[frame.cue_id] = path
    return paths


def build_records(cues: list[Cue], hashes: dict[str, str]) -> list[dict]:
    records = []
    for cue in cues:
        records.append(
            {
                "cue": cue.id,
                "track": cue.track,
                "final": [cue.final_start, cue.final_end],
                "resolved": cue.resolved,
                "moved": cue.moved,
                "dropped": cue.dropped,
                "sha256": hashes.get(cue.id),
                "source": cue.source,
                "line": cue.line,
            }
        )
    return records


def write_manifest(
    out_dir: str | Path,
    records: list[dict],
    summary: dict,
    timestamp: str | None,
) -> Path:
    payload: dict = {"summary": summary, "cues": records}
    if timestamp is not None:
        payload["generated_at"] = timestamp
    path = Path(out_dir) / "manifest.json"
    text = json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True)
    path.write_text(text + "\n", encoding="utf-8")
    return path


def load_manifest(out_dir: str | Path) -> dict | None:
    path = Path(out_dir) / "manifest.json"
    if not path.exists():
        return None
    return json.loads(path.read_text(encoding="utf-8"))


def compare_manifest(previous: dict | None, hashes: dict[str, str]) -> int:
    if not previous or "cues" not in previous:
        return 0
    mismatches = 0
    old = {row["cue"]: row.get("sha256") for row in previous["cues"]}
    for cue_id, digest in hashes.items():
        if cue_id in old and old[cue_id] != digest:
            mismatches += 1
    return mismatches
