"""Crash-safe checkpoints: stage cursors, input fingerprints and commits.

A checkpoint is a JSON document written atomically (temp file + ``os.replace``).
It records three things that must agree before any work is skipped:

* an input fingerprint (size + BLAKE2b digest of every input file),
* a configuration digest (seed, codec, bitrate, sizes, tool flags), and
* a list of committed stage boundaries, each with the unit range it covers and
  the digest of its own recorded output.

Only a stage whose recorded input offset is still covered by a validated block
may be reused.  A partial write (temp file left behind, or a commit line without
a completed payload) is never treated as a successful commit: the stage is
simply not in the committed set, so it re-runs.  Input or configuration changes
raise :class:`CheckpointConflict` naming the field that differs.
"""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable

from .errors import CheckpointConflict, MediaInputError

CHECKPOINT_VERSION = 1
#: Digest domain separator so file digests cannot be confused with other hashes.
INPUT_DIGEST_PREFIX = b"mediacore-input-v1\x00"
CONFIG_DIGEST_PREFIX = b"mediacore-config-v1\x00"


@dataclass
class InputFile:
    path: str
    size: int
    digest: str

    def to_dict(self) -> dict[str, Any]:
        return {"path": self.path, "size": self.size, "digest": self.digest}


@dataclass
class StageCommit:
    """A completed, validated stage boundary."""

    stage: str
    unit_start: int
    unit_end: int
    byte_start: int
    byte_end: int
    records: int
    digest: str
    payload: dict[str, Any] = field(default_factory=dict)

    @property
    def covered_units(self) -> int:
        return max(0, self.unit_end - self.unit_start)

    def to_dict(self) -> dict[str, Any]:
        return {
            "stage": self.stage,
            "unit_start": self.unit_start,
            "unit_end": self.unit_end,
            "byte_start": self.byte_start,
            "byte_end": self.byte_end,
            "records": self.records,
            "digest": self.digest,
            "payload": dict(self.payload),
        }

    @classmethod
    def from_dict(cls, raw: dict[str, Any]) -> "StageCommit | None":
        try:
            commit = cls(
                stage=str(raw["stage"]),
                unit_start=int(raw["unit_start"]),
                unit_end=int(raw["unit_end"]),
                byte_start=int(raw["byte_start"]),
                byte_end=int(raw["byte_end"]),
                records=int(raw["records"]),
                digest=str(raw["digest"]),
                payload=dict(raw.get("payload") or {}),
            )
        except (KeyError, TypeError, ValueError):
            return None
        if commit.unit_end < commit.unit_start or commit.records < 0:
            return None
        return commit


def compact_digest(kind: bytes, parts: Iterable[Any]) -> str:
    """BLAKE2b digest over a domain-separated, JSON-encoded sequence."""
    hasher = hashlib.blake2b(digest_size=20)
    hasher.update(kind)
    for part in parts:
        hasher.update(b"\x1f")
        encoded = part if isinstance(part, bytes) else json.dumps(
            part, sort_keys=True, separators=(",", ":")).encode("utf-8")
        hasher.update(len(encoded).to_bytes(8, "big"))
        hasher.update(encoded)
    return hasher.hexdigest()


def digest_file(path: str | Path, block: int = 1 << 20) -> tuple[str, int]:
    """Stream a file through BLAKE2b; returns (hex digest, size)."""
    hasher = hashlib.blake2b(digest_size=20)
    hasher.update(INPUT_DIGEST_PREFIX)
    size = 0
    with open(path, "rb") as handle:
        while True:
            chunk = handle.read(block)
            if not chunk:
                break
            size += len(chunk)
            hasher.update(chunk)
    return hasher.hexdigest(), size


def fingerprint_inputs(paths: Iterable[str | Path]) -> list[InputFile]:
    files: list[InputFile] = []
    for raw in paths:
        path = Path(raw)
        if not path.exists():
            raise MediaInputError(f"input not found: {path}", code="input_missing")
        digest, size = digest_file(path)
        files.append(InputFile(path=str(path), size=size, digest=digest))
    return files


def config_digest(config: dict[str, Any]) -> str:
    return compact_digest(CONFIG_DIGEST_PREFIX, [config])


@dataclass
class Checkpoint:
    """In-memory view of one checkpoint document."""

    path: Path
    inputs: list[InputFile]
    config: dict[str, Any]
    commits: list[StageCommit] = field(default_factory=list)
    runs: int = 0
    last_status: str = "new"
    version: int = CHECKPOINT_VERSION

    @property
    def input_digest(self) -> str:
        return compact_digest(
            INPUT_DIGEST_PREFIX, [[f.path, f.size, f.digest] for f in self.inputs])

    @property
    def config_digest(self) -> str:
        return config_digest(self.config)

    def stages(self) -> list[str]:
        return [c.stage for c in self.commits]

    def commit(self, stage: str, *, unit_start: int, unit_end: int,
               byte_start: int, byte_end: int, records: int,
               payload: dict[str, Any] | None = None) -> StageCommit:
        payload = dict(payload or {})
        digest = compact_digest(
            b"mediacore-stage-v1\x00",
            [stage, unit_start, unit_end, byte_start, byte_end, records, payload])
        entry = StageCommit(
            stage=stage, unit_start=unit_start, unit_end=unit_end,
            byte_start=byte_start, byte_end=byte_end, records=records,
            digest=digest, payload=payload)
        # Replacing a stage's previous commit keeps the checkpoint idempotent.
        self.commits = [c for c in self.commits if c.stage != stage]
        self.commits.append(entry)
        return entry

    def is_committed(self, stage: str) -> bool:
        return any(c.stage == stage for c in self.commits)

    def get(self, stage: str) -> StageCommit | None:
        for commit in self.commits:
            if commit.stage == stage:
                return commit
        return None

    def to_dict(self) -> dict[str, Any]:
        return {
            "version": self.version,
            "inputs": [f.to_dict() for f in self.inputs],
            "input_digest": self.input_digest,
            "config": dict(self.config),
            "config_digest": self.config_digest,
            "runs": self.runs,
            "last_status": self.last_status,
            "commits": [c.to_dict() for c in self.commits],
        }

    def save(self) -> None:
        write_json_atomic(self.path, self.to_dict())

    @classmethod
    def new(cls, path: str | Path, *, inputs: list[InputFile],
            config: dict[str, Any]) -> "Checkpoint":
        return cls(path=Path(path), inputs=inputs, config=dict(config))

    @classmethod
    def load(
        cls,
        path: str | Path,
        *,
        inputs: list[InputFile],
        config: dict[str, Any],
    ) -> "Checkpoint | None":
        """Load a checkpoint, refusing reuse when input or config changed.

        Returns ``None`` when no checkpoint exists yet.
        """
        path = Path(path)
        if not path.exists():
            return None
        try:
            raw = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            raise CheckpointConflict(
                f"checkpoint {path} is unreadable: {exc}",
                reason="unreadable") from exc
        if not isinstance(raw, dict) or raw.get("version") != CHECKPOINT_VERSION:
            raise CheckpointConflict(
                f"checkpoint {path} has unsupported version {raw.get('version')!r}",
                reason="version")
        stored_inputs = raw.get("inputs")
        if not isinstance(stored_inputs, list):
            raise CheckpointConflict("checkpoint has no input fingerprint",
                                     reason="missing_inputs")
        stored = [
            (str(item.get("path")), int(item.get("size", -1)), str(item.get("digest")))
            for item in stored_inputs if isinstance(item, dict)
        ]
        current = [(f.path, f.size, f.digest) for f in inputs]
        if stored != current:
            raise CheckpointConflict(
                "input files changed since the checkpoint was written",
                reason="inputs_changed")
        if raw.get("config_digest") != config_digest(config):
            stored_config = raw.get("config")
            differing = sorted(
                key for key in set(config) | set(stored_config or {})
                if (stored_config or {}).get(key) != config.get(key))
            raise CheckpointConflict(
                "configuration differs from the checkpointed run: "
                + ", ".join(differing), reason="config_changed")
        checkpoint = cls(
            path=path, inputs=inputs, config=dict(config),
            runs=int(raw.get("runs", 0)),
            last_status=str(raw.get("last_status", "unknown")),
        )
        commits = []
        for item in raw.get("commits") or []:
            commit = StageCommit.from_dict(item) if isinstance(item, dict) else None
            if commit is not None:
                commits.append(commit)
        checkpoint.commits = commits
        return checkpoint


def write_json_atomic(path: str | Path, payload: Any) -> None:
    """Write JSON so readers see either the old file or the complete new one."""
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    text = json.dumps(payload, sort_keys=True, indent=2) + "\n"
    handle = tempfile.NamedTemporaryFile(
        "w", encoding="utf-8", dir=str(path.parent),
        prefix=f".{path.name}.", suffix=".tmp", delete=False)
    try:
        with handle:
            handle.write(text)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(handle.name, path)
    except BaseException:
        try:
            os.unlink(handle.name)
        except OSError:
            pass
        raise


def find_partial_writes(directory: str | Path) -> list[Path]:
    """Leftover temp files from a crash; they are ignored, never resumed."""
    directory = Path(directory)
    if not directory.exists():
        return []
    return sorted(p for p in directory.glob(".*.tmp") if p.is_file())
