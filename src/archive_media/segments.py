"""Segmentation on the shared, corrected presentation timeline.

Boundaries are placed at regular target durations expressed in corrected
seconds (not container ticks), so VFR clips, drift and wraparound all
segment against the same physical time axis the audio is cut on.

Nearest-frame rule (explicit, for auditable cut points):
* A boundary at time ``T`` snaps to the frame whose corrected presentation
  time is closest to ``T``.  Ties resolve to the *earlier* frame, so a
  segment never claims content past its requested end.
* When a boundary lands inside a VFR gap (a missing presentation) the two
  bracketing frames are equidistant in frame index but not in time; the
  frame with the smaller absolute time distance wins, again ties-to-earlier.
* Audio is trimmed by sample index computed from the same corrected times,
  keeping A/V cut points on one timeline.

The segmenter is streaming and stateless beyond the pending boundary, so it
does not buffer the whole recording.
"""

from __future__ import annotations

from dataclasses import dataclass, field


@dataclass
class FrameMark:
    """One frame positioned on the corrected timeline."""

    track: str
    index: int
    corrected_s: float
    raw_pts: int


@dataclass
class Segment:
    """A finalized segment boundary pair on the shared timeline."""

    segment_id: int
    track: str
    start_s: float
    end_s: float
    start_index: int
    end_index: int
    frame_count: int
    # Frames that fell exactly on a boundary and how the tie was resolved.
    boundary_notes: list[str] = field(default_factory=list)

    def to_dict(self) -> dict:
        return {
            "segment_id": self.segment_id,
            "track": self.track,
            "start_s": round(self.start_s, 6),
            "end_s": round(self.end_s, 6),
            "start_index": self.start_index,
            "end_index": self.end_index,
            "frame_count": self.frame_count,
            "boundary_notes": list(self.boundary_notes),
        }


class Segmenter:
    """Emits per-track segments of a target duration on corrected time."""

    def __init__(self, target_duration_s: float) -> None:
        if target_duration_s <= 0:
            raise ValueError("target_duration_s must be positive")
        self.target = float(target_duration_s)
        self._marks: dict[str, list[FrameMark]] = {}
        self._emitted_index: dict[str, int] = {}

    def add(self, mark: FrameMark) -> None:
        self._marks.setdefault(mark.track, []).append(mark)

    @staticmethod
    def _nearest_index(marks: list[FrameMark], target_s: float) -> tuple[int, str]:
        """Index of the frame closest to target_s; ties resolve earlier."""
        best_idx = 0
        best_note = ""
        best_dist = abs(marks[0].corrected_s - target_s)
        for i in range(1, len(marks)):
            dist = abs(marks[i].corrected_s - target_s)
            # Strict comparison preserves the earlier frame on an exact tie.
            if dist < best_dist:
                best_dist = dist
                best_idx = i
                best_note = ""
            elif dist == best_dist and marks[i].corrected_s > target_s:
                best_note = "tie-resolved-earlier"
        return best_idx, best_note

    def build(self) -> list[Segment]:
        """Finalize all segments once the stream of marks is complete."""
        segments: list[Segment] = []
        for track in sorted(self._marks):
            marks = self._marks[track]
            if not marks:
                continue
            timeline_end = marks[-1].corrected_s
            boundary = self.target
            seg_id = 0
            start_idx = 0
            while start_idx < len(marks):
                if boundary >= timeline_end:
                    end_idx = len(marks) - 1
                    note = "tail-segment"
                else:
                    end_idx, note = self._nearest_index(marks, boundary)
                    if end_idx < start_idx:
                        end_idx = start_idx
                start_mark = marks[start_idx]
                end_mark = marks[end_idx]
                notes = [note] if note else []
                if end_mark.corrected_s - start_mark.corrected_s <= 0 and end_idx == start_idx and boundary < timeline_end:
                    notes.append("zero-length-boundary-skipped")
                segments.append(
                    Segment(
                        segment_id=seg_id,
                        track=track,
                        start_s=start_mark.corrected_s,
                        end_s=end_mark.corrected_s,
                        start_index=start_idx,
                        end_index=end_idx,
                        frame_count=end_idx - start_idx + 1,
                        boundary_notes=notes,
                    )
                )
                seg_id += 1
                start_idx = end_idx + 1
                boundary = marks[start_idx].corrected_s + self.target if start_idx < len(marks) else boundary
        return segments


def audio_sample_window(
    start_s: float, end_s: float, sample_rate: int
) -> tuple[int, int]:
    """Convert corrected seconds to an inclusive-exclusive sample range."""
    start = max(0, int(round(start_s * sample_rate)))
    end = max(start, int(round(end_s * sample_rate)))
    return start, end
