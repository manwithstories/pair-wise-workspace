"""Aggregate metrics over a list of per-segment :class:`Decision` records.

Beyond the headline counters required by the CLI, underflow segments are
grouped into *episodes* (runs of consecutive stalled segments) so a brief
one-segment hiccup and a sustained outage are reported as distinct things.
Estimation error is measured only where the estimator itself reports a
confident (``OK``) state against a non-zero realized throughput.
"""

from __future__ import annotations

import statistics
from dataclasses import asdict, dataclass

from .models import Decision, EstimatorState


@dataclass(frozen=True, slots=True)
class StallEpisode:
    """One contiguous run of stalled segments."""

    start_index: int
    end_index: int
    segments: int
    stall_seconds: float

    @property
    def brief(self) -> bool:
        """A single-segment hiccup counts as a brief underflow."""
        return self.segments <= 1


@dataclass(frozen=True, slots=True)
class SimulationSummary:
    segments: int
    switch_count: int
    stall_segment_count: int
    stall_episode_count: int
    stall_seconds: float
    average_bitrate_kbps: float
    max_bitrate_kbps: int
    min_bitrate_kbps: int
    mean_abs_estimation_error: float
    brief_episode_count: int
    sustained_episode_count: int
    stall_episodes: tuple[StallEpisode, ...]

    def to_dict(self) -> dict:
        return asdict(self)


def find_stall_episodes(decisions: list[Decision]) -> list[StallEpisode]:
    """Group consecutive stalled segments into contiguous episodes."""
    episodes: list[StallEpisode] = []
    start: int | None = None
    for index, decision in enumerate(decisions):
        if decision.stall and start is None:
            start = index
        if not decision.stall and start is not None:
            episodes.append(_build_episode(decisions, start, index - 1))
            start = None
    if start is not None:
        episodes.append(_build_episode(decisions, start, len(decisions) - 1))
    return episodes


def _build_episode(
    decisions: list[Decision], start: int, end: int
) -> StallEpisode:
    return StallEpisode(
        start_index=start,
        end_index=end,
        segments=end - start + 1,
        stall_seconds=round(
            sum(d.stall_seconds for d in decisions[start:end + 1]), 3
        ),
    )


def summarize(decisions: list[Decision]) -> SimulationSummary:
    """Compute every aggregate metric for one simulation."""
    if not decisions:
        raise ValueError("cannot summarize an empty decision list")

    episodes = find_stall_episodes(decisions)
    bitrates = [d.bitrate_kbps for d in decisions]

    errors = [
        abs(d.est_bandwidth_kbps - d.throughput_kbps)
        for d in decisions
        if d.estimator_state is EstimatorState.OK
        and d.throughput_kbps > 0.0
    ]
    mean_error = statistics.fmean(errors) if errors else 0.0

    return SimulationSummary(
        segments=len(decisions),
        switch_count=sum(1 for d in decisions if d.switch),
        stall_segment_count=sum(1 for d in decisions if d.stall),
        stall_episode_count=len(episodes),
        stall_seconds=round(sum(d.stall_seconds for d in decisions), 3),
        average_bitrate_kbps=round(statistics.fmean(bitrates), 2),
        max_bitrate_kbps=max(bitrates),
        min_bitrate_kbps=min(bitrates),
        mean_abs_estimation_error=round(mean_error, 3),
        brief_episode_count=sum(1 for ep in episodes if ep.brief),
        sustained_episode_count=sum(1 for ep in episodes if not ep.brief),
        stall_episodes=tuple(episodes),
    )
