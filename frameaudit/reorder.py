"""Bounded presentation-order reordering driven by raw PTS."""

from __future__ import annotations

import heapq
from statistics import median

from frameaudit.model import DecodedFrame, PresentedFrame


class PtsReorderer:
    def __init__(self, depth: int = 16, default_step_us: int = 41_667) -> None:
        if depth < 1:
            raise ValueError("depth must be at least 1")
        self.depth = depth
        self.default_step_us = max(1, default_step_us)
        self._heap: list[tuple[int, int, int, DecodedFrame]] = []
        self._missing: list[DecodedFrame] = []
        self._sequence = 0
        self._present_idx = 0
        self._segment: int | None = None
        self._previous_input_pts: int | None = None
        self._last_known_input_pts: int | None = None
        self._recent_steps: list[int] = []
        self.max_buffered = 0
        self.interpolated_count = 0

    @property
    def presented_count(self) -> int:
        return self._present_idx

    def _step(self) -> int:
        if self._recent_steps:
            return max(1, round(median(self._recent_steps)))
        return self.default_step_us

    def _present(self, frame: DecodedFrame, pts_us: int) -> PresentedFrame:
        result = PresentedFrame(
            present_idx=self._present_idx,
            source=frame,
            pts_raw_us=pts_us,
            missing_pts=frame.missing_pts,
            segment=frame.segment,
            decode_delta_us=frame.decode_delta_us,
        )
        self._present_idx += 1
        return result

    def _queue(self, frame: DecodedFrame, pts_us: int) -> None:
        self._sequence += 1
        heapq.heappush(self._heap, (pts_us, frame.decode_idx, self._sequence, frame))

    def _resolve_missing(self, next_pts: int | None) -> None:
        if not self._missing:
            return
        step = self._step()
        count = len(self._missing)
        previous = self._last_known_input_pts
        if previous is not None and next_pts is not None and next_pts > previous:
            for offset, frame in enumerate(self._missing, start=1):
                pts = previous + round((next_pts - previous) * offset / (count + 1))
                frame.output_pts_us = pts
                self._queue(frame, pts)
        elif previous is not None:
            for offset, frame in enumerate(self._missing, start=1):
                pts = previous + step * offset
                frame.output_pts_us = pts
                self._queue(frame, pts)
        elif next_pts is not None:
            start = next_pts - step * count
            for offset, frame in enumerate(self._missing, start=1):
                pts = start + step * offset
                frame.output_pts_us = pts
                self._queue(frame, pts)
        else:
            for offset, frame in enumerate(self._missing, start=1):
                pts = step * offset
                frame.output_pts_us = pts
                self._queue(frame, pts)
        self.interpolated_count += count
        self._missing = []

    def _drain(self) -> list[PresentedFrame]:
        outputs: list[PresentedFrame] = []
        while len(self._heap) >= self.depth:
            _, _, _, frame = heapq.heappop(self._heap)
            outputs.append(self._present(frame, frame.output_pts_us))
        return outputs

    def _push_known(self, frame: DecodedFrame, pts_us: int) -> None:
        frame.output_pts_us = pts_us
        self._queue(frame, pts_us)

    def push(self, frame: DecodedFrame) -> list[PresentedFrame]:
        outputs: list[PresentedFrame] = []
        if self._segment is None:
            self._segment = frame.segment
        elif frame.segment != self._segment:
            outputs.extend(self.flush())
            self._segment = frame.segment

        if frame.pts_raw_us is None:
            frame.missing_pts = True
            self._missing.append(frame)
            if len(self._missing) >= self.depth:
                self._resolve_missing(None)
        else:
            pts_us = frame.pts_raw_us
            if self._previous_input_pts is not None:
                frame.decode_delta_us = pts_us - self._previous_input_pts
            self._previous_input_pts = pts_us
            if self._last_known_input_pts is not None:
                step = pts_us - self._last_known_input_pts
                if step > 0:
                    self._recent_steps.append(step)
                    if len(self._recent_steps) > 32:
                        self._recent_steps.pop(0)
            self._resolve_missing(pts_us)
            self._last_known_input_pts = pts_us
            self._push_known(frame, pts_us)

        if self._missing:
            while len(self._heap) > self.depth:
                _, _, _, queued = heapq.heappop(self._heap)
                outputs.append(self._present(queued, queued.output_pts_us))
        else:
            outputs.extend(self._drain())
        self.max_buffered = max(self.max_buffered, len(self._heap) + len(self._missing))
        return outputs

    def flush(self) -> list[PresentedFrame]:
        self._resolve_missing(None)
        outputs: list[PresentedFrame] = []
        while self._heap:
            _, _, _, frame = heapq.heappop(self._heap)
            outputs.append(self._present(frame, frame.output_pts_us))
        self._previous_input_pts = None
        self._last_known_input_pts = None
        self._recent_steps = []
        self.max_buffered = max(self.max_buffered, len(self._heap) + len(self._missing))
        return outputs

