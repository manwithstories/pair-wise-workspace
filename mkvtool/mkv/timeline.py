"""Timestamp repair and per-track rewrite mapping."""

from __future__ import annotations

from dataclasses import dataclass, field

import numpy as np

from mkvtool.errors import TimelineError
from mkvtool.mkv.demux import DemuxResult, Sample, Track

NS_PER_MS = 1_000_000


@dataclass
class RewrittenSample:
    source_index: int | None
    source_dts_ns: int
    dts_ns: int
    pts_ns: int
    duration_ns: int
    keyframe: bool
    data: bytes
    composition_offset_ns: int
    synthetic: bool = False

    @property
    def dts_ms(self) -> float:
        return self.dts_ns / NS_PER_MS

    @property
    def pts_ms(self) -> float:
        return self.pts_ns / NS_PER_MS


@dataclass
class Gap:
    before_output_index: int
    start_ns: int
    end_ns: int
    duration_ns: int
    filled_frames: int = 0

    @property
    def duration_ms(self) -> float:
        return self.duration_ns / NS_PER_MS


@dataclass
class TrackTimeline:
    track: Track
    samples: list[RewrittenSample]
    source_first_dts_ns: int
    rewritten_first_dts_ns: int
    gaps: list[Gap] = field(default_factory=list)
    dts_adjustments: int = 0
    default_duration_ns: int = 0

    @property
    def keyframes(self) -> int:
        return sum(1 for sample in self.samples if sample.keyframe)

    @property
    def last_pts_ns(self) -> int:
        return max((sample.pts_ns for sample in self.samples), default=0)

    @property
    def first_pts_ns(self) -> int:
        return min((sample.pts_ns for sample in self.samples), default=0)


@dataclass
class TimelineResult:
    demux: DemuxResult
    tracks: dict[int, TrackTimeline]
    shift_ns: int
    gap_threshold_ns: int
    filled_gaps: int = 0

    def track(self, number: int) -> TrackTimeline:
        return self.tracks[number]


def _infer_default_duration(samples: list[Sample], scale_ns: int, track: Track) -> int:
    if track.default_duration_ns and track.default_duration_ns > 0:
        return track.default_duration_ns
    if len(samples) > 1:
        diffs = np.diff(np.array([s.timestamp_ticks for s in samples], dtype=np.int64))
        positive = diffs[diffs > 0]
        if positive.size:
            return int(round(float(np.median(positive)) * scale_ns))
    if track.kind == "video":
        return 40 * NS_PER_MS
    return 1024 * 1_000_000_000 // 48_000


def _source_durations(
    samples: list[Sample], default_ns: int, scale_ns: int, gap_threshold_ns: int
) -> list[int]:
    if not samples:
        return []
    raw_ticks = np.array([sample.timestamp_ticks for sample in samples], dtype=np.int64)
    diffs = np.diff(raw_ticks) * scale_ns
    durations: list[int] = []
    for index, sample in enumerate(samples):
        explicit = None
        if sample.duration_ticks is not None and sample.duration_ticks > 0:
            explicit = sample.duration_ticks * scale_ns
        if explicit is not None:
            durations.append(explicit)
        elif index < len(diffs) and 0 < int(diffs[index]) <= gap_threshold_ns:
            durations.append(int(diffs[index]))
        else:
            durations.append(default_ns)
    return [max(1, duration) for duration in durations]


def _fill_video_gaps(
    output: list[RewrittenSample], track: Track, default_ns: int
) -> int:
    if track.kind != "video" or len(output) < 2:
        return 0
    filled = 0
    index = 0
    while index < len(output):
        if output[index].synthetic:
            index += 1
            continue
        next_index = index + 1
        while next_index < len(output) and output[next_index].synthetic:
            next_index += 1
        if next_index >= len(output):
            break
        current = output[index]
        target = output[next_index]
        cursor = current.dts_ns + current.duration_ns
        count = 0
        while cursor + default_ns <= target.dts_ns and count < 10_000:
            output.insert(next_index, RewrittenSample(
                source_index=None,
                source_dts_ns=cursor,
                dts_ns=cursor,
                pts_ns=cursor,
                duration_ns=default_ns,
                keyframe=False,
                data=current.data,
                composition_offset_ns=0,
                synthetic=True,
            ))
            cursor += default_ns
            next_index += 1
            count += 1
        filled += count
        index = next_index
    return filled


def build_timeline(
    demux: DemuxResult,
    *,
    gap_threshold_ms: float = 1_000.0,
    fill_gaps: bool = False,
    shift_negative: bool = True,
) -> TimelineResult:
    if gap_threshold_ms < 0:
        raise TimelineError("gap threshold must be non-negative")
    scale_ns = demux.timecode_scale_ns
    threshold_ns = int(gap_threshold_ms * NS_PER_MS)
    supported_numbers = [track.number for track in demux.tracks if track.supported]
    if not supported_numbers:
        raise TimelineError("no supported tracks with timestamps")

    all_ticks = [
        sample.timestamp_ticks
        for number in supported_numbers
        for sample in demux.samples.get(number, [])
    ]
    minimum_ns = min(all_ticks) * scale_ns
    shift_ns = max(0, -minimum_ns)
    if minimum_ns < 0 and not shift_negative:
        raise TimelineError("negative DTS requires a segment-wide shift")

    timelines: dict[int, TrackTimeline] = {}
    total_filled = 0
    for track in demux.tracks:
        if not track.supported:
            continue
        samples = demux.samples.get(track.number, [])
        if not samples:
            raise TimelineError(f"track {track.number} has no usable samples")
        default_ns = _infer_default_duration(samples, scale_ns, track)
        durations = _source_durations(samples, default_ns, scale_ns, threshold_ns)
        output: list[RewrittenSample] = []
        adjustments = 0
        previous_dts: int | None = None
        previous_duration = default_ns
        for index, (sample, duration_ns) in enumerate(zip(samples, durations)):
            original_ns = sample.timestamp_ticks * scale_ns
            dts_ns = original_ns + shift_ns
            if previous_dts is not None and dts_ns <= previous_dts:
                dts_ns = previous_dts + previous_duration
                adjustments += 1
            composition_ns = sample.composition_offset_ticks * scale_ns
            if composition_ns < 0:
                raise TimelineError(
                    f"track {track.number} sample {index} has negative composition offset"
                )
            output.append(RewrittenSample(
                source_index=index,
                source_dts_ns=original_ns,
                dts_ns=dts_ns,
                pts_ns=dts_ns + composition_ns,
                duration_ns=duration_ns,
                keyframe=sample.keyframe,
                data=sample.data,
                composition_offset_ns=composition_ns,
            ))
            previous_dts = dts_ns
            previous_duration = duration_ns

        gaps: list[Gap] = []
        for index in range(len(output) - 1):
            end_ns = output[index].dts_ns + output[index].duration_ns
            missing = output[index + 1].dts_ns - end_ns
            if missing > threshold_ns:
                gaps.append(Gap(index + 1, end_ns, output[index + 1].dts_ns, missing))

        filled = _fill_video_gaps(output, track, default_ns) if fill_gaps else 0
        total_filled += filled
        if filled:
            for gap in gaps:
                gap.filled_frames = sum(
                    1 for sample in output
                    if sample.synthetic and gap.start_ns <= sample.dts_ns < gap.end_ns
                )
        first = output[0]
        timelines[track.number] = TrackTimeline(
            track=track,
            samples=output,
            source_first_dts_ns=first.source_dts_ns,
            rewritten_first_dts_ns=first.dts_ns,
            gaps=gaps,
            dts_adjustments=adjustments,
            default_duration_ns=default_ns,
        )
    if not timelines:
        raise TimelineError("no supported tracks could be rewritten")
    return TimelineResult(
        demux=demux,
        tracks=timelines,
        shift_ns=shift_ns,
        gap_threshold_ns=threshold_ns,
        filled_gaps=total_filled,
    )
