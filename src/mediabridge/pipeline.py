"""Deterministic chunk planner shared by ``repair`` and ``verify``.

The planner is the single implementation of "what should the repaired file
contain".  It consumes demuxed packets in order and yields
:class:`PlannedChunk` objects.  ``repair`` writes them to disk and commits
them to the index; ``verify`` recomputes the same plan from the source and
compares per-chunk digests with the stored index.  Because both paths use
this class, a resumed run and a full run provably agree.

Chunk boundaries
----------------
A chunk closes when it already holds ``chunk_packets`` packets and the next
packet is a video keyframe (audio-only files close on packet count).  The
rule depends only on the packet stream, so it is stable across
interruptions - the reason a resumed run reproduces the full run byte for
byte.

Damage handling
---------------
Corruption is localised by decoding.  Audio packets are validated with a
*fresh* decoder per packet, which makes detection stateless and therefore
identical on every run.  Video packets are decoded with one context per
chunk; the context is reset at every chunk boundary, which always begins at
a keyframe, so a resumed run starting at a committed boundary sees exactly
the decoder state an uninterrupted run would.

A video decode error drops every following non-keyframe packet up to the
next keyframe (a P-frame that references the lost packet would paint
garbage); an audio error drops only the damaged packet.  Each dropped run is
recorded as a :class:`DamageRow` naming the first offending packet, and the
affected clock resynchronises instead of treating the gap as a timestamp
discontinuity.
"""

from __future__ import annotations

import warnings
from dataclasses import dataclass, field
from fractions import Fraction
from typing import Iterable

import av

from .chunking import assemble_chunk
from .errors import StructuralError
from .validators import AudioValidator, VideoValidator
from .index import DamageRow, SampleRow
from .mp4write import ChunkEncoder, EncodedFragment
from .probe import PacketInfo, ProbeResult
from .timeline import (
    RepairDecision,
    TimelineProfile,
    TrackClock,
    TrackSpec,
    build_clocks,
    seconds,
    to_ticks,
)

try:  # PyAV <=12 exposes the warning at module level; newer versions moved it.
    warnings.filterwarnings("ignore", category=av.AVDeprecationWarning)
except AttributeError:  # pragma: no cover
    warnings.filterwarnings("ignore", message=".*CodecContext.time_base.*")


@dataclass
class PlannedPacket:
    info: PacketInfo
    packet_seq: int
    decision: RepairDecision
    size: int
    packet: "av.Packet"


@dataclass
class PlannedChunk:
    chunk_index: int
    packets: list[PlannedPacket]
    damage: list[DamageRow]
    samples: list[SampleRow]
    fragment: EncodedFragment
    init_segment: bytes
    body: bytes
    start_seconds: Fraction
    first_global_packet: int
    last_global_packet: int
    first_packet_seq: int
    last_packet_seq: int
    byte_offset: int = 0
    bytes_sha256: str = ""
    rows_sha256: str = ""
    # Snapshot of planner state taken *inside* ``_flush`` - i.e. exactly at
    # the committed byte boundary.  The checkpoint persists these, and a
    # resumed run restores them, so the boundary is reproduced precisely.
    clock_state: dict = field(default_factory=dict)
    packet_seq: dict = field(default_factory=dict)
    origin: Fraction | None = None
    damage_count: int = 0

    @property
    def packet_count(self) -> int:
        return len(self.packets)

    @property
    def file_length(self) -> int:
        return len(self.body)


@dataclass
class PlannedDamage:
    track_index: int
    start_packet: int
    end_packet: int
    reason: str
    dts: int | None = None
    pts: int | None = None
    notes: dict | None = None


class ChunkPlanner:
    """Turn demuxed packets into deterministic, indexable chunks."""

    def __init__(self, probe_result: ProbeResult, streams: Iterable[av.stream.Stream],
                 profile: TimelineProfile, *, chunk_packets: int = 128,
                 detect_damage: bool = True, consumer_delay: float = 0.0,
                 audio_index_stride: int = 16):
        self.probe = probe_result
        self.streams = list(streams)
        self.profile = profile
        self.chunk_packets = max(1, chunk_packets)
        self.detect_damage = detect_damage
        self.consumer_delay = consumer_delay
        self.audio_index_stride = max(1, audio_index_stride)

        self.tracks: list[TrackSpec] = list(probe_result.tracks)
        self.clocks: dict[int, TrackClock] = build_clocks(self.tracks, profile)
        self.encoder = ChunkEncoder(self.streams)
        self.audio_validators = {
            spec.stream_index: AudioValidator(self._stream(spec.stream_index))
            for spec in self.tracks if spec.kind == "audio"
        }
        self.video_validators = {
            spec.stream_index: VideoValidator(self._stream(spec.stream_index))
            for spec in self.tracks if spec.kind == "video"
        }

        self.track_by_stream = {spec.stream_index: spec for spec in self.tracks}
        self.packet_seq = {spec.stream_index: 0 for spec in self.tracks}
        self.timescales: dict[int, int] = {}
        self.origin: Fraction | None = None

        self.chunk_index = 0
        self.pending: list[PlannedPacket] = []
        self.pending_damage: list[DamageRow] = []
        self.damage: list[DamageRow] = []           # restored from checkpoint
        self.committed_damage: list[DamageRow] = [] # rows already in the index
        self.samples_written = 0
        self.samples_offset = 0
        self.committed_chunks: list[PlannedChunk] = []
        self.first_error_packet: int | None = None
        self.packets_seen = 0
        self.packets_written = 0
        self.packets_dropped = 0

        self.plan_only = False  # verify uses plan-only mode (no file output)
        self.timestamp_repairs: list[dict] = []
        self.video_skip_until_keyframe: set[int] = set()
        self.video_damage_start: dict[int, int] = {}
        self.video_damage_meta: dict[int, dict] = {}
        self.video_stream = next((s.stream_index for s in self.tracks if s.kind == "video"), None)

    # -- setup ------------------------------------------------------------
    def _stream(self, index: int) -> av.stream.Stream:
        for stream in self.streams:
            if stream.index == index:
                return stream
        raise StructuralError(f"stream {index} disappeared")

    def set_timescales(self, timescales: dict[int, int]) -> None:
        self.timescales = dict(timescales)
        if not self.timescales:
            raise StructuralError("could not determine output timescales")

    def clock_state(self) -> dict[int, dict]:
        return {index: clock.state_dict() for index, clock in self.clocks.items()}

    def set_resume_state(self, *, chunk_index: int, packet_seq: dict[int, int],
                         origin: Fraction | None, samples_written: int,
                         first_error_packet: int | None,
                         clocks: dict[int, dict] | None = None,
                         samples_offset: int = 0,
                         damage: list[DamageRow] | None = None) -> None:
        self.chunk_index = chunk_index
        for stream_index, value in packet_seq.items():
            if stream_index in self.packet_seq:
                self.packet_seq[stream_index] = int(value)
        self.origin = origin
        # ``samples_written`` counts rows already committed; accumulate from
        # there so final counts cover the whole run, not just the tail.
        self.samples_written = samples_written
        self.samples_offset = samples_offset
        self.first_error_packet = first_error_packet
        if damage:
            self.damage = list(damage)
        if clocks:
            for stream_index, state in clocks.items():
                clock = self.clocks.get(int(stream_index))
                if clock is not None:
                    clock.restore_state(state)
                    if self.origin is None:
                        self.origin = clock.origin

    # -- ingest -----------------------------------------------------------
    def ingest(self, info: PacketInfo, packet: av.Packet) -> list[PlannedChunk]:
        """Feed one demuxed packet; returns chunks closed by this packet."""
        self.packets_seen += 1
        track = self.track_by_stream.get(info.stream_index)
        if track is None:
            return []
        emitted: list[PlannedChunk] = []
        if self.consumer_delay:
            import time
            time.sleep(self.consumer_delay)

        # 1. Stateless damage validation.
        problem = self._validate(info, packet)
        if problem is not None:
            self.packets_dropped += 1
            self._begin_video_skip(info)
            self._record_damage(PlannedDamage(
                track_index=info.track_index,
                start_packet=info.global_index,
                end_packet=info.global_index,
                reason=problem,
                dts=info.dts,
                pts=info.pts,
                notes={"detected_by": "decoder"},
            ))
            if info.stream_index != self.video_stream:
                # Audio damage drops a single packet; tell the clock not to
                # read the following gap as a timestamp anomaly.
                self.clocks[info.stream_index].resync()
            return emitted

        # 2. Video resynchronisation: drop non-keyframes until a keyframe.
        if info.stream_index in self.video_skip_until_keyframe:
            if info.is_keyframe:
                self.video_skip_until_keyframe.discard(info.stream_index)
                self._close_video_damage(info)
            else:
                self.packets_dropped += 1
                return emitted

        # 3. Close the chunk when the next packet is a video keyframe.
        if self._should_close(info):
            emitted.append(self._flush())

        decision = self._observe(track, info)
        if decision.discontinuity:
            self.timestamp_repairs.append({
                "track_index": info.track_index,
                "global_packet": info.global_index,
                "dts": decision.raw_dts,
                "pts": decision.raw_pts,
                "deviation_seconds": float(decision.deviation_seconds or 0),
                "note": decision.note,
            })
        self.pending.append(PlannedPacket(
            info=info,
            packet_seq=self.packet_seq[info.stream_index],
            decision=decision,
            size=info.size,
            packet=packet,
        ))
        self.packet_seq[info.stream_index] += 1
        return emitted

