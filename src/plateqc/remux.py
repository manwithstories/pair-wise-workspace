"""Normalize: copy packets with repaired timestamps into a fresh MP4.

No re-encode. The repaired timeline is the only timestamp source. The output
is written to a temporary sibling first and renamed on success, so a failed
run never clobbers an existing file at ``--output``.
"""

from __future__ import annotations

import os

import av
import numpy as np

from .probe import open_container, select_stream
from .timeline import FrameTimeline


def normalize(
    input_path: str,
    output_path: str,
    timeline: FrameTimeline,
    stream_index: int | None = None,
) -> dict:
    """Remux the video stream of ``input_path`` with repaired timestamps."""
    container = open_container(input_path)
    tmp_path = output_path + ".part"
    written = 0
    try:
        stream = select_stream(container, stream_index)
        inv = timeline.decode_to_row()
        # Reorder delay in frames: how far a frame's display row lags its
        # decode position. DTS is rebuilt monotonic in decode order.
        delay = max(0, int((timeline.decode_index - np.arange(timeline.count)).max()))
        interval = timeline.interval

        out_container = av.open(tmp_path, "w", format="mp4")
        try:
            out_stream = out_container.add_stream(template=stream)
            try:
                for packet in container.demux(stream):
                    if packet.size == 0:
                        continue
                    if written >= timeline.count:
                        break  # truncated input: packets beyond decoded frames
                    packet.stream = out_stream
                    packet.pts = int(timeline.pts_repaired[inv[written]])
                    packet.dts = (written - delay) * interval
                    out_container.mux(packet)
                    written += 1
            except av.AVError:
                pass  # truncated tail: keep packets muxed so far
        finally:
            out_container.close()
        os.replace(tmp_path, output_path)
    except BaseException:
        if os.path.exists(tmp_path):
            os.remove(tmp_path)
        raise
    finally:
        container.close()

    return {
        "frames": written,
        "duration_ms": int(round(timeline.to_ms(timeline.duration_units))),
        "first_pts_ms": int(round(timeline.to_ms(timeline.pts_repaired[0])))
        if timeline.count
        else 0,
        "last_pts_ms": int(round(timeline.to_ms(timeline.pts_repaired[-1])))
        if timeline.count
        else 0,
    }
