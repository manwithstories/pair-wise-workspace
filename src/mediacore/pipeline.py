"""Pipeline orchestration: probe, plan, render, resume, assemble.

Each step does one job: probe tracks and record damage; build the dependency
plan (cycles fail before work); for each ready node either reuse a checkpoint
record (digests match, output hash verifies) or render it; assemble segments
per track; verify and report.

Resume is correct by construction: a reused node's input digest covers source
content, clip bounds, audio settings and upstream digests, so digest equality
means the same samples.  Interruption (``stop_after``) is a controlled stop
that leaves a consistent checkpoint for the resumed run to reuse.
"""

from __future__ import annotations

import time
from pathlib import Path
from typing import Callable

from .audio import render_clip
from .audio_io import render_blend
from .checkpoint import write_checkpoint
from .errors import AudioError
from .graph import Plan, build_plan
from .integrity import file_sha256 as source_sha256
from .pipeline_types import (
    Interrupted,
    NodeOutcome,
    PipelineSettings,
    RunResult,
)
from .pipeline_assembly import assemble_output, safe_name
from .pipeline_checkpoint import Digester, load_checkpoint_soft
from .probe import scan_tracks
from .timeline import Timeline

__all__ = [
    "PipelineSettings",
    "NodeOutcome",
    "RunResult",
    "Interrupted",
    "Pipeline",
]




class Pipeline:
    """Stateful runner; one instance per workdir invocation."""

    def __init__(
        self,
        timeline: Timeline,
        settings: PipelineSettings,
        *,
        stop_after: int | None = None,
        on_node: Callable[[NodeOutcome], None] | None = None,
    ) -> None:
        self.timeline = timeline
        self.settings = settings
        self.stop_after = stop_after
        self.on_node = on_node
        self.workdir = Path(settings.workdir).resolve()
        self.segments_dir = self.workdir / "segments"
        self.checkpoint_path = self.workdir / "checkpoint.json"
        self.source_hashes: dict[str, str] = {}
        self._plan: Plan | None = None
        self.digester: Digester | None = None

    # -- public -------------------------------------------------------------
    def run(self) -> RunResult:
        start = time.perf_counter()
        self.segments_dir.mkdir(parents=True, exist_ok=True)

        damage = self._probe()
        # Validate clips against what was actually measured.  Out-of-range or
        # empty clips fail here, before any rendering.
        self.timeline.validate(durations_us=self._measured_durations(damage))

        self._plan = build_plan(self.timeline)
        self.digester = Digester(
            self.timeline, self.settings, self.source_hashes, self.workdir
        )
        checkpoint = load_checkpoint_soft(self.checkpoint_path)
        settings_digest = self.settings.digest()
        if checkpoint is not None and (
            checkpoint.timeline_digest != self.digester.timeline_digest()
            or checkpoint.settings_digest != settings_digest
        ):
            checkpoint = None  # inputs changed: old records describe another run
        if checkpoint is None:
            checkpoint = self.digester.fresh_checkpoint(self.checkpoint_path)

        outcomes: dict[str, NodeOutcome] = {}
        reused = 0
        rendered = 0
        interrupted = False
        checkpoint_changed = False

        try:
            while not self._plan.complete:
                ready = self._plan.ready_nodes()
                if not ready:
                    break
                for node_id in ready:
                    outcome = self._process_node(node_id, checkpoint)
                    if outcome.reused:
                        reused += 1
                    else:
                        rendered += 1
                        node = self._plan.node(node_id)
                        checkpoint.record(
                            self.digester.record_for(
                                node_id, outcome, deps=list(node.deps)
                            )
                        )
                        write_checkpoint(checkpoint)
                        checkpoint_changed = True
                        if self.stop_after is not None and rendered >= self.stop_after:
                            outcomes[node_id] = outcome
                            self._plan.mark_done(node_id)
                            raise Interrupted(rendered)
                    outcomes[node_id] = outcome
                    self._plan.mark_done(node_id)
                    if self.on_node is not None:
                        self.on_node(outcome)
        except Interrupted:
            interrupted = True
            # Persist whatever was completed before the stop.
            if checkpoint_changed:
                write_checkpoint(checkpoint)
            output = self._interrupted_result(outcomes, damage, reused, rendered, start)
            return output

        skipped = [n for n, s in self._plan.states().items() if s == "skipped"]
        failed = {
            n: r for n, r in self._plan.failure_reasons().items()
            if self._plan.state(n) == "failed"
        }

        output_path, output_sha, output_samples = assemble_output(
            self.timeline,
            outcomes,
            output_path=self.workdir / "output.wav",
            sample_rate=self.settings.sample_rate,
            layout=self.settings.layout,
        )
        elapsed = time.perf_counter() - start
        write_checkpoint(checkpoint)
        return RunResult(
            plan=self._plan,
            outcomes=outcomes,
            damage=damage,
            output_path=output_path,
            output_sha256=output_sha,
            output_samples=output_samples,
            reused_nodes=reused,
            rendered_nodes=rendered,
            recovery_seconds=elapsed,
            skipped=skipped,
            failed=failed,
        )

    # -- probing ------------------------------------------------------------
    def _probe(self) -> DamageReport:
        tracks = list(self.timeline.tracks.values())
        for track in tracks:
            self.source_hashes[track.id] = source_sha256(track.path)
        return scan_tracks(tracks)

    def _measured_durations(self, damage: DamageReport) -> dict[str, int]:
        durations: dict[str, int] = {}
        for track in self.timeline.tracks.values():
            scan = damage.scans.get(track.id)
            if scan is None:
                continue
            # For truncated tracks, trust only the media that actually
            # demuxed: rendering past it would fail anyway, and reporting a
            # clear out-of-range error beats a decoder error.
            usable = scan.end_us
            if any(d.code.endswith("truncated") or d.code.endswith("index_missing")
                   for d in scan.damage):
                usable = max(scan.last_pts_us, 0)
            durations[track.id] = usable
        return durations

    # -- checkpoint ---------------------------------------------------------





    # -- node processing ----------------------------------------------------
    def _process_node(self, node_id: str, checkpoint: Checkpoint) -> NodeOutcome:
        record = self.digester.reusable(node_id, checkpoint)
        if record is not None:
            return NodeOutcome(
                node_id=node_id,
                reused=True,
                path=Path(record.output_path),
                sha256=record.output_sha256,
                samples=record.output_samples,
                loudness=dict(record.extra.get("loudness") or {}),
            )
        kind, _, ident = node_id.partition(":")
        try:
            if kind == "clip":
                return self._render_clip_node(ident, node_id)
            return self._render_transition_node(ident, node_id)
        except Exception as exc:
            self._plan.fail(node_id, f"{type(exc).__name__}: {exc}")
            raise

    def _render_clip_node(self, clip_id: str, node_id: str) -> NodeOutcome:
        clip = self.timeline.clips[clip_id]
        track = self.timeline.tracks[clip.track]
        if track.kind != "audio":
            # Video tracks are probed and validated but not re-encoded: the
            # deliverable is audio segments with exact boundaries.
            raise AudioError(
                "only audio tracks are rendered", track=track.id, kind=track.kind
            )
        output = self.segments_dir / f"{safe_name(clip_id)}.wav"
        result = render_clip(
            track.path,
            output,
            stream_index=track.stream_index,
            start_us=clip.start_us,
            end_us=clip.end_us,
            sample_rate=self.settings.sample_rate,
            layout=self.settings.layout,  # type: ignore[arg-type]
            target_lufs=self.settings.target_lufs,
            ceiling_dbfs=self.settings.ceiling_dbfs,
        )
        return NodeOutcome(
            node_id=node_id,
            reused=False,
            path=result.path,
            sha256=result.sha256,
            samples=result.samples,
            loudness=result.loudness.to_dict(),
        )

    def _render_transition_node(self, transition_id: str, node_id: str) -> NodeOutcome:
        transition = self.timeline.transitions[transition_id]
        left = self.segments_dir / f"{safe_name(transition.left)}.wav"
        right = self.segments_dir / f"{safe_name(transition.right)}.wav"
        output = self.segments_dir / f"transition-{safe_name(transition_id)}.wav"
        result = render_blend(
            left,
            right,
            output,
            duration_us=transition.duration_us,
            sample_rate=self.settings.sample_rate,
            curve=transition.curve,
        )
        return NodeOutcome(
            node_id=node_id,
            reused=False,
            path=result.path,
            sha256=result.sha256,
            samples=result.samples,
            loudness=result.loudness.to_dict(),
        )

    # -- assembly -----------------------------------------------------------


    def _interrupted_result(
        self,
        outcomes: dict[str, NodeOutcome],
        damage: DamageReport,
        reused: int,
        rendered: int,
        start: float,
    ) -> RunResult:
        placeholder = self.workdir / "output.wav"
        return RunResult(
            plan=self._plan,  # type: ignore[arg-type]
            outcomes=outcomes,
            damage=damage,
            output_path=placeholder,
            output_sha256="",
            output_samples=0,
            reused_nodes=reused,
            rendered_nodes=rendered,
            recovery_seconds=time.perf_counter() - start,
            interrupted=True,
        )




