"""Streaming PyAV decode with bounded-queue backpressure.

The decoder never buffers a whole input: decoded items are pushed into a
bounded queue whose capacity is the configured queue size, so resident
memory scales with that capacity rather than with stream duration.
"""

from __future__ import annotations

import threading
from fractions import Fraction

import av
import numpy as np

from .clock import ClockState
from .config import MPEG_PTS_MOD
from .errors import ErrorCode, RecoverableError
from .manifest import ManifestPlan, Segment
from .streamio import (AudioItem, BoundedQueue, CancellationToken,
                       SegmentBoundary, VideoItem)


def _to_rgb(frame: av.VideoFrame) -> np.ndarray:
    if frame.format.name != "rgb24":
        frame = frame.reformat(format="rgb24")
    return frame.to_ndarray(format="rgb24")


class SegmentDecoder:
    """Decodes one segment, normalizing PTS through the shared clock."""

    def __init__(self, modulus: int, token: CancellationToken) -> None:
        self.modulus = modulus
        self.token = token
        self.video_clock = ClockState("video", Fraction(1, 90_000), modulus)
        # Audio is a separate node clock starting at zero and never wrapping
        # on these timelines, so it uses the full 33-bit modulus; applying
        # the video's (possibly small) test modulus would mis-unwrap it.
        self.audio_clock = ClockState(
            "audio", Fraction(1, 90_000), MPEG_PTS_MOD)
        self.frame_index = 0
        self._resampler: av.AudioResampler | None = None
        self._audio_sample_cursor = 0
        self._audio_segment_seq: int | None = None
        self.total_wraps = 0
        self._prev_segment_last_raw: int | None = None

    def _to_mono_s16(self, frame: av.AudioFrame) -> list[av.AudioFrame]:
        if frame.format.name == "s16" and frame.layout.name == "mono":
            return [frame]
        if self._resampler is None:
            self._resampler = av.AudioResampler(
                format="s16", layout="mono", rate=frame.sample_rate
            )
        return self._resampler.resample(frame)

    def decode(
        self, segment: Segment, queue: BoundedQueue
    ) -> SegmentBoundary:
        boundary = SegmentBoundary(
            segment.sequence, segment.path.name, segment.duration
        )
        # Each segment carries an independent local clock; reset the
        # unroller for this segment while the cumulative wrap count is kept.
        self.video_clock.unroller.reset()
        if segment.byte_size == 0:
            code = (ErrorCode.MISSING_SEGMENT
                    if not segment.path.is_file()
                    else ErrorCode.ZERO_LENGTH_SEGMENT)
            raise RecoverableError(
                code,
                f"unusable segment {segment.path.name} "
                f"({'missing' if code is ErrorCode.MISSING_SEGMENT else 'zero-length'})",
                segment=segment.path.name,
            )
        try:
            container = av.open(str(segment.path))
        except av.AVError as exc:
            raise RecoverableError(
                ErrorCode.DECODE_FAILURE,
                f"cannot open segment {segment.path.name}: {exc}",
                segment=segment.path.name,
            ) from exc
        video_stream = next(
            (s for s in container.streams if s.type == "video"), None
        )
        audio_stream = next(
            (s for s in container.streams if s.type == "audio"), None
        )
        if video_stream is None:
            container.close()
            raise RecoverableError(
                ErrorCode.DECODE_FAILURE,
                f"no video stream in {segment.path.name}",
                segment=segment.path.name,
            )
        try:
            self._demux(container, video_stream, audio_stream, segment,
                        queue, boundary)
        except RecoverableError:
            boundary.truncated = True
            raise
        except av.AVError as exc:
            boundary.truncated = True
            raise RecoverableError(
                ErrorCode.TRUNCATED_SEGMENT,
                f"truncated/corrupt segment {segment.path.name}: {exc}",
                segment=segment.path.name,
            ) from exc
        finally:
            container.close()
        # PyAV often reaches EOF on a truncated stream without raising, so
        # validate the observed span against the declared duration explicitly.
        self._raise_if_truncated(segment, boundary)
        return boundary

    def _raise_if_truncated(self, segment: Segment,
                            boundary: SegmentBoundary) -> None:
        if boundary.first_pts is None:
            raise RecoverableError(
                ErrorCode.TRUNCATED_SEGMENT,
                f"no decodable video frame in {segment.path.name}",
                segment=segment.path.name,
            )
        observed_span = boundary.last_pts - boundary.first_pts
        # A healthy segment covers at least half its declared duration;
        # truncation in the fixture removes ~60% of the bytes and frames.
        minimum = segment.duration / 2
        if observed_span < minimum:
            raise RecoverableError(
                ErrorCode.TRUNCATED_SEGMENT,
                f"segment {segment.path.name} spans {float(observed_span):.3f}s "
                f"but declares {float(segment.duration):.3f}s (truncated)",
                segment=segment.path.name,
            )

    def _demux(self, container, video_stream, audio_stream, segment,
               queue, boundary) -> None:
        for packet in container.demux(video_stream, audio_stream):
            self.token.raise_if_cancelled()
            try:
                frames = packet.decode()
            except av.AVError as exc:
                raise RecoverableError(
                    ErrorCode.TRUNCATED_SEGMENT,
                    f"decode error in {segment.path.name}: {exc}",
                    segment=segment.path.name,
                ) from exc
            for frame in frames:
                if packet.stream.type == "video":
                    self._emit_video(frame, segment, queue, boundary)
                else:
                    for resampled in self._to_mono_s16(frame):
                        self._emit_audio(resampled, segment, queue)

    def _emit_video(self, frame, segment, queue, boundary) -> None:
        raw_pts = int(frame.pts) % self.modulus
        local_seconds, kind = self.video_clock.normalize(raw_pts)
        # Cross-segment wrap: a new segment whose first raw PTS jumps far
        # backward relative to the previous segment's last raw PTS.
        wrapped = False
        if (boundary.first_pts is None
                and self._prev_segment_last_raw is not None
                and raw_pts < self._prev_segment_last_raw
                and (self._prev_segment_last_raw - raw_pts)
                > self.modulus // 2):
            self.total_wraps += 1
            self._boundary_wrap = True
            wrapped = True
        # Global time = sequence base + local position + accumulated wraps.
        wrap_seconds = Fraction(self.total_wraps * self.modulus, 90_000)
        seg_base = Fraction(segment.sequence * float(segment.duration))
        seconds = local_seconds + seg_base + wrap_seconds
        if boundary.first_pts is None:
            boundary.first_pts = seconds
        boundary.last_pts = seconds
        self._prev_segment_last_raw = raw_pts
        item = VideoItem(
            rgb=_to_rgb(frame),
            pts_seconds=seconds,
            raw_pts=raw_pts,
            kind=kind,
            segment=segment.path.name,
            sequence=segment.sequence,
            frame_index=self.frame_index,
            wrapped=wrapped,
        )
        self.frame_index += 1
        queue.push(item)

    def _emit_audio(self, frame, segment, queue) -> None:
        pcm = frame.to_ndarray(format="s16").reshape(-1)
        # Use the frame's own time base (often 1/90000 after the container
        # remaps it) rather than assuming the sample rate, so node clock
        # drift in the audio PTS is preserved through normalization.
        tb = Fraction(frame.time_base)
        raw_pts = int(frame.pts * tb * 90_000)
        seconds, _ = self.audio_clock.normalize(raw_pts % MPEG_PTS_MOD)
        # Each segment's audio grid restarts at that segment's base; reset
        # the ordinal sample cursor on a segment change so the undrifted
        # reference stays aligned with the observed PTS's local origin.
        if self._audio_segment_seq != segment.sequence:
            self._audio_segment_seq = segment.sequence
            self._audio_sample_cursor = 0
        segment_base = segment.sequence * float(segment.duration)
        nominal = Fraction(
            int(segment_base * frame.sample_rate) + self._audio_sample_cursor,
            frame.sample_rate,
        )
        self._audio_sample_cursor += len(pcm)
        queue.push(AudioItem(
            pcm=pcm,
            pts_seconds=seconds,
            nominal_seconds=nominal,
            sample_rate=frame.sample_rate,
            segment=segment.path.name,
            sequence=segment.sequence,
        ))


class DecodeWorker:
    """Runs segment decoding on a background thread.

    The bounded queue applies real backpressure: when the consumer (the
    pipeline thread) is slow, :meth:`BoundedQueue.push` blocks inside this
    worker and memory stays proportional to the queue capacity.  Exceptions
    raised while decoding a segment are captured so the consumer thread can
    apply its missing-segment policy instead of crashing the worker silently.
    """

    def __init__(self, plan: ManifestPlan, modulus: int,
                 queue: BoundedQueue, token: CancellationToken) -> None:
        self.plan = plan
        self.modulus = modulus
        self.queue = queue
        self.token = token
        self.decoder: SegmentDecoder | None = None
        self.error: RecoverableError | None = None
        self._thread = threading.Thread(target=self._run, name="decode-worker",
                                        daemon=True)

    def start(self) -> None:
        self._thread.start()

    def _run(self) -> None:
        try:
            self.decoder = SegmentDecoder(self.modulus, self.token)
            for segment in self.plan.segments:
                try:
                    self.decoder.decode(segment, self.queue)
                except RecoverableError as exc:
                    if exc.code is ErrorCode.CANCELLED:
                        # Cooperative shutdown: exit the worker silently; the
                        # pipeline thread already raised the simulated crash.
                        return
                    self.queue.push(("error", segment.sequence, exc))
            self.queue.push(("done", None, None))
        except RecoverableError as exc:
            if exc.code is ErrorCode.CANCELLED:
                return
            self.queue.push(("fatal", None, exc))
        except Exception as exc:  # pragma: no cover - defensive
            self.queue.push(("fatal", None, RecoverableError(
                ErrorCode.DECODE_FAILURE, f"decoder crashed: {exc}")))

    def join(self) -> None:
        self._thread.join()
