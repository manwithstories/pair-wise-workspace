"""Checkpoint bookkeeping: input digests, reuse decisions, records.

Isolating this from the runner makes the reuse rule auditable in one place:
a node is reusable iff its *input digest* matches, all *dependency digests*
match, and its output file still hashes to the recorded value.  Anything else
is recomputed.  Because the input digest covers source content, clip bounds,
audio settings and upstream digests, digest equality implies the same
computation — which is what makes resume identical to a full run.
"""

from __future__ import annotations

from pathlib import Path
from typing import TYPE_CHECKING

from .checkpoint import (
    Checkpoint,
    NodeRecord,
    digest_payload,
    read_checkpoint,
    verify_record,
)
from .timeline import Timeline

if TYPE_CHECKING:  # pragma: no cover
    from .pipeline_types import NodeOutcome, PipelineSettings

__all__ = ["Digester", "load_checkpoint_soft"]


class Digester:
    """Computes node input digests and answers reuse questions."""

    def __init__(
        self,
        timeline: Timeline,
        settings: "PipelineSettings",
        source_hashes: dict[str, str],
        workdir: Path,
    ) -> None:
        self.timeline = timeline
        self.settings = settings
        self.source_hashes = source_hashes
        self.workdir = workdir

    def timeline_digest(self) -> str:
        return self.timeline.fingerprint()

    def input_digest(self, node_id: str) -> str:
        kind, _, ident = node_id.partition(":")
        if kind == "clip":
            clip = self.timeline.clips[ident]
            track = self.timeline.tracks[clip.track]
            return digest_payload(
                {
                    "kind": "clip",
                    "clip": clip.to_dict(),
                    "track": track.to_dict(),
                    "source_sha256": self.source_hashes.get(track.id, ""),
                    "audio": self.settings.digest(),
                }
            )
        transition = self.timeline.transitions[ident]
        return digest_payload(
            {
                "kind": "transition",
                "transition": transition.to_dict(),
                "inputs": {
                    dep: self.input_digest(dep)
                    for dep in (f"clip:{transition.left}", f"clip:{transition.right}")
                },
                "audio": self.settings.digest(),
            }
        )

    def record_for(self, node_id: str, outcome: "NodeOutcome", *, deps: list[str]) -> NodeRecord:
        kind = node_id.partition(":")[0]
        return NodeRecord(
            node_id=node_id,
            kind=kind,
            input_digest=self.input_digest(node_id),
            output_path=str(outcome.path),
            output_sha256=outcome.sha256,
            output_samples=outcome.samples,
            deps=deps,
            dep_digests={dep: self.input_digest(dep) for dep in deps},
            extra={"loudness": outcome.loudness},
        )

    def reusable(self, node_id: str, checkpoint: Checkpoint) -> NodeRecord | None:
        record = checkpoint.records.get(node_id)
        if record is None or record.input_digest != self.input_digest(node_id):
            return None
        if any(record.dep_digests.get(dep) != self.input_digest(dep) for dep in record.deps):
            return None
        ok, _reason = verify_record(record, root=self.workdir)
        return record if ok else None

    def fresh_checkpoint(self, path: Path) -> Checkpoint:
        return Checkpoint(
            path=path,
            timeline_digest=self.timeline_digest(),
            settings_digest=self.settings.digest(),
        )


def load_checkpoint_soft(path: Path) -> Checkpoint | None:
    """Load a checkpoint, treating any corruption as 'no checkpoint'."""
    try:
        return read_checkpoint(path)
    except Exception:
        return None
