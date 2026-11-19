"""Lossless remux: write frame payloads into the delivery container.

Payloads are wrapped as rawvideo packets — never re-encoded.  Per-frame
exact nanosecond timestamps, metadata and payload digests are kept in a
sidecar index (``<output>.index.json``) so validation can prove the
round trip even where the container's own timestamp grid is coarser.
Unsupported pixel formats raise a locatable FieldPackError instead of
triggering any implicit conversion.
"""
from __future__ import annotations

import hashlib
import json
from fractions import Fraction
from pathlib import Path

from .mapping import MapPlan
from .model import (
    PIX_FMTS,
    FieldPackError,
    FrameRecord,
    Segment,
    Stage,
    TrackInfo,
)

_NS_BASE = Fraction(1, 1_000_000_000)


class Muxer:
    def __init__(
        self,
        out_path: str | Path,
        plan: MapPlan,
        tracks: dict[int, TrackInfo],
    ) -> None:
        import av  # lazy import: dependency errors surface as exit code 2

        self.out_path = Path(out_path)
        self.index_path = self.out_path.with_suffix(self.out_path.suffix + ".index.json")
        self._av = av
        # NUT: an ffmpeg-native container that stores rawvideo losslessly
        # and keeps the exact 1/1e9 time base (matroska rejects rawvideo).
        self._container = av.open(str(self.out_path), "w", format="nut")
        self._streams: dict[int, "av.video.stream.VideoStream"] = {}
        self._outputs: dict[int, str] = {}
        for vm in plan.videos:
            info = tracks[vm.device_id]
            if info.pixfmt not in PIX_FMTS.values():
                raise FieldPackError(
                    code="unsupported-pixfmt",
                    message=f"pixel format {info.pixfmt!r} cannot be stored losslessly",
                    stage=Stage.MUX,
                    recoverable=False,
                )
            stream = self._container.add_stream("rawvideo")
            stream.width = info.width
            stream.height = info.height
            stream.pix_fmt = info.pixfmt
            stream.time_base = _NS_BASE
            stream.metadata["fieldpack.output"] = vm.output
            stream.metadata["fieldpack.device"] = str(vm.device_id)
            self._streams[vm.device_id] = stream
            self._outputs[vm.device_id] = vm.output
        # Sidecar index: exact ns timestamps, metadata, digests, keyframes.
        self._frames: list[dict] = []
        self._segments: dict[tuple[int, int], dict] = {}
        self._audio = plan.audio.summary() if plan.audio else None
        self._closed = False

    def write_frame(self, record: FrameRecord, pts_ns: int, segment: Segment) -> None:
        if self._closed:
            raise FieldPackError(
                code="mux-closed", message="write after close", stage=Stage.MUX,
                offset=record.offset, recoverable=False,
            )
        if record.pixfmt not in PIX_FMTS.values():
            raise FieldPackError(
                code="unsupported-pixfmt",
                message=f"pixel format {record.pixfmt!r} not supported by delivery container",
                stage=Stage.MUX,
                offset=record.offset,
            )
        import numpy as np

        stream = self._streams[record.device_id]
        arr = np.frombuffer(record.payload, dtype=np.uint8)
        if record.pixfmt == "yuv420p":
            arr = arr.reshape(record.height * 3 // 2, record.width)
        elif record.pixfmt == "gray":
            arr = arr.reshape(record.height, record.width)
        else:
            arr = arr.reshape(record.height, record.width, -1)
        frame = self._av.VideoFrame.from_ndarray(arr, format=record.pixfmt)
        frame.pts = pts_ns
        frame.time_base = _NS_BASE
        try:
            for packet in stream.encode(frame):
                self._container.mux(packet)
        except Exception as exc:  # container-level write failure: unrecoverable
            raise FieldPackError(
                code="mux-write-failed",
                message=f"container write failed: {exc}",
                stage=Stage.MUX,
                offset=record.offset,
                recoverable=False,
            ) from exc
        digest = hashlib.sha256(record.payload).hexdigest()
        key = (record.device_id, segment.index)
        seg = self._segments.setdefault(
            key,
            {
                "device_id": record.device_id,
                "segment": segment.index,
                "start_offset": segment.start_offset,
                "hash": hashlib.sha256(),
                "frames": 0,
            },
        )
        seg["hash"].update(record.payload)
        seg["frames"] += 1
        self._frames.append(
            {
                "device_id": record.device_id,
                "segment": segment.index,
                "pts_ns": pts_ns,
                "offset": record.offset,
                "keyframe": record.keyframe,
                "sha256": digest,
                "meta": record.meta.decode("utf-8", "replace"),
            }
        )

    def close(self) -> None:
        """Flush encoders and persist the sidecar index (completed segments
        are preserved even when the run was cancelled mid-segment)."""
        if self._closed:
            return
        self._closed = True
        for stream in self._streams.values():
            try:
                for packet in stream.encode(None):
                    self._container.mux(packet)
            except Exception:
                pass
        self._container.close()
        index = {
            "container": self.out_path.name,
            "audio": self._audio,
            "frames": self._frames,
            "keyframes": [
                {"device_id": f["device_id"], "pts_ns": f["pts_ns"]}
                for f in self._frames
                if f["keyframe"]
            ],
            "segments": [
                {
                    "device_id": s["device_id"],
                    "segment": s["segment"],
                    "start_offset": s["start_offset"],
                    "frames": s["frames"],
                    "sha256": s["hash"].hexdigest(),
                }
                for s in sorted(self._segments.values(), key=lambda s: (s["device_id"], s["segment"]))
            ],
        }
        self.index_path.write_text(json.dumps(index, indent=1))
