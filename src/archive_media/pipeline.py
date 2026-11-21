"""Pipeline orchestration: correction, chunking, checkpoints and recovery.

The pipeline wires the mechanical stages together:

1. :mod:`probe` streams decoded samples from the container.
2. :mod:`timebase` calibrates and corrects every PTS onto the shared
   timeline (unwrap, drift, shared origin).
3. The corrected samples feed segmentation, silence detection and
   fingerprinting concurrently from the same stream.
4. Results are buffered into bounded chunks and written transactionally via
   :mod:`index`, which also owns the resume checkpoint.

Resource use stays bounded: only the current chunk plus the small
calibration window is held, never the whole recording.  Fatal errors abort
with the last committed checkpoint intact; recoverable errors are isolated
into regions and processing continues.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field

from .errors import ErrorLog, FatalMediaError
from .fingerprint import (
    AudioChunkAccumulator,
    Fingerprint,
    VideoChunkAccumulator,
    find_duplicates,
)
from .index import MediaIndex, file_content_hash
from .probe import AUDIO, VIDEO, Probe
from .segments import FrameMark, Segmenter
from .silence import SilenceDetector
from .timebase import SharedTimeline


@dataclass
class PipelineConfig:
    """Processing parameters captured into the resume digest."""

    chunk_size: int = 4
    target_kbps: int = 900
    segment_duration_s: float = 0.5
    silence_threshold: float = 0.01
    silence_min_duration_s: float = 0.08
    video_frames_per_fp: int = 4
    max_frames: int = 100_000

    def canonical(self) -> dict:
        return {
            "chunk_size": self.chunk_size,
            "target_kbps": self.target_kbps,
            "segment_duration_s": self.segment_duration_s,
            "silence_threshold": self.silence_threshold,
            "silence_min_duration_s": self.silence_min_duration_s,
            "video_frames_per_fp": self.video_frames_per_fp,
            "max_frames": self.max_frames,
        }


@dataclass
class FileMetrics:
    """Aggregated per-file quality and bitrate statistics."""

    source: str
    target_kbps: int = 0
    actual_kbps: float = 0.0
    bitrate_delta_pct: float = 0.0
    video_frames: int = 0
    audio_peak: float = 0.0
    audio_rms: float = 0.0
    segment_count: int = 0
    silence_count: int = 0
    duplicate_count: int = 0
    manifest_alignment_ms: float = 0.0
    checkpoint_resumed: bool = False
    duration_s: float = 0.0
    recoverable_errors: int = 0
    status: str = "ok"

    def to_dict(self) -> dict:
        data = dict(self.__dict__)
        for key in ("actual_kbps", "bitrate_delta_pct", "audio_peak", "audio_rms",
                    "manifest_alignment_ms", "duration_s"):
            data[key] = round(data[key], 4)
        return data


@dataclass
class PipelineResult:
    metrics: FileMetrics
    manifest_rows: list[dict] = field(default_factory=list)
    corrections: dict = field(default_factory=dict)


class Pipeline:
    """Coordinates one processing run over a set of local files."""

    def __init__(self, config: PipelineConfig, index: MediaIndex) -> None:
        self.config = config
        self.index = index
        self.errors = ErrorLog()

    def process_file(self, path: str, *, wrap_period_ticks: int | None = None) -> PipelineResult:
        """Process a single file with checkpoint-aware recovery.

        Two passes over the container keep the timebase model honest:
        calibration learns unwrap/drift, then correction maps timestamps
        while the three consumers (segmenter, silence, fingerprints) run
        over bounded chunks that are committed transactionally.
        """
        content_hash = file_content_hash(path)
        checkpoint = self.index.register_or_verify(
            path, content_hash, self.config.canonical()
        )
        if checkpoint.chunk_index > 0 or checkpoint.resumed:
            self.index.mark_resumed(path)
        resumed = checkpoint.resumed or checkpoint.chunk_index > 0

        file_errors = ErrorLog()
        timeline = self._calibrate(path, file_errors, wrap_period_ticks)
        metrics, rows = self._consume(
            path, timeline, file_errors, checkpoint, resumed
        )
        self.index.mark_status(path, "completed")
        return PipelineResult(
            metrics=metrics, manifest_rows=rows, corrections=timeline.corrections()
        )

    def _calibrate(
        self, path: str, file_errors: ErrorLog, wrap_period_ticks: int | None
    ) -> SharedTimeline:
        with Probe(path, file_errors) as probe:
            infos = probe.streams
            video_info = infos[VIDEO]
            timeline = SharedTimeline()
            timeline.add_track(
                VIDEO,
                time_base=video_info.time_base,
                wrap_period_ticks=wrap_period_ticks,
                ref_period_s=1 / (video_info.frame_rate or 12.0),
            )
            if AUDIO in probe.streams:
                audio_info = probe.streams[AUDIO]
                timeline.add_track(
                    AUDIO,
                    time_base=audio_info.time_base,
                    wrap_period_ticks=None,
                    ref_period_s=1024 / audio_info.sample_rate,
                )
            frame_count = 0
            for sample in probe.samples():
                frame_count += 1
                if frame_count > self.config.max_frames:
                    raise FatalMediaError(
                        "resource_limit",
                        f"{path} exceeded max frame count {self.config.max_frames}",
                    )
                timeline.calibrate(sample.track, sample.raw_pts)
            timeline.finalize_calibration()
        return timeline

    def _consume(
        self,
        path: str,
        timeline: SharedTimeline,
        file_errors: ErrorLog,
        checkpoint,
        resumed: bool,
    ) -> tuple[FileMetrics, list[dict]]:
        cfg = self.config
        with Probe(path, file_errors) as probe:
            audio_info = probe.streams.get(AUDIO)
            segmenter = Segmenter(cfg.segment_duration_s)
            silence = (
                SilenceDetector(
                    audio_info.sample_rate,
                    threshold=cfg.silence_threshold,
                    min_duration_s=cfg.silence_min_duration_s,
                )
                if audio_info
                else None
            )
            video_fp = VideoChunkAccumulator(cfg.video_frames_per_fp)
            audio_fp = (
                AudioChunkAccumulator(audio_info.sample_rate // 2) if audio_info else None
            )

            chunk_segments: list[dict] = []
            chunk_silence: list[dict] = []
            chunk_fingerprints: list[tuple[str, int, str]] = []
            manifest_rows: list[dict] = []
            video_frames = 0
            audio_peak_sum = 0.0
            audio_sq_sum = 0.0
            audio_sample_total = 0
            last_time_s = 0.0
            chunk_index = checkpoint.chunk_index
            fp_ordinal = 0
            audio_clock = 0.0

            for sample in probe.samples():
                corrected = timeline.correct(sample.track, sample.raw_pts)
                last_time_s = max(last_time_s, corrected)
                if sample.track == VIDEO:
                    video_frames += 1
                    segmenter.add(
                        FrameMark(VIDEO, sample.ref_index, corrected, sample.raw_pts)
                    )
                    for digest in video_fp.add_frame(sample.data):
                        chunk_fingerprints.append(("video", fp_ordinal, digest))
                        fp_ordinal += 1
                elif audio_info is not None:
                    arr = sample.data
                    segmenter.add(
                        FrameMark(AUDIO, sample.ref_index, corrected, sample.raw_pts)
                    )
                    if silence is not None:
                        silence.add_frame(corrected, arr)
                    if audio_fp is not None:
                        for digest in audio_fp.add_samples(arr):
                            chunk_fingerprints.append(("audio", fp_ordinal, digest))
                            fp_ordinal += 1
                    flat = arr.reshape(-1)
                    audio_peak_sum = max(audio_peak_sum, float(abs(flat).max()))
                    audio_sq_sum += float((flat.astype("float64") ** 2).sum())
                    audio_sample_total += flat.size
                    audio_clock = corrected + flat.size / audio_info.sample_rate

            # Finalize chunk-boundary consumers.
            for digest in video_fp.flush():
                chunk_fingerprints.append(("video", fp_ordinal, digest))
                fp_ordinal += 1
            if audio_fp is not None:
                for digest in audio_fp.flush():
                    chunk_fingerprints.append(("audio", fp_ordinal, digest))
                    fp_ordinal += 1
            silence_regions = silence.finalize() if silence is not None else []
            segments = segmenter.build()
            chunk_segments = [seg.to_dict() for seg in segments]
            chunk_silence = [region.to_dict() for region in silence_regions]

            duration_s = max(last_time_s, audio_clock)
            actual_kbps, delta = self._bitrate_metrics(path, duration_s)
            audio_rms = (
                (audio_sq_sum / audio_sample_total) ** 0.5 if audio_sample_total else 0.0
            )

            chunk_index += 1
            self.index.commit_chunk(
                path,
                chunk_index,
                duration_s,
                segments=chunk_segments,
                silence=chunk_silence,
                fingerprints=chunk_fingerprints,
                errors=file_errors.to_dicts(),
            )

            alignment_ms = self._alignment_error(segments, timeline)
            metrics = FileMetrics(
                source=path,
                target_kbps=cfg.target_kbps,
                actual_kbps=actual_kbps,
                bitrate_delta_pct=delta,
                video_frames=video_frames,
                audio_peak=audio_peak_sum,
                audio_rms=audio_rms,
                segment_count=len(chunk_segments),
                silence_count=len(chunk_silence),
                manifest_alignment_ms=alignment_ms,
                checkpoint_resumed=resumed,
                duration_s=duration_s,
                recoverable_errors=len(file_errors.regions),
                status="recovered" if file_errors.has_errors() else "ok",
            )
            manifest_rows = self._manifest_rows(
                path, timeline, chunk_segments, chunk_silence, chunk_fingerprints, metrics
            )
        return metrics, manifest_rows

    @staticmethod
    def _alignment_error(segments, timeline: SharedTimeline) -> float:
        """Manifest A/V alignment error in milliseconds.

        Compares the *start cut point* of audio and video segments sharing an
        ordinal on the shared boundary grid.  Start points are the auditable
        cut positions; segment ends naturally differ in length under VFR and
        are not part of the alignment contract.
        """
        by_track: dict[int, float] = {}
        audio_starts: dict[int, float] = {}
        for seg in segments:
            if seg.track == VIDEO:
                by_track[seg.segment_id] = seg.start_s
            elif seg.track == AUDIO:
                audio_starts[seg.segment_id] = seg.start_s
        if not by_track or not audio_starts:
            return 0.0
        worst = 0.0
        for seg_id, v_start in by_track.items():
            if seg_id in audio_starts:
                worst = max(worst, abs(v_start - audio_starts[seg_id]))
        return worst * 1000.0

    def _manifest_rows(
        self, path, timeline, segments, silence_regions, fingerprints, metrics
    ) -> list[dict]:
        base = {
            "source": os.path.basename(path),
            "source_path": path,
            "corrections": timeline.corrections(),
            "target_kbps": metrics.target_kbps,
            "actual_kbps": round(metrics.actual_kbps, 4),
            "bitrate_delta_pct": round(metrics.bitrate_delta_pct, 4),
        }
        rows: list[dict] = []
        for seg in segments:
            row = dict(base)
            row["record"] = "segment"
            row["segment"] = seg
            rows.append(row)
        for region in silence_regions:
            row = dict(base)
            row["record"] = "silence"
            row["silence"] = region
            rows.append(row)
        for kind, ordinal, digest in fingerprints:
            row = dict(base)
            row["record"] = "fingerprint"
            row["fingerprint"] = {"kind": kind, "ordinal": ordinal, "hash": digest}
            rows.append(row)
        return rows

    def _bitrate_metrics(self, path: str, duration_s: float) -> tuple[float, float]:
        """Actual container bitrate and its percent delta vs target."""
        size_bits = os.path.getsize(path) * 8
        actual_kbps = (size_bits / duration_s / 1000.0) if duration_s > 0 else 0.0
        target = max(1, self.config.target_kbps)
        delta = ((actual_kbps - target) / target) * 100.0
        return actual_kbps, delta

    def run_batch(
        self, cases: list, *, wrap_periods: dict[str, int] | None = None
    ) -> list[PipelineResult]:
        """Process every case, then resolve cross-file duplicates.

        Fingerprints are read back from the committed index so duplicate
        detection relies solely on durable content hashes.  Each file's
        ``duplicate_count`` is updated in place on its returned metrics.
        """
        wrap_periods = wrap_periods or {}
        results: list[PipelineResult] = []
        for case in cases:
            result = self.process_file(
                case.path, wrap_period_ticks=wrap_periods.get(case.name)
            )
            results.append(result)

        fingerprints = [
            Fingerprint(
                source=res.metrics.source,
                video_hashes=[
                    h for h in self.index.load_fingerprint_hashes(res.metrics.source)
                ],
                audio_hashes=[],
            )
            for res in results
        ]
        pairs = find_duplicates(fingerprints)
        counts: dict[str, int] = {}
        for pair in pairs:
            counts[pair["source_a"]] = counts.get(pair["source_a"], 0) + 1
            counts[pair["source_b"]] = counts.get(pair["source_b"], 0) + 1
        for result in results:
            result.metrics.duplicate_count = counts.get(result.metrics.source, 0)
        self._duplicate_pairs = pairs
        return results

    @staticmethod
    def aggregate_exit(results: list[PipelineResult], *, bitrate_tol_pct: float = 10.0,
                       alignment_tol_ms: float = 40.0) -> tuple[int, list[str]]:
        """Determine the terminal exit code and human-readable failures."""
        from . import EXIT_FATAL, EXIT_OK, EXIT_RECOVERED

        failures: list[str] = []
        any_recovered = False
        for result in results:
            m = result.metrics
            if m.status not in ("ok", "recovered"):
                failures.append(f"{os.path.basename(m.source)}: fatal status {m.status}")
                continue
            if abs(m.bitrate_delta_pct) > bitrate_tol_pct:
                failures.append(
                    f"{os.path.basename(m.source)}: bitrate delta {m.bitrate_delta_pct:.1f}%"
                )
            if m.manifest_alignment_ms > alignment_tol_ms:
                failures.append(
                    f"{os.path.basename(m.source)}: alignment {m.manifest_alignment_ms:.1f}ms"
                )
            if m.status == "recovered":
                any_recovered = True
        if failures:
            return EXIT_FATAL, failures
        if any_recovered:
            return EXIT_RECOVERED, []
        return EXIT_OK, []
