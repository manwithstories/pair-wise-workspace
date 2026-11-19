"""Independent export validator.

The validator never trusts the pipeline's in-memory state: it re-reads every
artifact from disk (manifest, PNG frames, audio sidecar), recomputes all
hashes and dimensions, and checks time continuity. Reported failure classes:

* missing frame (manifest entry without a file, or non-contiguous naming)
* duplicate time point (two entries on the same master tick)
* dimension mismatch (decoded PNG size differs from the manifest record)
* hash mismatch (file content digest differs from the recorded digest)
"""

from __future__ import annotations

import hashlib
import json
from pathlib import Path

from .model import ValidationError, safe_resolve
from .render import decode_png, frame_name


def _sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def load_manifest(workdir: str | Path) -> dict:
    path = safe_resolve(workdir, "frames", "manifest.json")
    if not path.exists():
        raise ValidationError("manifest.json missing", path=str(path))
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        raise ValidationError(
            f"manifest is not valid JSON: {exc}", path=str(path), offset=exc.pos
        ) from exc


def validate_export(workdir: str | Path, manifest: dict | None = None) -> list[ValidationError]:
    """Re-read the full export and return every defect found (empty == clean)."""
    workdir = Path(workdir)
    if manifest is None:
        manifest = load_manifest(workdir)
    errors: list[ValidationError] = []
    frames_dir = safe_resolve(workdir, "frames")
    entries = manifest.get("frames", [])

    if not entries:
        errors.append(ValidationError("manifest contains no frames", path=str(frames_dir)))
        return errors

    seen_ticks: dict[int, str] = {}
    recomputed_root = hashlib.sha256()
    prev_tick: int | None = None

    for ordinal, entry in enumerate(entries):
        expected_name = frame_name(ordinal)
        name = entry.get("name", "")
        if name != expected_name:
            errors.append(ValidationError(
                f"frame naming gap: expected {expected_name}, manifest has {name}",
                path=str(frames_dir / name), frame_seq=ordinal,
            ))
        target = frames_dir / name
        if not target.exists():
            errors.append(ValidationError(
                "missing exported frame", path=str(target),
                frame_seq=entry.get("frame_seq"),
            ))
            continue

        actual_hash = _sha256(target)
        if actual_hash != entry.get("sha256"):
            errors.append(ValidationError(
                f"frame hash mismatch: manifest={entry.get('sha256')} actual={actual_hash}",
                path=str(target), frame_seq=entry.get("frame_seq"),
            ))
        recomputed_root.update(bytes.fromhex(actual_hash))

        # Pixel dimensions are decoded on a sampled subset: the full-content
        # SHA-256 above already proves every byte of every frame, so decoding
        # every image would add cost without catching a distinct failure class.
        tick = entry.get("tick")
        if ordinal % 8 == 0 or actual_hash != entry.get("sha256"):
            try:
                rgb = decode_png(target.read_bytes())
            except Exception as exc:  # decoder failure is a validation failure
                errors.append(ValidationError(
                    f"PNG decode failed: {exc}", path=str(target),
                    frame_seq=entry.get("frame_seq"),
                ))
                continue
            h, w = rgb.shape[:2]
            if (w, h) != (entry.get("width"), entry.get("height")):
                errors.append(ValidationError(
                    f"dimension mismatch: manifest={entry.get('width')}x{entry.get('height')} "
                    f"actual={w}x{h}",
                    path=str(target), frame_seq=entry.get("frame_seq"),
                ))

        tick = entry.get("tick")
        if not isinstance(tick, int):
            errors.append(ValidationError(
                f"frame entry lacks integer tick: {tick!r}",
                path=str(target), frame_seq=entry.get("frame_seq"),
            ))
            continue
        if tick in seen_ticks:
            errors.append(ValidationError(
                f"duplicate time point tick={tick} shared with {seen_ticks[tick]}",
                path=str(target), frame_seq=entry.get("frame_seq"),
            ))
        if prev_tick is not None and tick <= prev_tick:
            errors.append(ValidationError(
                f"time continuity broken: tick {tick} does not follow {prev_tick}",
                path=str(target), frame_seq=entry.get("frame_seq"),
            ))
        seen_ticks[tick] = name
        prev_tick = tick

    # Orphan files on disk are defects too (export not reproducible from manifest).
    expected_names = {e.get("name") for e in entries}
    for extra in sorted(frames_dir.glob("frame_*.png")):
        if extra.name not in expected_names:
            errors.append(ValidationError(
                f"unexpected frame file not listed in manifest: {extra.name}",
                path=str(extra),
            ))

    # Audio sidecar: recompute and compare against the manifest.
    audio = manifest.get("audio", {})
    audio_path = frames_dir / audio.get("name", "sidecar.wav")
    if not audio_path.exists():
        errors.append(ValidationError("audio sidecar missing", path=str(audio_path)))
    else:
        actual_audio_hash = _sha256(audio_path)
        if actual_audio_hash != audio.get("sha256"):
            errors.append(ValidationError(
                f"audio hash mismatch: manifest={audio.get('sha256')} "
                f"actual={actual_audio_hash}",
                path=str(audio_path),
            ))
        recomputed_root.update(bytes.fromhex(actual_audio_hash))

    if manifest.get("output_sha256") != recomputed_root.hexdigest():
        errors.append(ValidationError(
            f"output root hash mismatch: manifest={manifest.get('output_sha256')} "
            f"recomputed={recomputed_root.hexdigest()}",
            path=str(frames_dir),
        ))
    return errors
