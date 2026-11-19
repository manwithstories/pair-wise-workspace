"""Checkpoint persistence for interruptible, resumable restores.

The state directory holds a manifest plus one JSON file per completed
stage.  The manifest pins the input SHA-256 and the parameter hash:
if either changes, a resume attempt is rejected with the reason.
All files are written atomically (temp file + os.replace).
"""

from __future__ import annotations

import hashlib
import json
import os
import tempfile

# Stage format versions: bump when a stage's persisted shape changes.
STAGE_VERSIONS = {"parse": 2, "repair": 2, "index": 2, "verify": 2}
MANIFEST_NAME = "manifest.json"


class CheckpointMismatch(Exception):
    """Raised when a resume is requested against incompatible state."""


def _atomic_write_json(path: str, obj: dict) -> None:
    directory = os.path.dirname(os.path.abspath(path))
    os.makedirs(directory, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=directory, prefix=".ckpt-", suffix=".tmp")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            json.dump(obj, fh, sort_keys=True)
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def params_fingerprint(params: dict) -> str:
    blob = json.dumps(params, sort_keys=True).encode("utf-8")
    return hashlib.sha256(blob).hexdigest()


def manifest_path(state_dir: str) -> str:
    return os.path.join(state_dir, MANIFEST_NAME)


def stage_path(state_dir: str, stage: str) -> str:
    return os.path.join(state_dir, f"stage-{stage}.json")


def load_manifest(state_dir: str) -> dict | None:
    path = manifest_path(state_dir)
    if not os.path.exists(path):
        return None
    with open(path, "r", encoding="utf-8") as fh:
        return json.load(fh)


def open_run(state_dir: str, input_sha256: str, params: dict) -> tuple[dict, bool]:
    """Open or create a run manifest.

    Returns (manifest, fresh).  Raises CheckpointMismatch when existing
    state belongs to a different input or parameter set.
    """
    fp = params_fingerprint(params)
    manifest = load_manifest(state_dir)
    if manifest is None:
        return {
            "input_sha256": input_sha256,
            "params_fingerprint": fp,
            "stage_versions": dict(STAGE_VERSIONS),
            "stages": {},
        }, True
    if manifest.get("input_sha256") != input_sha256:
        raise CheckpointMismatch(
            "input changed: checkpoint input_sha256="
            f"{manifest.get('input_sha256')} but current input sha256={input_sha256}"
        )
    if manifest.get("params_fingerprint") != fp:
        raise CheckpointMismatch(
            "parameters changed: rerun with the original window/fallback "
            "settings or remove the state directory"
        )
    if manifest.get("stage_versions") != STAGE_VERSIONS:
        raise CheckpointMismatch("stage format versions changed")
    return manifest, False


def save_stage(state_dir: str, manifest: dict, stage: str, payload: dict) -> None:
    path = stage_path(state_dir, stage)
    blob = json.dumps(payload, sort_keys=True).encode("utf-8")
    digest = hashlib.sha256(blob).hexdigest()
    _atomic_write_json(path, payload)
    manifest["stages"][stage] = {"sha256": digest}
    _atomic_write_json(manifest_path(state_dir), manifest)


def load_stage(state_dir: str, manifest: dict, stage: str) -> dict | None:
    """Return a completed stage's payload, or None if absent/corrupt."""
    info = manifest.get("stages", {}).get(stage)
    if info is None:
        return None
    path = stage_path(state_dir, stage)
    try:
        with open(path, "r", encoding="utf-8") as fh:
            payload = json.load(fh)
    except (OSError, json.JSONDecodeError):
        return None
    blob = json.dumps(payload, sort_keys=True).encode("utf-8")
    if hashlib.sha256(blob).hexdigest() != info.get("sha256"):
        return None
    return payload
