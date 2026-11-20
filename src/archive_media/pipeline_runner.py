"""Two-pass recovery orchestration over the bounded-queue pipeline.

Pass 1 streams every segment through demux -> normalize -> loudness metering
to derive a single programme gain without buffering the whole programme.
Pass 2 re-decodes (the segment cache makes this cheap on small fixtures) and
hands each segment to :class:`~archive_media.pipeline_writer.SegmentRenderer`,
which applies the gain and the skip / silence-gap / stop policy.

Read, demux and measure run on separate threads joined by fixed-capacity
queues, so a slow consumer blocks the producer and memory stays bounded by
``queue_size`` items rather than programme duration.
"""

from __future__ import annotations

import threading
from pathlib import Path

import numpy as np

from .audio import AudioConfig, AudioProcessor
from .audio_layout import canonical_layout
from .audio_loudness import compute_gain_db
from .demux import SegmentDecodeResult, concatenate_chunks, demux_segment
from .errors import (
    DiagnosticCollector,
    FatalDamageError,
)
from .manifest_core import SegmentPlan, SegmentRef
from .pipeline import (
    BoundedQueue,
    FATAL_UNDER_POLICY,
    PipelineMetrics,
    Policy,
    SENTINEL,
    StageError,
)
from .pipeline_writer import SegmentRenderer, output_path_for, write_wav


class RecoveryRunner:
    """Executes the two-pass plan and owns all run state."""

    def __init__(
        self,
        plan: SegmentPlan,
        config: AudioConfig,
        output_dir: Path,
        *,
        policy: Policy,
        queue_size: int,
    ) -> None:
        self.plan = plan
        self.config = config
        self.output_dir = Path(output_dir)
        self.policy = policy
        self.queue_size = queue_size
        self.collector = DiagnosticCollector()
        self.metrics = PipelineMetrics()
        self.outputs: list[dict[str, object]] = []
        self.decode_cache: dict[int, SegmentDecodeResult] = {}
        self.stop_event = threading.Event()
        self.gain_db = 0.0
        self.peak_limited = False

    # ------------------------------------------------------------------
    def _enforce(self, result: SegmentDecodeResult) -> None:
        fatal_codes = FATAL_UNDER_POLICY[self.policy]
        for diag in result.diagnostics:
            self.collector.add(diag)
            if diag.code in fatal_codes:
                raise FatalDamageError(
                    diag.code,
                    f"policy stops on {diag.code.value}: {diag.message}",
                    locator=diag.locator,
                    detail=diag.detail,
                )

    def _decode(self, segment: SegmentRef) -> SegmentDecodeResult:
        if segment.sequence not in self.decode_cache:
            self.decode_cache[segment.sequence] = demux_segment(segment)
        return self.decode_cache[segment.sequence]

    # ------------------------------------------------------------------
    def measure_pass(self) -> tuple[float | None, float | None]:
        read_q = BoundedQueue(self.queue_size, "read->demux")
        demux_q = BoundedQueue(self.queue_size, "demux->audio")
        self.metrics.queues = [read_q.metrics, demux_q.metrics]
        programme_powers: list[float] = []
        programme_peak: float | None = None
        lock = threading.Lock()
        errors: list[StageError] = []

        def measure_handle(result: SegmentDecodeResult) -> None:
            nonlocal programme_peak
            self._enforce(result)
            if result.total_samples == 0:
                return
            samples, rate, layout = concatenate_chunks(result)
            processor = AudioProcessor(
                rate, canonical_layout(layout) or "mono", self.config,
                source_track=result.segment.raw_ref,
            )
            normalized = processor.normalize(samples)
            tail = processor.resampler.flush()
            if tail.size:
                normalized = np.concatenate([normalized, tail], axis=0)
            processor.measure(normalized)
            lufs, peak = processor.meter.result()
            with lock:
                if lufs is not None:
                    programme_powers.append(10.0 ** (lufs / 10.0))
                if peak is not None:
                    programme_peak = (
                        peak if programme_peak is None
                        else max(programme_peak, peak)
                    )

        def reader() -> None:
            for segment in self.plan.segments:
                if self.stop_event.is_set():
                    break
                read_q.put(segment)
            read_q.put(SENTINEL)

        def demux_worker() -> None:
            while True:
                item = read_q.get()
                if item is SENTINEL:
                    demux_q.put(SENTINEL)
                    return
                try:
                    demux_q.put(self._decode(item))
                except FatalDamageError as exc:
                    self.stop_event.set()
                    demux_q.put(StageError("demux", exc.diagnostic(), fatal=True))

        def measure_worker() -> None:
            while True:
                item = demux_q.get()
                if item is SENTINEL:
                    return
                if isinstance(item, StageError):
                    errors.append(item)
                    self.stop_event.set()
                    continue
                try:
                    measure_handle(item)
                except FatalDamageError as exc:
                    errors.append(StageError("measure", exc.diagnostic(), fatal=True))
                    self.stop_event.set()

        threads = [
            threading.Thread(target=reader, name="reader"),
            threading.Thread(target=demux_worker, name="demux"),
            threading.Thread(target=measure_worker, name="measure"),
        ]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()
        if errors:
            error = errors[0]
            raise FatalDamageError(
                error.diagnostic.code,
                error.diagnostic.message,
                locator=error.diagnostic.locator,
                detail=error.diagnostic.detail,
            )
        if not programme_powers:
            return None, programme_peak
        lufs = float(10.0 * np.log10(np.mean(programme_powers)))
        return lufs, programme_peak

    # ------------------------------------------------------------------
    def process_pass(
        self, programme_lufs: float | None, programme_peak: float | None
    ) -> None:
        gain_db, limited = compute_gain_db(
            programme_lufs, programme_peak,
            target_lufs=self.config.target_lufs,
            peak_ceiling_dbfs=self.config.peak_ceiling_dbfs,
        )
        self.gain_db = gain_db
        self.peak_limited = limited
        self.output_dir.mkdir(parents=True, exist_ok=True)
        renderer = SegmentRenderer(
            self.config, self.output_dir, self.policy, self.collector
        )
        for segment in self.plan.segments:
            result = self._decode(segment)
            self._enforce(result)
            status, samples, expected = renderer.render(segment, result, gain_db)
            path = output_path_for(self.output_dir, segment)
            sha = write_wav(path, samples, self.config.target_rate)
            self.outputs.append(
                {
                    "sequence": segment.sequence,
                    "representation": segment.representation,
                    "raw_ref": segment.raw_ref,
                    "output_path": str(path),
                    "status": status,
                    "written_samples": int(samples.shape[0]),
                    "expected_samples": expected,
                    "sha256": sha,
                    "diagnostics": [d.to_json() for d in result.diagnostics],
                }
            )
            if status in {"recovered", "duplicate", "gap"}:
                self.metrics.recovered_segments += 1
            if status == "gap":
                self.metrics.gap_segments += 1
            if status == "skipped":
                self.metrics.skipped_segments += 1

    def run(self) -> None:
        lufs, peak = self.measure_pass()
        self.process_pass(lufs, peak)


__all__ = ["RecoveryRunner"]
