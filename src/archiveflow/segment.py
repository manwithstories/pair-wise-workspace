"""Segment boundary planning on the unified media clock.

Boundaries are computed once, on the shared 90 kHz media clock, so every
representation layer cuts at identical times. Cuts prefer keyframes; when
no keyframe is available near a boundary, when the tail is truncated, or
when a final segment is shorter than the target, the segment carries an
explanatory flag instead of failing silently.
"""

from __future__ import annotations

from dataclasses import dataclass, field

FLAG_NO_KEYFRAME = "no_keyframe"
FLAG_SHORT_SEGMENT = "short_segment"
FLAG_TRUNCATED_TAIL = "truncated_tail"
FLAG_PLACEHOLDER = "placeholder_fill"

# A keyframe must lie within this many ms after a boundary to be used.
KEYFRAME_TOLERANCE_MS = 1500.0
# Final segments shorter than this fraction of the target are "short".
SHORT_SEGMENT_FRACTION = 0.5


@dataclass(frozen=True)
class SegmentSpec:
    index: int
    start_ms: float
    end_ms: float
    keyframe_start: bool
    flags: tuple[str, ...] = field(default_factory=tuple)

    @property
    def duration_ms(self) -> float:
        return self.end_ms - self.start_ms


def plan_segments(media_end_ms: float, keyframe_times_ms: list[float],
                  target_ms: float, truncated: bool,
                  declared_end_ms: float | None = None) -> list[SegmentSpec]:
    """Plan aligned segment boundaries for the recovered material.

    ``media_end_ms`` is the end of the last recovered frame; keyframes are
    the recovered keyframe times. Boundaries land on exact multiples of
    ``target_ms`` so all representations share them.
    """
    if target_ms <= 0:
        raise ValueError("target segment duration must be positive")
    if media_end_ms <= 0:
        return []
    specs: list[SegmentSpec] = []
    kfs = sorted(t for t in keyframe_times_ms if t >= 0)
    start = 0.0
    index = 0
    while start < media_end_ms:
        end = min(start + target_ms, media_end_ms)
        flags: list[str] = []
        keyframe_start = any(abs(k - start) < 1.0 for k in kfs)
        if not keyframe_start and start > 0:
            windowed = [k for k in kfs if start <= k <= start
                        + KEYFRAME_TOLERANCE_MS]
            if windowed:
                end = min(max(windowed[0], start + 1.0), media_end_ms)
                keyframe_start = True
            else:
                flags.append(FLAG_NO_KEYFRAME)
        if end - start < target_ms * SHORT_SEGMENT_FRACTION:
            flags.append(FLAG_SHORT_SEGMENT)
        specs.append(SegmentSpec(index=index, start_ms=start, end_ms=end,
                                 keyframe_start=keyframe_start or start == 0,
                                 flags=tuple(flags)))
        start = end
        index += 1
    if truncated and specs:
        last = specs[-1]
        flags = tuple(dict.fromkeys(last.flags + (FLAG_TRUNCATED_TAIL,)))
        specs[-1] = SegmentSpec(index=last.index, start_ms=last.start_ms,
                                end_ms=last.end_ms,
                                keyframe_start=last.keyframe_start,
                                flags=flags)
    return specs
