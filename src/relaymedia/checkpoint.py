"""Atomic, content-bound checkpointing for resumable pipeline runs.

A checkpoint binds three digests — the input summary, the parameter summary
and, per completed stage, a digest of that stage's payload — to a monotonic
stage cursor. Writes go through a temp file and ``os.replace`` so a crash can
never leave a half-written checkpoint. On resume, a stage is reused only when
its stored payload still hashes to the recorded digest *and* the input and
parameter digests are unchanged; any drift raises
:class:`relaymedia.errors.CheckpointMismatch` carrying a locatable reason, so a
run can never silently continue against altered inputs.
"""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable

from .errors import CheckpointMismatch, Stage

_CHECKPOINT_NAME = "checkpoint.json"
_CHECKPOINT_VERSION = 1


def digest_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def digest_text(text: str) -> str:
    return digest_bytes(text.encode("utf-8"))


def digest_json(obj: Any) -> str:
    """Stable digest of a JSON-serialisable object (sorted keys, no whitespace drift)."""
    return digest_text(json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=False))


def digest_file(path: str | Path, chunk: int = 1 << 20) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as handle:
        for block in iter(lambda: handle.read(chunk), b""):
            h.update(block)
    return h.hexdigest()


@dataclass(slots=True)
class StageRecord:
    name: str
    payload_digest: str
    completed_at: str  # ISO-8601 UTC, informational only; not part of any digest


@dataclass(slots=True)
class Checkpoint:
    path: Path
    version: int
    input_digest: str
    params_digest: str
    cursor: int
    stages: dict[str, StageRecord] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "version": self.version,
            "input_digest": self.input_digest,
            "params_digest": self.params_digest,
            "cursor": self.cursor,
            "stages": [
                {"name": s.name, "payload_digest": s.payload_digest, "completed_at": s.completed_at}
                for s in self.stages.values()
            ],
        }

    def _write(self) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        fd, tmp = tempfile.mkstemp(prefix=".checkpoint-", dir=str(self.path.parent))
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as handle:
                json.dump(self.to_dict(), handle, sort_keys=True, indent=2)
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(tmp, self.path)
        except BaseException:
            os.unlink(tmp) if os.path.exists(tmp) else None
            raise

    def mark_stage(
        self,
        stage: Stage,
        payload: Any,
        *,
        completed_at: str,
        payload_digest: str | None = None,
    ) -> str:
        """Record a completed stage with its payload digest and advance the cursor."""
        digest = payload_digest if payload_digest is not None else digest_json(payload)
        order = list(Stage)
        idx = order.index(stage)
        record = StageRecord(name=stage.value, payload_digest=digest, completed_at=completed_at)
        self.stages[stage.value] = record
        self.cursor = max(self.cursor, idx + 1)
        self._write()
        return digest

    def is_stage_done(self, stage: Stage) -> bool:
        return stage.value in self.stages

    def stage_digest(self, stage: Stage) -> str | None:
        record = self.stages.get(stage.value)
        return record.payload_digest if record else None


def new_checkpoint(work_dir: str | Path, input_digest: str, params_digest: str) -> Checkpoint:
    return Checkpoint(
        path=Path(work_dir) / _CHECKPOINT_NAME,
        version=_CHECKPOINT_VERSION,
        input_digest=input_digest,
        params_digest=params_digest,
        cursor=0,
    )


def load_checkpoint(work_dir: str | Path) -> Checkpoint | None:
    path = Path(work_dir) / _CHECKPOINT_NAME
    if not path.exists():
        return None
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError) as exc:
        raise CheckpointMismatch(f"checkpoint unreadable: {exc}", stage=Stage.CHECKPOINT) from exc
    stages = {
        item["name"]: StageRecord(
            name=item["name"], payload_digest=item["payload_digest"],
            completed_at=item.get("completed_at", ""),
        )
        for item in raw.get("stages", [])
    }
    return Checkpoint(
        path=path,
        version=int(raw.get("version", 0)),
        input_digest=raw["input_digest"],
        params_digest=raw["params_digest"],
        cursor=int(raw["cursor"]),
        stages=stages,
    )


def resume_checkpoint(
    work_dir: str | Path,
    *,
    input_digest: str,
    params_digest: str,
) -> Checkpoint:
    """Load and validate a checkpoint for resume, refusing any digest drift."""
    checkpoint = load_checkpoint(work_dir)
    if checkpoint is None:
        raise CheckpointMismatch("no checkpoint found to resume", stage=Stage.CHECKPOINT)
    if checkpoint.version != _CHECKPOINT_VERSION:
        raise CheckpointMismatch(
            f"checkpoint version {checkpoint.version} != supported {_CHECKPOINT_VERSION}",
            stage=Stage.CHECKPOINT,
        )
    if checkpoint.input_digest != input_digest:
        raise CheckpointMismatch(
            f"input changed: stored {checkpoint.input_digest[:12]}... "
            f"!= current {input_digest[:12]}...",
            stage=Stage.CHECKPOINT,
        )
    if checkpoint.params_digest != params_digest:
        raise CheckpointMismatch(
            "parameters changed: stored and current parameter digests differ",
            stage=Stage.CHECKPOINT,
        )
    return checkpoint


def verify_stage_payload(
    checkpoint: Checkpoint, stage: Stage, load_payload: Callable[[], Any]
) -> bool:
    """Return True when a stored stage's payload still matches its digest."""
    stored = checkpoint.stage_digest(stage)
    if stored is None:
        return False
    current = digest_json(load_payload())
    if current != stored:
        raise CheckpointMismatch(
            f"stage {stage.value} payload digest {current[:12]}... "
            f"!= stored {stored[:12]}...",
            stage=Stage.CHECKPOINT,
        )
    return True
