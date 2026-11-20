"""Pipeline value objects: settings, per-node outcome, run result.

Kept separate from the runner so reports and tests can depend on the result
shape without importing the orchestration code.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path
from typing import TYPE_CHECKING

from .checkpoint import digest_payload
from .probe import DamageReport

if TYPE_CHECKING:  # pragma: no cover
    from .graph import Plan

class Interrupted(RuntimeError):
    """Raised when the simulated interruption budget is exhausted.

    This is a *controlled* stop: the checkpoint is already on disk and the
    process exits with a dedicated code so the harness can resume.
    """

    def __init__(self, completed: int) -> None:
        super().__init__(f"interrupted after {completed} nodes")
        self.completed = completed


@dataclass(frozen=True)
class PipelineSettings:
    sample_rate: int = 48_000
    layout: str = "stereo"
    target_lufs: float | None = -16.0
    ceiling_dbfs: float = -1.0
    workdir: str = "."
    force: bool = False

    def digest(self) -> str:
        return digest_payload(
            {
                "sample_rate": self.sample_rate,
                "layout": self.layout,
                "target_lufs": self.target_lufs,
                "ceiling_dbfs": self.ceiling_dbfs,
            }
        )


@dataclass
class NodeOutcome:
    node_id: str
    reused: bool
    path: Path
    sha256: str
    samples: int
    loudness: dict[str, object] = field(default_factory=dict)

    def to_dict(self) -> dict[str, object]:
        return {
            "node_id": self.node_id,
            "reused": self.reused,
            "path": str(self.path),
            "sha256": self.sha256,
            "samples": self.samples,
            "loudness": self.loudness,
        }


@dataclass
class RunResult:
    plan: Plan
    outcomes: dict[str, NodeOutcome]
    damage: DamageReport
    output_path: Path
    output_sha256: str
    output_samples: int
    reused_nodes: int
    rendered_nodes: int
    recovery_seconds: float
    skipped: list[str] = field(default_factory=list)
    failed: dict[str, str] = field(default_factory=dict)
    interrupted: bool = False

    @property
    def damaged(self) -> bool:
        return self.damage.any_damage()

    def to_dict(self) -> dict[str, object]:
        return {
            "output_path": str(self.output_path),
            "output_sha256": self.output_sha256,
            "output_samples": self.output_samples,
            "reused_nodes": self.reused_nodes,
            "rendered_nodes": self.rendered_nodes,
            "recovery_seconds": round(self.recovery_seconds, 6),
            "skipped": self.skipped,
            "failed": self.failed,
            "interrupted": self.interrupted,
            "nodes": {k: v.to_dict() for k, v in sorted(self.outcomes.items())},
        }

