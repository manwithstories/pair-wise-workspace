"""Full decode loop with bounded packet/frame matching and NumPy summaries."""

from __future__ import annotations

import hashlib
from collections import deque
from fractions import Fraction
from typing import Iterator, Optional

import av
import numpy as np

from .errors import DecodeError
from .models import FrameRecord, PacketMeta, frame_type_name
from .probe import open_video, packet_meta


def pixel_summary(frame: av.VideoFrame) -> tuple[str, float]:
    """Hash every visible decoded plane and compute the luma mean with NumPy."""
    digest = hashlib.blake2b(digest_size=16)
    digest.update(str(frame.format.name).encode("ascii", "replace"))
    luma_mean = 0.0
    for plane_index, plane in enumerate(frame.planes):
        array = np.asarray(plane)
        if array.ndim == 1:
            array = array.reshape(plane.height, plane.line_size)
        elif array.ndim > 2:
            array = array.reshape(array.shape[-2], array.shape[-1])
        visible = array[: plane.height, : plane.width]
        contiguous = np.ascontiguousarray(visible)
        digest.update(str(contiguous.shape).encode("ascii"))
        digest.update(contiguous.tobytes())
        if plane_index == 0 and contiguous.size:
            luma_mean = round(float(np.mean(contiguous, dtype=np.float64)), 6)
    return digest.hexdigest(), luma_mean


def _take_pending(
    pending: dict[Optional[int], deque[PacketMeta]],
    pts: Optional[int],
    dts: Optional[int],
    pict_type: str,
) -> Optional[PacketMeta]:
    queue = pending.get(pts)
    if queue:
        desired_keyframe = pict_type == "I"
        for index, candidate in enumerate(queue):
            if candidate.is_keyframe == desired_keyframe:
                item = queue[index]
                del queue[index]
                if not queue:
                    pending.pop(pts, None)
                return item

    for key, candidate in list(pending.items()):
        if dts is not None and candidate and candidate[0].dts == dts:
            item = candidate.popleft()
            if not candidate:
                pending.pop(key, None)
            return item
    if pending:
        key = min((k for k in pending if k is not None), default=None)
        if key is not None:
            item = pending[key].popleft()
            if not pending[key]:
                pending.pop(key, None)
            return item
    return None


def iter_frame_records(path: str, *, include_pixels: bool = True) -> Iterator[FrameRecord]:
    """Decode *path* once, yielding compact frame records in decoder output order."""
    container, stream, info = open_video(path)
    codec = stream.codec_context
    pending: dict[Optional[int], deque[PacketMeta]] = {}
    packet_seq = 0
    frame_index = 0
    try:
        for packet in container.demux(stream):
            if not packet.size:
                continue
            meta = packet_meta(packet, packet_seq, info.time_base)
            pending.setdefault(meta.pts, deque()).append(meta)
            packet_seq += 1
            try:
                frames = codec.decode(packet)
            except (av.error.FFmpegError, ValueError) as exc:
                raise DecodeError(
                    f"decode error after {frame_index} frames: {exc}",
                    frames_emitted=frame_index,
                ) from exc
            for frame in frames:
                pict_type = frame_type_name(frame.pict_type)
                matched = _take_pending(pending, frame.pts, frame.dts, pict_type)
                record = _to_record(frame, frame_index, info.time_base, matched, include_pixels)
                frame_index += 1
                yield record

        try:
            tail = codec.decode(None)
        except (av.error.FFmpegError, ValueError) as exc:
            raise DecodeError(
                f"decoder flush error after {frame_index} frames: {exc}",
                frames_emitted=frame_index,
            ) from exc
        for frame in tail:
            pict_type = frame_type_name(frame.pict_type)
            matched = _take_pending(pending, frame.pts, frame.dts, pict_type)
            record = _to_record(frame, frame_index, info.time_base, matched, include_pixels)
            frame_index += 1
            yield record
    except DecodeError:
        raise
    except (av.error.FFmpegError, EOFError, OSError, ValueError) as exc:
        raise DecodeError(
            f"truncated or invalid stream after {frame_index} frames: {exc}",
            frames_emitted=frame_index,
        ) from exc
    finally:
        container.close()


def _to_record(
    frame: av.VideoFrame,
    index: int,
    time_base: Fraction,
    matched: Optional[PacketMeta],
    include_pixels: bool,
) -> FrameRecord:
    digest, luma_mean = pixel_summary(frame) if include_pixels else ("", 0.0)
    return FrameRecord(
        index=index,
        pts=frame.pts,
        dts=frame.dts,
        time_base=time_base,
        pict_type=frame_type_name(frame.pict_type),
        width=int(frame.width),
        height=int(frame.height),
        pixel_digest=digest,
        luma_mean=luma_mean,
        packet_seq=None if matched is None else matched.seq,
        packet_keyframe=None if matched is None else matched.is_keyframe,
    )


class PixelSequence:
    """Constant-memory rolling digest over the ordered per-frame summaries."""

    def __init__(self) -> None:
        self._digest = hashlib.blake2b(digest_size=32)
        self.frames = 0

    def update(self, record: FrameRecord) -> None:
        self._digest.update(record.pixel_digest.encode("ascii"))
        self._digest.update(b"\x00")
        self.frames += 1

    @property
    def hexdigest(self) -> str:
        return self._digest.hexdigest()
