"""Atomic checkpoints and per-node input digests.

A checkpoint answers one question on restart: *which completed nodes can I
trust without redoing the work?*  Trust requires two things:

* the node's **output** still hashes to what the checkpoint recorded, and
* the node's **inputs** — source file, track, clip bounds, audio settings and
  the digests of its upstream nodes — are unchanged.

Anything else is recomputed.  That is what makes "resume is identical to a
full run" a checkable property rather than a hope: a node whose input digest
matches is by construction the same computation, and the pipeline still
verifies the output hash after reuse.

Writes are atomic (temp file in the same directory, ``os.replace``) and
checksummed, so a process killed mid-write leaves either the previous
checkpoint or the new one, never a torn file.
"""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Mapping

from .errors import CheckpointError
from .integrity import file_sha256

__all__ = [
    "Checkpoint",
    "NodeRecord",
    "write_checkpoint",
    "read_checkpoint",
    "digest_payload",
    "CHECKPOINT_SCHEMA",
]

CHECKPOINT_SCHEMA = 1


def digest_payload(payload: Any) -> str:
    """Stable SHA-256 over a JSON-able payload.

    ``sort_keys`` and compact separators make the digest independent of dict
    insertion order, so semantically equal inputs always match.
    """
    canonical = json.dumps(payload, sort_keys=True, separators=(",", ":"), default=str)
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


@dataclass
class NodeRecord:
    """What a checkpoint knows about one completed node."""

    node_id: str
    kind: str
    input_digest: str
    output_path: str
    output_sha256: str
    output_samples: int
    deps: list[str] = field(default_factory=list)
    dep_digests: dict[str, str] = field(default_factory=dict)
    extra: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "node_id": self.node_id,
            "kind": self.kind,
            "input_digest": self.input_digest,
            "output_path": self.output_path,
            "output_sha256": self.output_sha256,
            "output_samples": self.output_samples,
            "deps": list(self.deps),
            "dep_digests": dict(self.dep_digests),
            "extra": dict(self.extra),
        }

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> "NodeRecord":
        return cls(
            node_id=str(data["node_id"]),
            kind=str(data["kind"]),
            input_digest=str(data["input_digest"]),
            output_path=str(data["output_path"]),
            output_sha256=str(data["output_sha256"]),
            output_samples=int(data["output_samples"]),
            deps=list(data.get("deps") or []),
            dep_digests=dict(data.get("dep_digests") or {}),
            extra=dict(data.get("extra") or {}),
        )


@dataclass
class Checkpoint:
    """In-memory checkpoint state."""

    path: Path
    timeline_digest: str
    settings_digest: str
    records: dict[str, NodeRecord] = field(default_factory=dict)

    def record(self, node: NodeRecord) -> None:
        self.records[node.node_id] = node

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema": CHECKPOINT_SCHEMA,
            "timeline_digest": self.timeline_digest,
            "settings_digest": self.settings_digest,
            "records": {
                node_id: rec.to_dict()
                for node_id, rec in sorted(self.records.items())
            },
        }

    def payload_digest(self) -> str:
        return digest_payload(
            {
                "schema": CHECKPOINT_SCHEMA,
                "timeline_digest": self.timeline_digest,
                "settings_digest": self.settings_digest,
                "records": {
                    k: v.to_dict() for k, v in sorted(self.records.items())
                },
            }
        )


def write_checkpoint(checkpoint: Checkpoint) -> Path:
    """Atomically write ``checkpoint`` to its path."""
    payload = checkpoint.to_dict()
    payload["digest"] = checkpoint.payload_digest()
    text = json.dumps(payload, indent=2, sort_keys=True) + "\n"
    path = Path(checkpoint.path)
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp_name = tempfile.mkstemp(prefix=".ckpt-", dir=str(path.parent))
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            fh.write(text)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp_name, path)
    except BaseException:
        try:
            os.unlink(tmp_name)
        except OSError:
            pass
        raise
    # fsync the directory so the rename survives a crash.
    try:
        dir_fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    except OSError:
        pass
    return path


def read_checkpoint(path: str | Path) -> Checkpoint | None:
    """Load and validate a checkpoint; ``None`` if it does not exist.

    A corrupt, truncated or schema-mismatched checkpoint is *not* fatal: it is
    reported as a recoverable :class:`CheckpointError` and the caller restarts
    from scratch.  Failing hard here would defeat the purpose of a checkpoint.
    """
    path = Path(path)
    if not path.is_file():
        return None
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise CheckpointError(
            "checkpoint unreadable; restarting from scratch",
            path=str(path),
            reason=str(exc)[:200],
        ) from exc
    if not isinstance(data, dict) or data.get("schema") != CHECKPOINT_SCHEMA:
        raise CheckpointError(
            "checkpoint schema mismatch; restarting from scratch",
            path=str(path),
            found_schema=data.get("schema") if isinstance(data, dict) else None,
            expected_schema=CHECKPOINT_SCHEMA,
        )
    stored_digest = data.pop("digest", None)
    checkpoint = Checkpoint(
        path=path,
        timeline_digest=str(data.get("timeline_digest", "")),
        settings_digest=str(data.get("settings_digest", "")),
    )
    for node_id, raw in (data.get("records") or {}).items():
        try:
            checkpoint.record(NodeRecord.from_dict(raw))
        except (KeyError, TypeError, ValueError) as exc:
            raise CheckpointError(
                "checkpoint record malformed; restarting from scratch",
                path=str(path),
                node=node_id,
                reason=str(exc)[:160],
            ) from exc
    actual = checkpoint.payload_digest()
    if stored_digest is not None and actual != stored_digest:
        raise CheckpointError(
            "checkpoint digest mismatch; restarting from scratch",
            path=str(path),
            expected=stored_digest,
            actual=actual,
        )
    return checkpoint


def verify_record(record: NodeRecord, *, root: str | Path) -> tuple[bool, str]:
    """Check that a record's output file still matches its digest."""
    output = Path(record.output_path)
    if not output.is_absolute():
        output = Path(root) / output
    if not output.is_file():
        return False, "missing_output"
    actual = file_sha256(output)
    if actual != record.output_sha256:
        return False, "output_digest_mismatch"
    return True, "ok"
