"""Atomic checkpointing for resumable stages.

A checkpoint records:

* the input digest (configuration + source file digests),
* the stage it belongs to and the completed stage list,
* the digest and byte size of every produced artifact,
* the resume point (which stage to run next).

Saving is atomic: the document is written to a temporary file in the same
directory, flushed and fsynced, then renamed over the target.  Loading verifies
the format version, the input digest and every artifact digest.  A checkpoint
whose artifacts no longer match (partial write, edited source, changed config)
is rejected and the caller redoes from the earliest invalid stage.
"""

from __future__ import annotations

import json
import os
import tempfile
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable

from .errors import Stage
from .probe import file_sha256

CHECKPOINT_VERSION = 2
CHECKPOINT_NAME = "checkpoint.json"


class CheckpointError(ValueError):
    """Raised when a checkpoint cannot be trusted."""


@dataclass(frozen=True)
class Artifact:
    stage: Stage
    path: str
    sha256: str
    size: int

    def to_dict(self, root: str | None = None) -> dict[str, Any]:
        path = os.path.relpath(self.path, root) if root else self.path
        return {
            "stage": self.stage.label,
            "path": path,
            "sha256": self.sha256,
            "size": self.size,
        }

    @classmethod
    def from_path(cls, stage: Stage, path: str, root: str | None = None) -> "Artifact":
        return cls(stage=stage, path=path, sha256=file_sha256(path), size=os.path.getsize(path))


@dataclass
class Checkpoint:
    """A verified-or-not record of completed work."""

    input_digest: str
    completed: list[Stage] = field(default_factory=list)
    artifacts: list[Artifact] = field(default_factory=list)
    resume_stage: Stage | None = None
    version: int = CHECKPOINT_VERSION
    created_by: str = "avtimeline"

    def mark_complete(self, stage: Stage, artifacts: Iterable[Artifact]) -> None:
        stages = set(self.completed)
        stages.add(stage)
        self.completed = sorted(stages)
        self.artifacts.extend(a for a in artifacts if a.stage is stage)
        next_stage = _next_stage(stage)
        self.resume_stage = next_stage

    def artifact_for(self, stage: Stage) -> list[Artifact]:
        return [a for a in self.artifacts if a.stage is stage]

    def to_dict(self, root: str | None = None) -> dict[str, Any]:
        return {
            "version": self.version,
            "created_by": self.created_by,
            "input_digest": self.input_digest,
            "completed": [stage.label for stage in self.completed],
            "resume_stage": self.resume_stage.label if self.resume_stage else None,
            "artifacts": [a.to_dict(root) for a in self.artifacts],
        }

    @classmethod
    def from_dict(cls, payload: dict[str, Any], root: str | None = None) -> "Checkpoint":
        version = int(payload.get("version", 0))
        if version != CHECKPOINT_VERSION:
            raise CheckpointError(f"unsupported checkpoint version {version}")
        try:
            completed = [Stage[name.upper()] for name in payload.get("completed", [])]
        except KeyError as exc:
            raise CheckpointError(f"unknown stage in checkpoint: {exc}") from exc
        artifacts: list[Artifact] = []
        for raw in payload.get("artifacts", []):
            try:
                stage = Stage[raw["stage"].upper()]
            except KeyError as exc:
                raise CheckpointError(f"unknown artifact stage {raw['stage']!r}") from exc
            path = raw["path"]
            if root and not os.path.isabs(path):
                path = os.path.join(root, path)
            artifacts.append(
                Artifact(
                    stage=stage,
                    path=path,
                    sha256=raw["sha256"],
                    size=int(raw.get("size", 0)),
                )
            )
        resume_raw = payload.get("resume_stage")
        resume = None
        if resume_raw:
            try:
                resume = Stage[resume_raw.upper()]
            except KeyError as exc:
                raise CheckpointError(f"unknown resume stage {resume_raw!r}") from exc
        return cls(
            input_digest=payload.get("input_digest", ""),
            completed=completed,
            artifacts=artifacts,
            resume_stage=resume,
            version=version,
            created_by=payload.get("created_by", "avtimeline"),
        )


_STAGE_ORDER = [
    Stage.TIMELINE,
    Stage.PROBE,
    Stage.NORMALIZE,
    Stage.TRANSITION,
    Stage.SEGMENT,
    Stage.ENCODE,
    Stage.MANIFEST,
]


def _next_stage(stage: Stage) -> Stage | None:
    try:
        index = _STAGE_ORDER.index(stage)
    except ValueError:  # pragma: no cover - every stage is listed
        return None
    if index + 1 >= len(_STAGE_ORDER):
        return None
    return _STAGE_ORDER[index + 1]


def checkpoint_path(workdir: str) -> str:
    return os.path.join(workdir, CHECKPOINT_NAME)


def save_checkpoint(checkpoint: Checkpoint, workdir: str) -> str:
    """Atomically write the checkpoint document into *workdir*."""
    os.makedirs(workdir, exist_ok=True)
    target = checkpoint_path(workdir)
    payload = json.dumps(checkpoint.to_dict(root=workdir), indent=2, sort_keys=True)
    fd, temp = tempfile.mkstemp(prefix=".checkpoint-", suffix=".json", dir=workdir)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp, target)
        _fsync_dir(workdir)
    finally:
        if os.path.exists(temp):
            os.unlink(temp)
    return target


def _fsync_dir(directory: str) -> None:
    try:
        fd = os.open(directory, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(fd)
    except OSError:
        pass
    finally:
        os.close(fd)


class CheckpointStatus:
    VALID = "valid"
    MISSING = "missing"
    STALE_INPUT = "stale_input"
    BROKEN_ARTIFACT = "broken_artifact"
    INVALID_FORMAT = "invalid_format"


@dataclass
class CheckpointLoadResult:
    checkpoint: Checkpoint | None
    status: str
    reason: str = ""
    first_invalid_stage: Stage | None = None


def _stage_index(stage: Stage) -> int:
    try:
        return _STAGE_ORDER.index(stage)
    except ValueError:  # pragma: no cover
        return len(_STAGE_ORDER)


def load_checkpoint(
    workdir: str, input_digest: str | None = None
) -> CheckpointLoadResult:
    """Load and verify a checkpoint, reporting the earliest invalid stage."""
    target = checkpoint_path(workdir)
    if not os.path.exists(target):
        return CheckpointLoadResult(None, CheckpointStatus.MISSING, "no checkpoint")
    try:
        with open(target, "r", encoding="utf-8") as handle:
            payload = json.load(handle)
        checkpoint = Checkpoint.from_dict(payload, root=workdir)
    except (OSError, json.JSONDecodeError, CheckpointError, KeyError, TypeError) as exc:
        return CheckpointLoadResult(None, CheckpointStatus.INVALID_FORMAT, str(exc))
    if input_digest is not None and checkpoint.input_digest != input_digest:
        return CheckpointLoadResult(
            checkpoint,
            CheckpointStatus.STALE_INPUT,
            "input digest changed since checkpoint",
            first_invalid_stage=Stage.TIMELINE,
        )
    # Artifacts are grouped by stage; the earliest stage with a missing or
    # mismatching artifact determines where processing must resume.
    broken: Artifact | None = None
    for artifact in sorted(checkpoint.artifacts, key=lambda a: (_stage_index(a.stage), a.path)):
        if not os.path.exists(artifact.path):
            broken = artifact
            break
        if os.path.getsize(artifact.path) != artifact.size:
            broken = artifact
            break
        if file_sha256(artifact.path) != artifact.sha256:
            broken = artifact
            break
    if broken is not None:
        return CheckpointLoadResult(
            checkpoint,
            CheckpointStatus.BROKEN_ARTIFACT,
            f"artifact {os.path.basename(broken.path)} does not match its digest",
            first_invalid_stage=broken.stage,
        )
    return CheckpointLoadResult(checkpoint, CheckpointStatus.VALID)


def input_digest(config: dict[str, Any], files: Iterable[str]) -> str:
    """Digest of configuration plus source file contents."""
    import hashlib

    digest = hashlib.sha256()
    digest.update(json.dumps(config, sort_keys=True, default=str).encode("utf-8"))
    for path in sorted(files):
        digest.update(b"\0")
        digest.update(os.path.basename(path).encode("utf-8"))
        digest.update(b"\0")
        digest.update(file_sha256(path).encode("ascii"))
    return digest.hexdigest()


def reusable_stages(
    checkpoint: Checkpoint | None, status: str, invalid_stage: Stage | None
) -> list[Stage]:
    """Stages whose artifacts can be reused for this run."""
    if checkpoint is None or status not in (CheckpointStatus.VALID, CheckpointStatus.BROKEN_ARTIFACT):
        return []
    completed = list(checkpoint.completed)
    if status == CheckpointStatus.VALID:
        return sorted(completed, key=_stage_index)
    if invalid_stage is None:
        return []
    cutoff = _stage_index(invalid_stage)
    return sorted((s for s in completed if _stage_index(s) < cutoff), key=_stage_index)


def describe(result: CheckpointLoadResult) -> dict[str, Any]:
    return {
        "status": result.status,
        "reason": result.reason,
        "completed": [s.label for s in result.checkpoint.completed]
        if result.checkpoint
        else [],
        "first_invalid_stage": result.first_invalid_stage.label
        if result.first_invalid_stage
        else None,
        "path": None,
    }


def relative(path: str, root: str) -> str:
    return str(Path(path).resolve().relative_to(Path(root).resolve())) if root else path
