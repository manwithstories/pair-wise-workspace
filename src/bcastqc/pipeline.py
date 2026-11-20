"""End-to-end QC pipeline: render -> bounded queue -> export -> report.

The pipeline connects the stages and enforces the operating contract:

* **Bounded memory** - frames travel in fixed-size blocks through a queue with
  a hard capacity, so peak memory is independent of material length.  A
  consumer (the PNG writer) that stalls beyond ``slow_consumer_timeout``
  raises a resource error instead of letting decoded frames pile up.
* **Determinism** - output frames and the manifest are byte-identical across
  runs for the same inputs, timeline and parameters.
* **Resumability** - a run interrupted between complete blocks can continue
  from the first missing block.  The manifest records the input digest,
  timeline digest and parameter digest; any mismatch is rejected rather than
  silently reusing stale output.
* **Diagnosability** - every failure closes files, leaves no partial block
  that a resume would trust, and surfaces a serialisable :class:`QcError`.
"""

from __future__ import annotations

import hashlib
import json
import os
import time
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path
from typing import Iterator

import numpy as np

from . import EXIT_MEDIA_UNRECOVERABLE, EXIT_OK
from .bitstream import file_digest_slices
from .decode import _read_access_units
from .decode import LossModel, LossPolicy
from .errors import ErrorCategory, ResourceError, make_error
from .render import Renderer, RenderedFrame, SyncTracker, pixel_hash
from .report import (
    LatencyRecorder,
    alignment_from_duplicates,
    measure_av_placement,
    measure_placement,
    QcReport,
    Thresholds,
    build_report,
    derive_exit_code,
    evaluate_thresholds,
    sync_errors_from_tracker,
)
from .stream import CancellationToken, pipeline as run_stages
from .timeline import Timeline

__all__ = [
    "RunParams",
    "PipelineResult",
]

MANIFEST_VERSION = 2
BLOCK_FRAMES = 8


@dataclass(frozen=True)
class RunParams:
    """Parameters that affect output; digested for resume compatibility."""

    seed: int = 41
    drop_rate: float = 0.0
    hard_loss_rate: float = 0.0
    block_frames: int = BLOCK_FRAMES
    queue_blocks: int = 1
    slow_consumer_timeout: float | None = 30.0
    # artificial per-block consumer pause, used to demonstrate back-pressure
    consumer_delay_ms: float = 0.0
    # render the loss-free pass as an alignment reference (doubles render work
    # but makes duplication/misplacement measurement independent of audio)
    reference: bool = False
    # clean pixel hashes of the same timeline, supplied by the caller for
    # placement verification (the demo runs a clean pass and passes them in)
    reference_hashes: tuple[str, ...] = ()
    reference_signals: tuple[tuple[float, ...], tuple[float, ...]] | None = None

    def __post_init__(self) -> None:
        if not isinstance(self.reference_hashes, tuple):
            object.__setattr__(self, "reference_hashes", tuple(self.reference_hashes))

    def to_json(self) -> dict:
        return {
            "seed": self.seed,
            "drop_rate": self.drop_rate,
            "hard_loss_rate": self.hard_loss_rate,
            "block_frames": self.block_frames,
            "queue_blocks": self.queue_blocks,
            "consumer_delay_ms": self.consumer_delay_ms,
            "reference": self.reference,
        }

    def digest(self) -> str:
        payload = json.dumps(self.to_json(), sort_keys=True)
        return hashlib.sha256(payload.encode()).hexdigest()


@dataclass
class PipelineResult:
    report: QcReport
    manifest_path: Path
    output_dir: Path
    verification: dict = field(default_factory=dict)
    resumed_from_block: int = 0
    frames_written: int = 0
    elapsed_seconds: float = 0.0

    def to_json(self) -> dict:
        payload = self.report.to_json()
        payload.update(
            {
                "manifest": str(self.manifest_path),
                "output_dir": str(self.output_dir),
                "resumed_from_block": self.resumed_from_block,
                "frames_written": self.frames_written,
                "elapsed_seconds": round(self.elapsed_seconds, 3),
            }
        )
        if self.verification:
            payload["verification"] = self.verification
        return payload
