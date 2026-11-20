"""Multi-track timeline composition and missing-segment policies.

The composer consumes decoded items in arrival order, keeps video frames
on their true presentation timestamps (never resampled to a fixed rate),
locates duplicate / backward / cross-manifest gap anomalies and applies
the configured policy to missing or truncated segments:

* ``fail``      - raise at the first bad segment
* ``gap-fill``  - emit deterministic blank frames covering the gap
* ``skip``      - drop the segment, leaving a recorded silent gap
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction

import numpy as np

from .clock import HZ_90K
from .config import POLICY_FAIL, POLICY_GAP_FILL
from .streamio import VideoItem
from .errors import ErrorCode, ErrorBook, RecoverableError


@dataclass(slots=True)
class TimelineFrame:
    rgb: np.ndarray
    pts_seconds: Fraction
    segment: str
    sequence: int
    filled: bool = False
    pixel_hash: str = ""


@dataclass(slots=True)
class GapRecord:
    sequence: int
    segment: str
    start_s: Fraction
    end_s: Fraction
    policy: str
    frames_emitted: int


@dataclass(slots=True)
class Composer:
    """Assembles the normalized video timeline and tracks anomalies."""

    nominal_fps: int
    modulus: int
    policy: str
    errors: ErrorBook
    frames: list[TimelineFrame] = field(default_factory=list)
    gaps: list[GapRecord] = field(default_factory=list)
    anomalies: list[dict] = field(default_factory=list)
    _last_pts: Fraction | None = None
    _last_segment: str | None = None
    _seen_sequences: set[int] = field(default_factory=set)

    def handle_video(self, item: VideoItem) -> None:
        if item.kind == "duplicate":
            self._record(ErrorCode.TIMESTAMP_DUPLICATE, item,
                         f"duplicate PTS at {float(item.pts_seconds):.3f}s")
            return
        if item.kind == "backward":
            self._record(ErrorCode.TIMESTAMP_BACKWARD, item,
                         f"backward PTS at {float(item.pts_seconds):.3f}s")
            return
        entering_segment = item.sequence not in self._seen_sequences
        if entering_segment and not item.wrapped:
            self._check_segment_gap(item)
            self._seen_sequences.add(item.sequence)
        elif entering_segment:
            # The new segment starts right at a 33-bit wrap boundary; the
            # apparent jump is the modulus, not missing media.
            self._seen_sequences.add(item.sequence)
        self._last_pts = item.pts_seconds
        self._last_segment = item.segment
        self.frames.append(TimelineFrame(
            rgb=item.rgb,
            pts_seconds=item.pts_seconds,
            segment=item.segment,
            sequence=item.sequence,
        ))

    def _record(self, code: ErrorCode, item: VideoItem, message: str) -> None:
        err = RecoverableError(code, message, segment=item.segment)
        self.errors.add(err)
        self.anomalies.append({
            "code": code.value,
            "pts_s": float(item.pts_seconds),
            "segment": item.segment,
            "message": message,
        })

    def _check_segment_gap(self, item: VideoItem) -> None:
        if self._last_pts is None:
            return
        # A modulus wrap (expanded by the clock) presents as a normal
        # forward step in unwrapped seconds; only flag a gap when the
        # presentation time actually jumps beyond three nominal frames.
        expected_step = Fraction(1, self.nominal_fps) * 3
        step = item.pts_seconds - self._last_pts
        if step > expected_step:
            err = RecoverableError(
                ErrorCode.TIMESTAMP_GAP,
                f"cross-manifest gap of {float(step):.3f}s before "
                f"{item.segment}",
                segment=item.segment,
            )
            self.errors.add(err, time_range=(
                int(self._last_pts * HZ_90K), int(item.pts_seconds * HZ_90K)
            ))
            self.anomalies.append({
                "code": ErrorCode.TIMESTAMP_GAP.value,
                "pts_s": float(item.pts_seconds),
                "segment": item.segment,
                "gap_s": float(step),
            })

    def handle_missing(
        self, sequence: int, segment: str, start_s: Fraction,
        end_s: Fraction, shape: tuple[int, int, int],
    ) -> None:
        """Apply the configured policy to a missing/truncated segment."""
        duration = end_s - start_s
        err = RecoverableError(
            ErrorCode.TRUNCATED_SEGMENT,
            f"unusable segment {segment} ({float(duration):.2f}s)",
            segment=segment,
        )
        if self.policy == POLICY_FAIL:
            self.errors.add(err)
            raise err
        n_fill = max(1, int(round(float(duration) * self.nominal_fps)))
        if self.policy == POLICY_GAP_FILL:
            step = Fraction(1, self.nominal_fps)
            blank = np.zeros(shape, dtype=np.uint8)
            for k in range(n_fill):
                pts = start_s + step * k
                if pts >= end_s:
                    break
                self.frames.append(TimelineFrame(
                    rgb=blank.copy(),
                    pts_seconds=pts,
                    segment=segment,
                    sequence=sequence,
                    filled=True,
                ))
            emitted = sum(1 for f in self.frames
                          if f.sequence == sequence and f.filled)
        else:  # POLICY_SKIP: no frames, silence on audio side
            emitted = 0
        self.errors.add(err, time_range=(
            int(start_s * HZ_90K), int(end_s * HZ_90K)
        ), recovered=True)
        self.gaps.append(GapRecord(
            sequence=sequence, segment=segment, start_s=start_s,
            end_s=end_s, policy=self.policy, frames_emitted=emitted,
        ))
        self._seen_sequences.add(sequence)

    def sort_timeline(self) -> list[TimelineFrame]:
        """Order frames by true presentation time (handles reordering)."""
        self.frames.sort(key=lambda f: f.pts_seconds)
        return self.frames


def attach_events(frames: list[TimelineFrame], events: list[dict]) -> list[dict]:
    """Bind event markers to the nearest frame by media time."""
    if not frames or not events:
        return []
    pts = [float(f.pts_seconds) for f in frames]
    bound: list[dict] = []
    for event in events:
        t = float(event["time_s"])
        idx = int(np.argmin([abs(p - t) for p in pts]))
        bound.append({
            "time_s": t,
            "label": event["label"],
            "frame_index": idx,
            "pts_s": pts[idx],
            "segment": frames[idx].segment,
        })
    return bound
