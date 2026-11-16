"""Segment-stepped download / playback / buffer simulator.

One loop iteration models one media segment:

1. ask the estimator (history only) for the current safe bandwidth;
2. ask the policy for a ladder index given the live buffer state;
3. download that segment over the segment's observed network sample —
   download time is segment size in kbits divided by throughput kbps;
4. account for buffer drain during the download, recording an underflow
   (with exact exhausted seconds) whenever playback would starve;
5. add the realized sample to the estimator for the next decision.

A dead link (zero throughput) cannot take infinite wall-clock, so its
download time is capped at ``stall_cap_factor`` segment durations (two by
default): enough to fully drain the buffer and record a real underflow
while keeping every number finite and serializable. The buffer level can
never go negative by construction.
"""

from __future__ import annotations

from .estimator import BandwidthEstimator
from .models import (
    ABRSimulationError,
    BufferState,
    Decision,
    SimulationConfig,
    Trace,
)
from .policy import ABRPolicy, PolicyConfig


class Simulator:
    """Runs one trace through estimator + policy and records decisions."""

    def __init__(self, config: SimulationConfig) -> None:
        self.config = config
        self.ladder = config.ladder
        self.estimator = BandwidthEstimator(window=config.window)
        self.policy = ABRPolicy(
            self.ladder,
            PolicyConfig(
                upshift_confirm=config.upshift_confirm,
                downshift_confirm=config.downshift_confirm,
                safety_margin=config.safety_margin,
            ),
        )
        self.buffer_level = float(config.initial_buffer)
        self.current_index = 0
        self.decisions: list[Decision] = []

    # ------------------------------------------------------------------ #
    def _download_seconds(self, bitrate_kbps: int, throughput_kbps: float) -> float:
        size_kbits = bitrate_kbps * self.config.segment_seconds
        if throughput_kbps <= 0.0:
            return self.config.stall_cap_factor * self.config.segment_seconds
        return size_kbits / throughput_kbps

    def run(self, trace: Trace) -> list[Decision]:
        if trace.segment_seconds != self.config.segment_seconds:
            raise ABRSimulationError(
                f"trace segment-seconds {trace.segment_seconds} does not match "
                f"simulation config {self.config.segment_seconds}"
            )
        if len(trace.samples) != len(trace):
            raise ABRSimulationError("trace length mismatch")

        self.decisions = []
        prev_bitrate: int | None = None
        for index, sample in enumerate(trace.samples):
            # 1-2. decide from history + current buffer (causal).
            estimate = self.estimator.estimate()
            buffer_state = BufferState(
                self.buffer_level, float(self.config.max_buffer)
            )
            chosen = self.policy.select(self.current_index, estimate, buffer_state)
            chosen = self.ladder.clamp(chosen)
            bitrate = self.ladder.bitrates[chosen]

            # 3. download over this segment's observed network.
            download = self._download_seconds(bitrate, sample.throughput)
            if download < 0:  # defensive; division above cannot yield this
                raise ABRSimulationError("negative download time computed")

            # 4. buffer accounting — never negative. Playback drains the
            #    buffer for the whole download; once it hits zero the player
            #    is stalled for exactly the remaining download time, then the
            #    finished segment refills one segment-duration of media.
            if download > self.buffer_level:
                stall_seconds = download - self.buffer_level
                stall = True
                post_buffer = self.config.segment_seconds
            else:
                stall_seconds = 0.0
                stall = False
                post_buffer = self.buffer_level - download + \
                    self.config.segment_seconds
            post_buffer = min(post_buffer, float(self.config.max_buffer))
            if post_buffer < 0.0:
                raise ABRSimulationError("buffer went negative")

            self.decisions.append(
                Decision(
                    index=index,
                    bitrate_kbps=bitrate,
                    est_bandwidth_kbps=round(estimate.bandwidth_kbps, 3),
                    estimator_state=estimate.state,
                    buffer_seconds=round(self.buffer_level, 3),
                    download_seconds=round(download, 3),
                    switch=prev_bitrate is not None and bitrate != prev_bitrate,
                    stall=stall,
                    stall_seconds=round(stall_seconds, 3),
                    throughput_kbps=sample.throughput,
                )
            )

            # 5. realized sample informs the next decision.
            self.estimator.add(sample)
            self.buffer_level = post_buffer
            self.current_index = chosen
            prev_bitrate = bitrate

        return self.decisions
