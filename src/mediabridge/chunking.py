"""Turn one chunk of repaired packets into an encodable fragment.

Extracted from the planner so the quirky parts have one home:

* ``base`` is the *first* packet's repaired time per stream, not the
  minimum.  ``absolute_start`` (the fragment's ``tfdt``) rebases against
  that, and seeking relies on each chunk starting exactly where its first
  packet lands.
* Packets are renumbered *in place*.  ``av.Packet(p)`` copies silently drop
  ``is_keyframe``, and without that flag the muxer cannot mark a fragment's
  first video sample as a sync sample - libavformat then refuses backward
  seeks in the repaired file.
* Sample rows are matched to packets by per-stream order, because the muxer
  may reorder or coalesce within a fragment.
"""

from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction
from typing import TYPE_CHECKING

from .index_types import SampleRow
from .mp4write import ChunkEncoder, EncodedFragment
from .timeline import to_ticks

if TYPE_CHECKING:  # pragma: no cover
    from .pipeline import PlannedPacket


@dataclass
class Assembled:
    fragment: EncodedFragment
    samples: list[SampleRow]
    start_seconds: Fraction
    clock_state: dict
    packet_seq: dict
    origin: Fraction | None


def assemble_chunk(*, chunk_index: int, packets: "list[PlannedPacket]",
                   encoder: ChunkEncoder, timescales: dict[int, int],
                   track_by_stream, video_stream: int | None,
                   audio_index_stride: int, clock_state: dict,
                   packet_seq: dict, origin: Fraction | None) -> Assembled:
    """Encode ``packets`` into a fragment and derive its index sample rows."""
    # First repaired time per stream fixes both the tfdt origin and the
    # chunk-local numbering.
    base: dict[int, Fraction] = {}
    for planned in packets:
        sid = planned.info.stream_index
        if sid not in base:
            base[sid] = planned.decision.dts_seconds
    absolute_start = {
        sid: to_ticks(start, Fraction(1, timescales[sid]))
        for sid, start in base.items()
    }

    for planned in packets:
        sid = planned.info.stream_index
        offset = to_ticks(base[sid], planned.info.time_base)
        planned.packet.dts = planned.decision.dts - offset
        planned.packet.pts = planned.decision.pts - offset
        planned.packet.time_base = planned.info.time_base

    fragment = encoder.encode([p.packet for p in packets], absolute_start=absolute_start)

    kept_by_stream: dict[int, list["PlannedPacket"]] = {}
    for planned in packets:
        kept_by_stream.setdefault(planned.info.stream_index, []).append(planned)

    samples: list[SampleRow] = []
    cursor: dict[int, int] = {}
    for sample in fragment.samples:
        sid = sample.stream_index
        bucket = kept_by_stream.get(sid, [])
        position = cursor.get(sid, 0)
        if position >= len(bucket):
            continue
        cursor[sid] = position + 1
        planned = bucket[position]
        track = track_by_stream[sid]
        is_key = sid == video_stream and bool(planned.info.is_keyframe)
        audio_due = (sid != video_stream
                     and (planned.packet_seq == 0
                          or planned.packet_seq % audio_index_stride == 0))
        if not is_key and not audio_due:
            continue
        samples.append(SampleRow(
            track_index=track.track_index,
            kind=track.kind,
            packet_seq=planned.packet_seq,
            global_packet=planned.info.global_index,
            keyframe=is_key,
            dts_ticks=planned.decision.dts,
            pts_ticks=planned.decision.pts,
            time_seconds=planned.decision.dts_seconds,
            byte_offset=sample.frag_offset,
            byte_size=sample.size,
            chunk_index=chunk_index,
        ))

    return Assembled(
        fragment=fragment,
        samples=samples,
        start_seconds=min(base.values()) if base else Fraction(0),
        clock_state=clock_state,
        packet_seq=packet_seq,
        origin=origin,
    )
