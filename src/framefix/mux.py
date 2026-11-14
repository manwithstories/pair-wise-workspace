"""Remux compressed packets according to a shared RepairPlan."""

from __future__ import annotations

import os
import tempfile
from pathlib import Path

import av

from .compat import add_stream_like
from .errors import RepairError
from .repair import RepairPlan


def mux_with_plan(input_path: str, output_path: str, plan: RepairPlan) -> dict[str, int]:
    """Write a timestamp-corrected MP4 without decoding or re-encoding frames."""
    source = Path(input_path)
    target = Path(output_path)
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(
        prefix=f".{target.stem}.",
        suffix=target.suffix or ".mp4",
        dir=str(target.parent),
    )
    os.close(fd)

    video_packets = 0
    copied_packets = 0
    try:
        with av.open(str(source)) as input_container, av.open(temporary, "w") as output_container:
            output_streams: dict[int, av.Stream] = {}
            for stream in input_container.streams:
                output_streams[stream.index] = add_stream_like(output_container, stream)

            video_index = plan.info.stream_index
            for packet in input_container.demux():
                if not packet.size:
                    continue
                output_stream = output_streams.get(packet.stream_index)
                if output_stream is None:
                    continue
                packet.stream = output_stream
                if packet.stream_index == video_index:
                    seq = video_packets
                    video_packets += 1
                    if seq not in plan.packet_pts or seq not in plan.packet_dts:
                        raise RepairError(
                            f"repair plan has no timestamps for video packet {seq}"
                        )
                    packet.pts = plan.packet_pts[seq]
                    packet.dts = plan.packet_dts[seq]
                copied_packets += 1
                output_container.mux(packet)
        os.replace(temporary, target)
    except RepairError:
        _remove(temporary)
        raise
    except (av.error.FFmpegError, OSError, ValueError) as exc:
        _remove(temporary)
        raise RepairError(f"cannot write repaired media: {exc}") from exc

    return {
        "video_packets": video_packets,
        "copied_packets": copied_packets,
        "bytes": target.stat().st_size,
    }


def _remove(path: str) -> None:
    try:
        os.unlink(path)
    except FileNotFoundError:
        pass

