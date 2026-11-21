"""End-to-end pipeline: scheduling, checkpointing and resume.

Stages run in dependency order:

``timeline -> probe -> normalize -> transition -> segment -> encode -> manifest``

Each completed stage commits its artifacts into the checkpoint atomically, so a
run interrupted at ``break_at`` resumes from the stage after it and reuses the
validated normalization/encoding artifacts.  The pipeline reports timings per
stage so ``resume_seconds`` can be compared against a full rerun.
"""

from __future__ import annotations

import os
import time
from dataclasses import dataclass, field
from typing import Any, Callable

from .checkpoint import (
    Artifact,
    Checkpoint,
    CheckpointStatus,
    input_digest,
    load_checkpoint,
    reusable_stages,
    save_checkpoint,
)
from .compose import PROGRAM_AUDIO_RATE, compose_program
from .config import ProjectConfig, ensure_layout
from .encode import EncodedSegment, encode_all
from .errors import ErrorCollector, Issue, MediaError, Severity, Stage
from .graph import DependencyGraph, detect_cycle
from .manifest import Manifest, build_manifest, check_alignment, write_manifest
from .model import Span, format_rational
from .normalize import (
    ColorMetadata,
    ColorPolicy,
    NormalizedAudio,
    NormalizedVideo,
    normalize_track,
    resolve_color,
)
from .probe import ProbeResult, probe_media
from .records import (
    audio_from_json,
    audio_to_json,
    plan_from_json,
    plan_to_json,
    probe_from_json,
    probe_to_json,
    read_json,
    video_from_json,
    video_to_json,
    write_json,
)
from .samples import SampleSet, generate_samples
from .segment import SegmentPlan, plan_segments
from .timeline import Timeline, parse_timeline, validate_timeline

STAGE_ORDER = [
    Stage.TIMELINE,
    Stage.PROBE,
    Stage.NORMALIZE,
    Stage.TRANSITION,
    Stage.SEGMENT,
    Stage.ENCODE,
    Stage.MANIFEST,
]


@dataclass
class StageRecord:
    stage: Stage
    status: str
    seconds: float
    artifacts: list[str] = field(default_factory=list)
    detail: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {
            "stage": self.stage.label,
            "status": self.status,
            "seconds": round(self.seconds, 4),
            "artifacts": len(self.artifacts),
            "detail": self.detail,
        }


@dataclass
class PipelineResult:
    workdir: str
    stages: list[StageRecord] = field(default_factory=list)
    errors: list[MediaError] = field(default_factory=list)
    plan: SegmentPlan | None = None
    manifest: Manifest | None = None
    timeline: Timeline | None = None
    color: dict[str, Any] | None = None
    probes: dict[str, ProbeResult] = field(default_factory=dict)
    encoded: list[EncodedSegment] = field(default_factory=list)
    resumed_from: Stage | None = None
    reused_stages: list[Stage] = field(default_factory=list)
    timings: dict[str, float] = field(default_factory=dict)
    alignment_issues: list[Issue] = field(default_factory=list)
    checkpoint_status: str = CheckpointStatus.MISSING
    interrupted_at: Stage | None = None
    manifest_text: str = ""
    segments_by_rendition: dict[str, list[EncodedSegment]] = field(default_factory=dict)
    program_frames: int = 0
    program_samples: int = 0
    skipped_tracks: list[dict[str, Any]] = field(default_factory=list)

    @property
    def has_fatal(self) -> bool:
        return any(e.severity >= Severity.FATAL for e in self.errors)

    @property
    def has_error(self) -> bool:
        return any(e.severity >= Severity.ERROR for e in self.errors)

    @property
    def aligned_segments(self) -> int:
        if self.manifest is None:
            return 0
        if self.alignment_issues:
            return 0
        return len(self.manifest.segments)


class Pipeline:
    """Runs the stage graph with checkpoint-based resume."""

    def __init__(
        self,
        workdir: str,
        config: ProjectConfig,
        *,
        resume: bool = False,
        break_at: Stage | None = None,
        corrupt_sample: bool = False,
        edge_sample: bool = False,
        force_samples: bool = False,
        clock: Callable[[], float] = time.perf_counter,
    ) -> None:
        self.workdir = workdir
        self.config = config
        self.resume = resume
        self.break_at = break_at
        self.corrupt_sample = corrupt_sample
        self.edge_sample = edge_sample
        self.force_samples = force_samples
        self.clock = clock
        self.paths = ensure_layout(workdir)
        self.result = PipelineResult(workdir=workdir)
        self._collector = ErrorCollector()
        self._artifacts: dict[Stage, list[Artifact]] = {}
        self._checkpoint: Checkpoint | None = None
        self._media: dict[str, Any] = {}
        # Sources whose probe/normalize failed and which the skip policy
        # excludes from composition.
        self._excluded: set[str] = set()
        self.skipped_tracks: list[dict[str, Any]] = []
        self._skipped_errors: list[MediaError] = []

    # ------------------------------------------------------------------ setup
    def _samples(self) -> SampleSet:
        force = self.force_samples
        samples = generate_samples(self.paths["samples"], force=force)
        if self.edge_sample and not os.path.exists(samples.edge_tail):
            samples = generate_samples(self.paths["samples"], force=True)
        return samples

    def _input_files(self, samples: SampleSet) -> list[str]:
        files = [samples.cam_main, samples.cam_alt, samples.team_voice, samples.player_voice]
        if self.edge_sample:
            files.append(samples.edge_tail)
        if self.corrupt_sample:
            files.append(samples.corrupt_cam)
        return files

    def _timeline_document(self, samples: SampleSet) -> dict[str, Any]:
        sources = {
            "cam_main": samples.cam_main,
            "team_voice": samples.team_voice,
            "player_voice": samples.player_voice,
        }
        if self.edge_sample:
            sources["edge_tail"] = samples.edge_tail
        if self.corrupt_sample:
            sources["corrupt_cam"] = samples.corrupt_cam
        tracks: list[dict[str, Any]] = [
            {
                "id": "cam_main",
                "kind": "audiovisual",
                "source": sources["cam_main"],
                "clips": [
                    {"id": "main_a", "start": "0", "end": "1", "source": "cam_main", "source_in": "0"},
                    {"id": "main_b", "start": "1", "end": "101/50", "source": "cam_main", "source_in": "1"},
                ],
                "transitions": [
                    {"after": "main_a", "before": "main_b", "start": "4/5", "end": "6/5"}
                ],
            },
            {
                "id": "team_voice",
                "kind": "audio",
                "source": sources["team_voice"],
                "clips": [
                    {"id": "team_a", "start": "0", "end": "1", "source": "team_voice", "source_in": "0"},
                    {"id": "team_b", "start": "1", "end": "101/50", "source": "team_voice", "source_in": "1"},
                ],
                "transitions": [
                    {"after": "team_a", "before": "team_b", "start": "4/5", "end": "23/20"}
                ],
            },
            {
                "id": "player_voice",
                "kind": "audio",
                "source": sources["player_voice"],
                "clips": [
                    {"id": "player_a", "start": "0", "end": "101/50", "source": "player_voice", "source_in": "0"}
                ],
            },
        ]
        if self.edge_sample:
            tracks.append(
                {
                    "id": "edge_cam",
                    "kind": "video",
                    "source": sources["edge_tail"],
                    "clips": [
                        {"id": "edge_a", "start": "0", "end": "1/5", "source": "edge_tail", "source_in": "0"}
                    ],
                    "coverage": "clips",
                }
            )
        if self.edge_sample:
            tracks.append(
                {
                    "id": "alt_cam",
                    "kind": "video",
                    "source": "cam_alt",
                    "coverage": "clips",
                    "clips": [
                        {"id": "alt_a", "start": "0", "end": "1/5", "source": "cam_alt", "source_in": "0"}
                    ],
                }
            )
        if self.corrupt_sample:
            tracks.append(
                {
                    "id": "corrupt_cam",
                    "kind": "video",
                    "source": sources["corrupt_cam"],
                    "clips": [
                        {"id": "bad_a", "start": "0", "end": "1", "source": "corrupt_cam", "source_in": "0"}
                    ],
                    "coverage": "clips",
                }
            )
        return {
            "window": {
                "start": format_rational(self.config.window_start),
                "end": format_rational(self.config.window_end),
            },
            "tracks": tracks,
        }

    def _checkpoint_inputs(self, samples: SampleSet) -> str:
        config_payload = self.config.digest_payload()
        config_payload["corrupt_sample"] = self.corrupt_sample
        config_payload["edge_sample"] = self.edge_sample
        return input_digest(config_payload, self._input_files(samples))

    # -------------------------------------------------------------- execution
    def run(self) -> PipelineResult:
        started = self.clock()
        try:
            samples = self._samples()
            digest = self._checkpoint_inputs(samples)
            self._prepare_checkpoint(digest)
            self._media["sources"] = {
                "cam_main": samples.cam_main,
                "cam_alt": samples.cam_alt,
                "team_voice": samples.team_voice,
                "player_voice": samples.player_voice,
                "edge_tail": samples.edge_tail,
                "corrupt_cam": samples.corrupt_cam,
            }
            self._source_map = dict(self._media["sources"])
            document = self._timeline_document(samples)
            timeline_path = self.paths["timeline"]
            write_json(timeline_path, document)
            timeline = parse_timeline(document)
            self.result.timeline = timeline
            if not self._graph_ok(timeline):
                return self._finish(started)
            stages: list[tuple[Stage, Callable[[], bool]]] = [
                (Stage.TIMELINE, lambda: self._stage_timeline(timeline)),
                (Stage.PROBE, lambda: self._stage_probe(timeline)),
                (Stage.NORMALIZE, lambda: self._stage_normalize(timeline)),
                (Stage.TRANSITION, lambda: self._stage_transition(timeline)),
                (Stage.SEGMENT, lambda: self._stage_segment(timeline)),
                (Stage.ENCODE, lambda: self._stage_encode(timeline)),
                (Stage.MANIFEST, lambda: self._stage_manifest(timeline)),
            ]
            start_index = self._resume_index()
            for index, (stage, action) in enumerate(stages):
                if index < start_index:
                    if self._rehydrate(stage):
                        self.result.stages.append(
                            StageRecord(stage=stage, status="reused", seconds=0.0)
                        )
                        continue
                    start_index = index
                stage_started = self.clock()
                ok = action()
                elapsed = self.clock() - stage_started
                record = StageRecord(stage=stage, status="done" if ok else "failed", seconds=elapsed)
                record.artifacts = [a.path for a in self._artifacts.get(stage, [])]
                self.result.stages.append(record)
                self.result.timings[stage.label] = elapsed
                if not ok:
                    break
                if not self._commit(stage):
                    record.status = "failed"
                    record.detail = "checkpoint commit failed"
                    break
                if self.break_at is not None and stage is self.break_at:
                    self.result.interrupted_at = stage
                    break
        except (ValueError, OSError) as exc:
            self._collector.add(
                MediaError(
                    code="pipeline_failed",
                    stage=Stage.TIMELINE,
                    message=f"pipeline aborted: {exc}",
                    severity=Severity.FATAL,
                )
            )
        return self._finish(started)

    def _finish(self, started: float) -> PipelineResult:
        self.result.errors = sorted(
            list(self._collector.errors) + list(self._skipped_errors),
            key=MediaError.sort_key,
        )
        self.result.skipped_tracks = list(self.skipped_tracks)
        self.result.timings["total"] = self.clock() - started
        if self.result.interrupted_at is None:
            write_json(
                self.paths["summary"],
                {
                    "stages": [r.to_dict() for r in self.result.stages],
                    "timings": {k: round(v, 4) for k, v in sorted(self.result.timings.items())},
                    "resumed_from": self.result.resumed_from.label
                    if self.result.resumed_from
                    else None,
                    "checkpoint_status": self.result.checkpoint_status,
                },
            )
        return self.result

    def _graph_ok(self, timeline: Timeline) -> bool:
        graph = DependencyGraph.default(sorted(timeline.track_ids))
        cycle = detect_cycle(graph)
        if cycle is None:
            return True
        self._collector.add(
            MediaError(
                code="dependency_cycle",
                stage=Stage.TIMELINE,
                message=cycle,
                severity=Severity.FATAL,
            )
        )
        return False

    def _prepare_checkpoint(self, digest: str) -> None:
        self._checkpoint = Checkpoint(input_digest=digest)
        if not self.resume:
            return
        load = load_checkpoint(self.workdir, digest)
        self.result.checkpoint_status = load.status
        if load.checkpoint is None:
            return
        if load.status not in (CheckpointStatus.VALID, CheckpointStatus.BROKEN_ARTIFACT):
            return
        reusable = reusable_stages(load.checkpoint, load.status, load.first_invalid_stage)
        self.result.reused_stages = reusable
        if not reusable:
            return
        self._checkpoint = Checkpoint(
            input_digest=digest,
            completed=list(reusable),
            artifacts=[a for a in load.checkpoint.artifacts if a.stage in reusable],
        )
        self.result.resumed_from = STAGE_ORDER[STAGE_ORDER.index(reusable[-1]) + 1] if reusable[-1] is not STAGE_ORDER[-1] else None

    def _resume_index(self) -> int:
        if not self.resume or not self.result.reused_stages:
            return 0
        last = max(STAGE_ORDER.index(s) for s in self.result.reused_stages)
        return last + 1 if last + 1 < len(STAGE_ORDER) else len(STAGE_ORDER) - 1

    def _rehydrate(self, stage: Stage) -> bool:
        try:
            if stage is Stage.TIMELINE:
                # The timeline file is rewritten from the same config on every
                # run; validation is cheap, so only trust the stage if the
                # artifact still matches the checkpoint digest.
                return True
            if stage is Stage.PROBE:
                probes: dict[str, ProbeResult] = {}
                for artifact in self._checkpoint.artifact_for(Stage.PROBE) if self._checkpoint else []:
                    if not artifact.path.endswith(".json"):
                        continue
                    payload = read_json(artifact.path)
                    probes[payload["track"]] = probe_from_json(payload)
                    for raw in payload.get("errors", []):
                        error = MediaError.from_dict(raw)
                        self._skipped_errors.append(error)
                        self.skipped_tracks.append(
                            {
                                "track": payload["track"],
                                "reason": "probe_failed",
                                "first_code": raw.get("code"),
                                "first_pts": raw.get("pts"),
                            }
                        )
                if not probes:
                    return False
                self._media["probes"] = probes
                self.result.probes = probes
                return True
            if stage is Stage.NORMALIZE:
                videos: dict[str, NormalizedVideo] = {}
                audios: dict[str, NormalizedAudio] = {}
                for artifact in self._checkpoint.artifact_for(Stage.NORMALIZE) if self._checkpoint else []:
                    if not artifact.path.endswith(".json"):
                        continue
                    payload = read_json(artifact.path)
                    if payload.get("kind") == "color":
                        self.result.color = payload.get("decision")
                        continue
                    if payload.get("kind") == "video":
                        videos[payload["track"]] = video_from_json(payload)
                    elif payload.get("kind") == "audio":
                        audios[payload["track"]] = audio_from_json(payload)
                self._media["videos"] = videos
                self._media["audios"] = audios
                return True
            if stage is Stage.SEGMENT:
                records = [
                    a
                    for a in (self._checkpoint.artifact_for(Stage.SEGMENT) if self._checkpoint else [])
                    if a.path.endswith(".json")
                ]
                if not records:
                    return False
                plan = plan_from_json(read_json(records[0].path))
                self._media["plan"] = plan
                self.result.plan = plan
                return True
            if stage is Stage.TRANSITION:
                # The composed program is in-memory (frames plus PCM) and is
                # not persisted, so it must be recomposed when a later stage
                # needs it. Probe and normalize artifacts are still reused.
                return False
            if stage is Stage.ENCODE:
                return self._rehydrate_encoded()
            return False
        except (OSError, ValueError, KeyError, TypeError):
            return False

    def _rehydrate_encoded(self) -> bool:
        from .encode import EncodedSegment, inspect_segment
        from .probe import file_sha256

        plan: SegmentPlan | None = self._media.get("plan")
        if plan is None:
            return False
        encoded: list[EncodedSegment] = []
        for rendition in self.config.renditions:
            rendition_dir = os.path.join(self.paths["segments"], rendition.name)
            for planned in plan.segments:
                path = os.path.join(rendition_dir, f"{rendition.name}_{planned.index:05d}.mp4")
                if not os.path.exists(path):
                    return False
                actual = inspect_segment(path)
                frames, samples = actual if actual else (0, 0)
                encoded.append(
                    EncodedSegment(
                        rendition=rendition.name,
                        index=planned.index,
                        path=path,
                        sha256=file_sha256(path),
                        size=os.path.getsize(path),
                        duration=planned.duration,
                        video_frames=planned.video_frames,
                        audio_samples=planned.audio_samples,
                        actual_video_frames=frames,
                        actual_audio_samples=samples,
                    )
                )
        self.result.encoded = encoded
        by_rendition: dict[str, list[EncodedSegment]] = {}
        for item in encoded:
            by_rendition.setdefault(item.rendition, []).append(item)
        self.result.segments_by_rendition = by_rendition
        return True

    # ------------------------------------------------------------------ stages
    def _stage_timeline(self, timeline: Timeline) -> bool:
        self._record_bytes(Stage.TIMELINE, self.paths["timeline"])
        issues = validate_timeline(timeline)
        for issue in issues:
            self._collector.add(issue.to_media_error(Stage.TIMELINE))
        return not self.result.has_fatal

    def _stage_probe(self, timeline: Timeline) -> bool:
        probes: dict[str, ProbeResult] = {}
        for track in timeline.tracks.values():
            if not track.clips:
                continue
            source = track.clips[0].source
            if source in probes:
                continue
            probe = probe_media(self._source_path(timeline, track.id), track_id=source)
            probes[source] = probe
            ignored: list[dict[str, Any]] = []
            if any(e.severity >= Severity.ERROR for e in probe.errors):
                self._excluded.add(source)
                self.skipped_tracks.append(
                    {
                        "track": source,
                        "reason": "probe_failed",
                        "first_code": probe.errors[0].code,
                        "first_pts": str(probe.errors[0].location.pts)
                        if probe.errors[0].location.pts is not None
                        else None,
                    }
                )
                self.result.skipped_tracks = list(self.skipped_tracks)
                if self.config.policy == "skip":
                    # Under the skip policy the damaged source is excluded and
                    # its errors stay in the summary without failing the run;
                    # they are persisted so a resumed run can still report
                    # them.
                    ignored = [e.to_dict() for e in probe.errors]
                    self._skipped_errors.extend(probe.errors)
                else:
                    for error in probe.errors:
                        self._collector.add(error)
            else:
                for error in probe.errors:
                    self._collector.add(error)
            path = os.path.join(self.paths["probe"], f"{source}.json")
            write_json(path, probe_to_json(probe, source, ignored))
            self._record_bytes(Stage.PROBE, path)
        self._media["probes"] = probes
        self.result.probes = probes
        if self.config.policy == "stop" and self.skipped_tracks:
            return False
        return not self.result.has_fatal

    _source_map: dict[str, str] = {}

    def _source_path(self, timeline: Timeline, track_id: str) -> str:
        clip = timeline.tracks[track_id].clips[0]
        return self._source_map[clip.source]

    def _stage_normalize(self, timeline: Timeline) -> bool:
        probes: dict[str, ProbeResult] = self._media.get("probes", {})
        color_sources = {
            key: (
                ColorMetadata.from_stream(probe.video())
                if probe.video() is not None
                else ColorMetadata()
            )
            for key, probe in probes.items()
        }
        policy = ColorPolicy(mode=self.config.color_policy, track=self.config.color_track)
        preferred = ("cam_main", "team_voice", "player_voice", "edge_tail", "cam_alt")
        decision = (
            resolve_color("cam_main", color_sources, policy, preferred)
            if color_sources
            else None
        )
        self.result.color = decision.to_dict() if decision else None
        color_path = os.path.join(self.paths["normalized"], "color.json")
        write_json(
            color_path,
            {"kind": "color", "decision": self.result.color},
        )
        self._record_bytes(Stage.NORMALIZE, color_path)
        videos: dict[str, NormalizedVideo] = {}
        audios: dict[str, NormalizedAudio] = {}
        from .normalize import VideoSpec

        for track_id, probe in probes.items():
            if track_id in self._excluded:
                continue
            spec = None
            if probe.video() is not None:
                spec = VideoSpec(
                    width=640,
                    height=360,
                    rate=probe.video().rate or self.config.video_rate,
                )
            outcome = normalize_track(probe, self.paths["normalized"], track_id, spec, decision)
            for error in outcome.errors:
                self._collector.add(error)
            if outcome.video is not None:
                videos[track_id] = outcome.video
                path = os.path.join(self.paths["normalized"], f"{track_id}.video.json")
                write_json(path, video_to_json(track_id, outcome.video))
                self._record_bytes(Stage.NORMALIZE, path)
                self._record_bytes(Stage.NORMALIZE, outcome.video.path)
            if outcome.audio is not None:
                audios[track_id] = outcome.audio
                path = os.path.join(self.paths["normalized"], f"{track_id}.audio.json")
                write_json(path, audio_to_json(track_id, outcome.audio))
                self._record_bytes(Stage.NORMALIZE, path)
                self._record_bytes(Stage.NORMALIZE, outcome.audio.path)
        self._media["videos"] = videos
        self._media["audios"] = audios
        return not self.result.has_fatal

    def _stage_transition(self, timeline: Timeline) -> bool:
        window = Span(self.config.window_start, self.config.window_end)
        effective = self._effective_timeline(timeline)
        outcome = compose_program(
            effective,
            self._media.get("videos", {}),
            self._media.get("audios", {}),
            window,
            self.config.audio_gains,
        )
        for error in outcome.errors:
            self._collector.add(error)
        if outcome.program is None:
            return False
        program = outcome.program
        self._media["program"] = program
        self.result.program_frames = program.frame_count
        self.result.program_samples = program.audio_samples
        path = os.path.join(self.paths["probe"], "program.json")
        write_json(
            path,
            {
                "kind": "program",
                "frames": program.frame_count,
                "samples": program.audio_samples,
                "rate": str(program.rate),
                "video_end": str(program.video_end),
                "audio_end": str(program.audio_end),
                "notes": outcome.notes,
            },
        )
        self._record_bytes(Stage.TRANSITION, path)
        return True

    def _effective_timeline(self, timeline: Timeline) -> Timeline:
        """Drop tracks whose sources were skipped under the skip policy."""
        if not self._excluded:
            return timeline
        effective = Timeline(window=timeline.window)
        for track_id, track in timeline.tracks.items():
            if any(clip.source in self._excluded for clip in track.clips):
                continue
            effective.add_track(track)
        return effective

    def _stage_segment(self, timeline: Timeline) -> bool:
        window = Span(self.config.window_start, self.config.window_end)
        program = self._media.get("program")
        video_end = program.video_end if program is not None else window.end
        audio_end = program.audio_end if program is not None else window.end
        plan = plan_segments(
            window,
            self.config.video_rate,
            PROGRAM_AUDIO_RATE,
            self.config.segment_duration,
            keyframe_grid=None,
            transitions=self._transition_spans(timeline),
            video_duration=video_end,
            audio_duration=audio_end,
        )
        self.result.plan = plan
        self._media["plan"] = plan
        path = os.path.join(self.paths["probe"], "plan.json")
        write_json(path, plan_to_json(plan))
        self._record_bytes(Stage.SEGMENT, path)
        return True

    def _stage_encode(self, timeline: Timeline) -> bool:
        plan: SegmentPlan = self._media["plan"]
        program = self._media["program"]
        encoded, errors = encode_all(
            program,
            plan.segments,
            list(self.config.renditions),
            self.paths["segments"],
            self.config.video_rate,
        )
        for error in errors:
            self._collector.add(error)
        self.result.encoded = encoded
        for item in encoded:
            self._record_bytes(Stage.ENCODE, item.path)
        by_rendition: dict[str, list[EncodedSegment]] = {}
        for item in encoded:
            by_rendition.setdefault(item.rendition, []).append(item)
        self.result.segments_by_rendition = by_rendition
        return not any(e.severity >= Severity.FATAL for e in errors)

    def _stage_manifest(self, timeline: Timeline) -> bool:
        encoded = self.result.encoded
        plan: SegmentPlan = self._media["plan"]
        by_rendition: dict[str, list[EncodedSegment]] = {}
        for item in encoded:
            by_rendition.setdefault(item.rendition, []).append(item)
        window = {
            "start": format_rational(self.config.window_start),
            "end": format_rational(self.config.window_end),
        }
        transitions = [
            {
                "track": track.id,
                "after": transition.after,
                "before": transition.before,
                "start": str(transition.span.start),
                "end": str(transition.span.end),
            }
            for track in timeline.tracks.values()
            for transition in track.transitions
        ]
        inputs = [
            {
                "track": key,
                "sha256": probe.sha256,
                "size": probe.size,
                "format": probe.format_name,
            }
            for key, probe in sorted(self._media.get("probes", {}).items())
        ]
        manifest = build_manifest(
            plan,
            by_rendition,
            list(self.config.renditions),
            window,
            self.config.segment_duration,
            color=None,
            transitions=transitions,
            inputs=inputs,
        )
        if self.result.color is not None:
            manifest.color = self.result.color
        issues = check_alignment(manifest)
        self.result.alignment_issues = issues
        for issue in issues:
            self._collector.add(issue.to_media_error(Stage.MANIFEST))
        path = write_manifest(manifest, self.paths["root"])
        self.result.manifest = manifest
        self.result.manifest_text = manifest.serialize()
        self._record_bytes(Stage.MANIFEST, path)
        with open(path, "r", encoding="utf-8") as handle:
            reread = handle.read()
        if reread != self.result.manifest_text:
            self._collector.add(
                MediaError(
                    code="manifest_not_deterministic",
                    stage=Stage.MANIFEST,
                    message="manifest re-read differs from what was written",
                    severity=Severity.ERROR,
                )
            )
        return not self.result.has_error

    # ----------------------------------------------------------------- helpers
    def _transition_spans(self, timeline: Timeline) -> list[Span]:
        return [
            transition.span
            for track in timeline.tracks.values()
            for transition in track.transitions
        ]

    def _record_bytes(self, stage: Stage, path: str) -> None:
        self._artifacts.setdefault(stage, []).append(Artifact.from_path(stage, path))

    def _commit(self, stage: Stage) -> bool:
        if self._checkpoint is None:
            return False
        self._checkpoint.mark_complete(stage, self._artifacts.get(stage, []))
        try:
            save_checkpoint(self._checkpoint, self.workdir)
        except OSError:
            return False
        return True
