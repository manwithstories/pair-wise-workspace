"""Stage orchestration: probe -> clock -> timeline -> render -> validate.

Every stage boundary writes a checkpoint through :mod:`mediamix.resume`. On
resume the store's input fingerprints and parameters are verified before any
work happens; probe/clock/timeline are re-derived cheaply (their summaries must
match the stored checkpoints byte-for-byte), while rendering skips every frame
already present and hash-verified in the frame ledger.
"""

from __future__ import annotations

import time
from dataclasses import dataclass, field
from pathlib import Path

from .clock import DriftModel, fit_drift, unwrap_counter
from .model import (
    MediaMixError,
    TICKS_PER_SECOND,
    safe_resolve,
)
from .probe import parse_track_file
from .render import (
    RenderConfig,
    output_root_hash,
    render_schedule,
    write_audio_sidecar,
    write_manifest,
)
from .resume import CheckpointStore, FrameEntry, sha256_file
from .timeline import build_timeline, map_track
from .validate import validate_export


@dataclass
class PipelineParams:
    """Everything that must stay identical between a run and its resume."""

    inputs: list[str]                      # file names relative to workdir/inputs
    out_width: int = 160
    out_height: int = 90
    tolerance_ns: int = 1_000
    interval_start: int = 0
    interval_end: int = 2**63 - 1
    strict: bool = False

    def fingerprint_dict(self) -> dict:
        return {
            "inputs": sorted(self.inputs),
            "out_width": self.out_width,
            "out_height": self.out_height,
            "tolerance_ns": self.tolerance_ns,
            "interval_start": self.interval_start,
            "interval_end": self.interval_end,
            "strict": self.strict,
        }


@dataclass
class PipelineResult:
    timeline_frames: int = 0
    frames_written: int = 0
    frames_skipped: int = 0
    isolated_tracks: list[str] = field(default_factory=list)
    corrupt_errors: list[MediaMixError] = field(default_factory=list)
    diagnostics: list[dict] = field(default_factory=list)
    wraps: int = 0
    drift_ppm: dict[str, float] = field(default_factory=dict)
    drift_models: dict[str, DriftModel] = field(default_factory=dict)
    validation_errors: list = field(default_factory=list)
    elapsed_seconds: float = 0.0
    interrupted: bool = False
    output_sha256: str = ""


class Interrupted(BaseException):  # noqa: B903 - internal control flow, not an error
    """Raised to simulate a crash after ``interrupt_after`` rendered frames."""


def _probe_summary(parse_results: dict) -> dict:
    return {
        name: {
            "file_digest": r.file_digest,
            "frames": len(r.frames),
            "offsets": [f.header_offset for f in r.frames],
            "errors": [
                {"type": type(e).__name__, "offset": e.offset, "frame_seq": e.frame_seq}
                for e in r.errors
            ],
        }
        for name, r in parse_results.items()
    }


def run_pipeline(
    workdir: str | Path,
    params: PipelineParams,
    *,
    interrupt_after: int | None = None,
) -> PipelineResult:
    """Execute (or resume) the full pipeline inside ``workdir``."""
    started = time.perf_counter()
    workdir = Path(workdir)
    db_path = safe_resolve(workdir, "state", "checkpoints.sqlite")
    result = PipelineResult()
    rendered_this_run = 0

    with CheckpointStore(db_path).open() as store:
        input_paths = {name: safe_resolve(workdir, "inputs", name) for name in params.inputs}
        fingerprints = {name: sha256_file(p) for name, p in input_paths.items()}
        fresh = not store.stages_done()
        if fresh:
            store.set_fingerprints(fingerprints)
            store.store_params(params.fingerprint_dict())
        else:
            store.verify_fingerprints(fingerprints)
            store.verify_params(params.fingerprint_dict())

        # -- stage 1: probe ------------------------------------------------
        parsed: dict[str, object] = {}
        for name, path in input_paths.items():
            parsed[name] = parse_track_file(path)
        summary = _probe_summary(parsed)
        stored = store.load_stage("probe")
        if stored is not None and stored != summary:
            raise MediaMixError(
                "probe checkpoint conflicts with re-parsed inputs", path=str(path)
            )
        if stored is None:
            store.save_stage("probe", summary)

        good_tracks = []
        for name, res in parsed.items():
            if res.errors:
                result.corrupt_errors.extend(res.errors)
                result.isolated_tracks.append(res.header.track_id if res.header else name)
                if params.strict:
                    raise res.errors[0]
                continue
            good_tracks.append((name, res))

        # -- stage 2: clock unwrap + drift fit -----------------------------
        # Reference time is derived from frame sequence + per-frame rate
        # (presentation time), which is independent of the drifting
        # acquisition clock, so the fit can actually measure clock drift.
        clock_summaries: dict[str, dict] = {}
        mapped_inputs = []
        for name, res in good_tracks:
            hdr = res.header
            seqs = [f.header.seq for f in res.frames]
            clocks = [f.header.clock for f in res.frames]
            seq_unwrapped = unwrap_counter(seqs, hdr.seq_bits, path=name, frame_seqs=seqs)
            clock_unwrapped = unwrap_counter(clocks, hdr.clock_bits, path=name, frame_seqs=seqs)
            refs: list[int] = []
            ref_tick = 0
            for idx, frame in enumerate(res.frames):
                if idx > 0:
                    prev = res.frames[idx - 1].header
                    frame_dur = TICKS_PER_SECOND * prev.fps_den // prev.fps_num
                    ref_tick += frame_dur
                refs.append(ref_tick)
            model = fit_drift(clock_unwrapped.values, refs, hdr.clock_rate)
            wraps = seq_unwrapped.wraps + clock_unwrapped.wraps
            result.wraps += wraps
            result.drift_ppm[hdr.track_id] = model.drift_ppm
            result.drift_models[hdr.track_id] = model
            clock_summaries[hdr.track_id] = {
                "wraps": wraps,
                "drift": model.as_dict(),
            }
            mapped_inputs.append(
                (name, res, seq_unwrapped.values, clock_unwrapped.values, model, wraps)
            )
        stored = store.load_stage("clock")
        if stored is not None and stored != clock_summaries:
            raise MediaMixError("clock checkpoint conflicts with recomputed model")
        if stored is None:
            store.save_stage("clock", clock_summaries)

        # -- stage 3: timeline ---------------------------------------------
        timelines = []
        for priority, (name, res, seq_vals, clock_vals, model, wraps) in enumerate(mapped_inputs):
            timelines.append(map_track(
                res.header.track_id, priority, res.frames,
                seq_vals, clock_vals, model, wraps,
            ))
        timeline = build_timeline(
            timelines,
            tolerance_ticks=params.tolerance_ns,
            interval=(params.interval_start, params.interval_end),
        )
        tl_summary = {
            "frames": len(timeline.frames),
            "ticks": [f.tick for f in timeline.frames],
            "diagnostics": [d.as_dict() for d in timeline.diagnostics],
        }
        stored = store.load_stage("timeline")
        if stored is not None and stored != tl_summary:
            raise MediaMixError("timeline checkpoint conflicts with recomputed schedule")
        if stored is None:
            store.save_stage("timeline", tl_summary)
        result.timeline_frames = len(timeline.frames)
        result.diagnostics = tl_summary["diagnostics"]

        # -- stage 4: render (the resumable, expensive stage) --------------
        payloads = {}
        for _name, res in good_tracks:
            for f in res.frames:
                payloads[(res.header.track_id, f.file_seq)] = (
                    f.payload, f.header.width, f.header.height
                )
        config = RenderConfig(
            width=params.out_width, height=params.out_height,
            tolerance_ticks=params.tolerance_ns,
            interval_start=params.interval_start,
            interval_end=params.interval_end, strict=params.strict,
        )
        known = store.known_frames()
        new_entries: list[FrameEntry] = []

        def on_progress(entry) -> None:
            nonlocal rendered_this_run
            new_entries.append(FrameEntry(
                name=entry.name, tick=entry.tick, sha256=entry.sha256,
                width=entry.width, height=entry.height,
            ))
            rendered_this_run += 1
            if interrupt_after is not None and rendered_this_run >= interrupt_after:
                # Durably persist everything rendered so far, then simulate
                # the crash: the resume run will skip all of these frames.
                store.mark_frames_bulk(new_entries)
                store.save_stage(
                    "render", {"interrupted": True, "frames": store.frame_count()}
                )
                result.interrupted = True
                raise Interrupted()

        try:
            rendered = render_schedule(
                Path(workdir), timeline, payloads, config, known, on_progress
            )
        except Interrupted:
            result.frames_written = store.frame_count()
            result.elapsed_seconds = time.perf_counter() - started
            return result
        # One transaction for the whole render instead of one commit per frame.
        store.mark_frames_bulk(new_entries)
        result.frames_skipped = sum(1 for e in rendered if e.name in known)
        audio_name, audio_hash = write_audio_sidecar(Path(workdir))
        frame_hashes = [e.sha256 for e in rendered]
        root = output_root_hash(frame_hashes, audio_hash)
        manifest = {
            "inputs": [
                {"name": n, "sha256": fingerprints[n]} for n in sorted(fingerprints)
            ],
            "mapping": {
                "ticks_per_second": 1_000_000_000,
                "tolerance_ns": params.tolerance_ns,
                "interval": [params.interval_start, params.interval_end],
                "wraps": result.wraps,
                "drift": {tid: m.as_dict() for tid, m in result.drift_models.items()},
                "diagnostics": result.diagnostics,
            },
            "frames": [
                {
                    "name": e.name, "tick": e.tick, "sha256": e.sha256,
                    "width": e.width, "height": e.height,
                    "track_id": e.track_id, "frame_seq": e.frame_seq,
                }
                for e in rendered
            ],
            "audio": {"name": audio_name, "sha256": audio_hash},
            "output_sha256": root,
        }
        write_manifest(Path(workdir), manifest)
        result.output_sha256 = root
        result.frames_written = store.frame_count()
        store.save_stage("render", {"interrupted": False, "frames": result.frames_written})

        # -- stage 5: independent validation --------------------------------
        errors = validate_export(Path(workdir), manifest)
        result.validation_errors = errors
        store.save_stage("validate", {
            "errors": [e.describe() for e in errors],
        })

    result.elapsed_seconds = time.perf_counter() - started
    return result
