"""Recovery for truncated files, corrupt packets and short indexes.

Three failure modes, three explicit policies.  One rule is absolute: **never
fabricate content**.  A packet is either written byte-for-byte from the input,
or skipped and counted.  Nothing is ever decoded and re-encoded, and no
bytes are synthesised to pad a hole.

Tail truncation
    The ``moov`` index still describes samples that no longer exist on disk.
    A boundary scan compares every indexed sample range with the bytes that
    actually survive, and reports the furthest fully-present sample.  Packets
    crossing the boundary are skipped with their byte offset; everything
    before it is kept.

Locally corrupt packets
    The container is intact but a payload fails structural validation (for
    H.264/HEVC the length-prefixed AVCC framing does not add up, for MPEG-4
    the start code is gone, an AAC payload cannot hold a frame header).  The
    packet is skipped and recorded; its neighbours are unaffected.

Incomplete index
    ``mdat`` holds more media than ``moov`` indexes, or the sample tables
    contradict each other.  The index is treated as the source of truth for
    *which* samples exist; packets whose recorded position falls outside the
    ``mdat`` payload (libavformat happily returns such entries) are skipped,
    the surviving packets are copied, and the muxer rebuilds a consistent
    index from exactly what was written.  The unreferenced tail is reported,
    never invented.

The recovery outcome feeds the track graph, and the remux stage writes tracks
in graph dependency order (video before audio) with the index last.
"""

from __future__ import annotations

import time
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

import av

from . import container
from .demux import AssetProbe, RawPacket, StreamInfo, iter_packets
from .errors import AssetTimeoutError, MediaLocation, QCError, RecoverableError, SkipError
from .timeline import PacketSpan, build_stream_timeline, seconds


@dataclass(slots=True)
class BoundaryScan:
    """Result of comparing the sample index with the bytes actually present."""

    usable_until: int | None = None
    declared_end: int | None = None
    scanned_bytes: int = 0
    unindexed_bytes: int = 0
    truncated: bool = False
    per_track_usable: dict[int, int] = field(default_factory=dict)

    @property
    def missing_bytes(self) -> int:
        """Bytes the index references beyond what actually survived.

        Measured from the last usable sample to the *declared* end, so a file
        cut from 113 KB to 97 KB reports the ~16 KB that are gone rather than
        the handful of bytes between the last sample and the cut.
        """
        if self.usable_until is None or self.declared_end is None:
            return 0
        return max(0, self.declared_end - self.usable_until)

    def to_dict(self) -> dict[str, object]:
        return {
            "usable_until": self.usable_until,
            "declared_end": self.declared_end,
            "scanned_bytes": self.scanned_bytes,
            "unindexed_bytes": self.unindexed_bytes,
            "missing_bytes": self.missing_bytes,
            "truncated": self.truncated,
            "per_track_usable_samples": {
                str(k): v for k, v in sorted(self.per_track_usable.items())
            },
        }


def boundary_scan(index: container.IndexReport) -> BoundaryScan:
    """Determine how much of the declared media actually exists."""
    scan = BoundaryScan(
        # The declared end is what the mdat *header* promises, not the clipped
        # length of the file: subtracting the surviving end would under-report
        # the missing tail by the whole truncated span.
        declared_end=index.mdat_declared_end,
        truncated=index.truncated_tail,
        unindexed_bytes=index.unindexed_bytes(),
    )
    usable_until = index.mdat_start
    for track in index.tracks:
        ranges = track.sample_ranges()
        usable = 0
        for offset, size in ranges:
            if index.mdat_end is not None and offset + size > index.mdat_end:
                break
            if index.mdat_start is not None and offset < index.mdat_start:
                break
            usable += 1
            usable_until = max(usable_until or 0, offset + size)
        scan.per_track_usable[track.track_id] = usable
    scan.usable_until = usable_until
    if index.mdat_start is not None and index.mdat_end is not None:
        scan.scanned_bytes = max(0, index.mdat_end - index.mdat_start)
    return scan


@dataclass(slots=True)
class PacketDecision:
    packet: RawPacket
    keep: bool
    reason: str | None = None
    error: QCError | None = None


@dataclass(slots=True)
class RecoveryPlan:
    """What the recovery stage will do, decided before anything is written."""

    strategies: list[str]
    truncated: bool
    corrupt_packets: int
    incomplete_index: bool
    repair_index: bool
    expected_packets: int
    notes: list[str] = field(default_factory=list)
    issues: list[QCError] = field(default_factory=list)

    @property
    def recovered(self) -> bool:
        return bool(self.strategies) and "none" not in self.strategies

    def to_dict(self) -> dict[str, object]:
        return {
            "strategies": list(self.strategies),
            "truncated": self.truncated,
            "corrupt_packets": self.corrupt_packets,
            "incomplete_index": self.incomplete_index,
            "repair_index": self.repair_index,
            "expected_packets": self.expected_packets,
            "notes": list(self.notes),
            "note": "expected_packets is derived from declared durations vs covered span",
            "issues": [issue.to_dict() for issue in self.issues],
        }


@dataclass(slots=True)
class RecoveryOutcome:
    plan: RecoveryPlan
    packets: list[RawPacket]
    skipped: list[PacketDecision]
    recovered_packets: int
    total_packets: int
    expected_packets: int
    scan: BoundaryScan
    per_stream: dict[int, dict[str, int]] = field(default_factory=dict)

    @property
    def recovery_rate(self) -> float:
        """Recovered packets over the declared expectation, unclamped.

        The value is deliberately *not* capped at 1.0: if recovery somehow
        yields more packets than the declared media implies, the estimate is
        wrong and a report reading 1.0 would hide it.  Callers treat >1.0 as
        "expectation unexplained" rather than as a perfect score.
        """
        if self.expected_packets <= 0:
            return 0.0
        return self.recovered_packets / self.expected_packets

    @property
    def over_recovered(self) -> bool:
        return self.expected_packets > 0 and self.recovered_packets > self.expected_packets

    @property
    def skipped_count(self) -> int:
        return len(self.skipped)

    def to_dict(self) -> dict[str, object]:
        return {
            "plan": self.plan.to_dict(),
            "recovered_packets": self.recovered_packets,
            "total_packets": self.total_packets,
            "expected_packets": self.expected_packets,
            "recovery_rate": round(self.recovery_rate, 6),
            "over_recovered": self.over_recovered,
            "boundary_scan": self.scan.to_dict(),
            "per_stream": {str(k): v for k, v in sorted(self.per_stream.items())},
            "skipped": [
                {
                    "stream": d.packet.stream,
                    "pos": d.packet.pos,
                    "size": d.packet.size,
                    "reason": d.reason,
                    "error": d.error.to_dict() if d.error else None,
                }
                for d in self.skipped
            ],
        }


# --------------------------------------------------------------------------
# structural packet validation
# --------------------------------------------------------------------------


def avcc_lengths_ok(payload: bytes) -> bool:
    """H.264/HEVC MP4 payloads are a sequence of 4-byte big-endian lengths."""
    if not payload:
        return False
    offset = 0
    total = len(payload)
    while offset + 4 <= total:
        length = int.from_bytes(payload[offset : offset + 4], "big")
        offset += 4
        if length <= 0 or offset + length > total:
            return False
        offset += length
    return offset == total


def mpeg4_start_code_ok(payload: bytes) -> bool:
    return payload.startswith(b"\x00\x00\x01") or payload.startswith(b"\x00\x00\x00\x01")


def validate_packet(stream: StreamInfo, payload: bytes) -> tuple[bool, str | None]:
    """Structural (not decode) validation of one compressed packet."""
    if not payload:
        return False, "empty payload"
    if stream.type == "video" and stream.codec in {"h264", "hevc"}:
        if not avcc_lengths_ok(payload):
            return False, "AVCC length framing is inconsistent"
    elif stream.type == "video" and stream.codec == "mpeg4":
        if not mpeg4_start_code_ok(payload):
            return False, "MPEG-4 start code missing"
    elif stream.type == "audio" and stream.codec == "aac":
        if len(payload) < 2:
            return False, "AAC payload shorter than one frame header"
    return True, None


# --------------------------------------------------------------------------
# demux + classify
# --------------------------------------------------------------------------


def classify_packets(
    probe: AssetProbe,
    index: container.IndexReport,
    scan: BoundaryScan,
    *,
    packet_budget: int | None = None,
    deadline: float | None = None,
) -> tuple[list[RawPacket], list[PacketDecision], int]:
    """Demux, validate, and decide keep/skip for every packet."""
    by_index = {info.index: info for info in probe.streams}
    mdat_start, mdat_end = index.mdat_start, index.mdat_end
    kept: list[RawPacket] = []
    skipped: list[PacketDecision] = []
    total = 0

    for packet in iter_packets(probe.path):
        total += 1
        info = by_index.get(packet.stream)
        if info is None:
            continue
        if deadline is not None and total % 256 == 0 and time.monotonic() > deadline:
            # The timeout is enforced inside the packet walk, not only between
            # stages: a pathological asset with a huge packet count stops here.
            raise AssetTimeoutError(
                "asset exceeded the processing timeout while demuxing",
                location=MediaLocation(
                    asset=str(probe.path), stream=packet.stream, packet_pos=packet.pos
                ),
            )
        stamp = seconds(packet.pts, packet.time_base)
        location = MediaLocation(
            asset=str(probe.path),
            stream=packet.stream,
            packet_pos=packet.pos,
            timestamp=stamp,
        )
        if packet_budget is not None and len(kept) >= packet_budget:
            skipped.append(
                PacketDecision(
                    packet=packet,
                    keep=False,
                    reason="packet budget exhausted",
                    error=SkipError(
                        "packet budget exhausted before this packet",
                        location=location,
                    ),
                )
            )
            continue
        if packet.pos is not None and mdat_start is not None and packet.pos < mdat_start:
            skipped.append(
                PacketDecision(
                    packet=packet,
                    keep=False,
                    reason="indexed position falls outside mdat",
                    error=SkipError(
                        "packet position is outside the media payload",
                        location=MediaLocation(
                            asset=str(probe.path),
                            stream=packet.stream,
                            packet_pos=packet.pos,
                            timestamp=stamp,
                            detail="incomplete index",
                        ),
                    ),
                )
            )
            continue
        if packet.pos is not None and mdat_end is not None and packet.pos + packet.size > mdat_end:
            skipped.append(
                PacketDecision(
                    packet=packet,
                    keep=False,
                    reason="packet crosses the truncation boundary",
                    error=RecoverableError(
                        "packet extends beyond the available bytes",
                        location=MediaLocation(
                            asset=str(probe.path),
                            stream=packet.stream,
                            packet_pos=packet.pos,
                            timestamp=stamp,
                            detail="truncated tail",
                        ),
                        code="truncated_packet",
                    ),
                )
            )
            continue
        if packet.corrupt:
            skipped.append(
                PacketDecision(
                    packet=packet,
                    keep=False,
                    reason="container flagged packet as corrupt",
                    error=SkipError("packet flagged corrupt by the demuxer", location=location),
                )
            )
            continue
        ok, why = validate_packet(info, packet.data)
        if not ok:
            skipped.append(
                PacketDecision(
                    packet=packet,
                    keep=False,
                    reason=why or "validation failed",
                    error=SkipError(
                        f"packet failed structural validation: {why}",
                        location=MediaLocation(
                            asset=str(probe.path),
                            stream=packet.stream,
                            packet_pos=packet.pos,
                            timestamp=stamp,
                            detail=why,
                        ),
                    ),
                )
            )
            continue
        kept.append(packet)
    return kept, skipped, total


def expected_packet_count(
    probe: AssetProbe,
    index: container.IndexReport,
    packets: list[RawPacket],
) -> int:
    """How many packets the *declared* media should contain.

    Derived per stream from the declared duration and the span that stream's
    packets actually cover, so an 85 %-truncated asset reports ~0.85 recovery
    rather than 1.0.

    Declared durations are paired with streams *one stream to one index track*
    of the same kind, never by a per-kind maximum: an asset with a 10 s main
    programme plus a 2 s commentary track must not predict a 10 s commentary,
    or a perfectly healthy file would score well under 1.0.  This is
    codec-agnostic -- it never assumes one packet per sample (PCM MOV packets
    group many indexed samples) -- and a complete asset yields the observed
    packet count exactly.
    """
    handler_kind = {"vide": "video", "soun": "audio", "text": "subtitle"}
    declared_by_kind: dict[str, list[Fraction]] = {}
    for track in index.tracks:
        seconds_declared = track.declared_seconds
        if seconds_declared and seconds_declared > 0:
            kind = handler_kind.get(track.handler, track.handler)
            declared_by_kind.setdefault(kind, []).append(seconds_declared)

    per_stream: dict[int, list[RawPacket]] = {}
    for packet in packets:
        per_stream.setdefault(packet.stream, []).append(packet)

    # Pair declared durations with streams of the same kind by *rank of length*:
    # the longest stream gets the longest declared duration, and so on.  Pairing
    # by list order is wrong whenever the index writes the short commentary
    # track before the main programme, which would scale the main track by the
    # commentary's duration (or the reverse) and report phantom loss on a
    # healthy asset.
    covered_by_stream: dict[int, Fraction | None] = {}
    kind_of_stream: dict[int, str] = {}
    for info in probe.streams:
        observed = per_stream.get(info.index)
        if not observed:
            continue
        timeline = build_stream_timeline(
            info.index,
            info.type,
            info.time_base_raw,
            [p.to_span() for p in observed],
        )
        covered_by_stream[info.index] = timeline.duration
        kind_of_stream[info.index] = info.type

    declared_for: dict[int, Fraction] = {}
    for kind, pool in declared_by_kind.items():
        streams = sorted(
            (index for index, stream_kind in kind_of_stream.items() if stream_kind == kind),
            key=lambda index: (
                covered_by_stream[index] or Fraction(0),
                -index,
            ),
            reverse=True,
        )
        for index, declared in zip(streams, sorted(pool, reverse=True)):
            declared_for[index] = declared

    expected = 0.0
    for index, observed in per_stream.items():
        covered = covered_by_stream.get(index)
        declared_seconds = declared_for.get(index)
        # An asset whose *declared* span is longer than what its packets cover
        # has lost media.  Never scale up: a healthy stream with an extra
        # container-level packet must stay at 1.0 rather than drive the rate
        # above it.
        if covered and covered > 0 and declared_seconds and declared_seconds > covered:
            expected += len(observed) * float(declared_seconds / covered)
        else:
            expected += len(observed)
    return max(len(packets), round(expected))


def plan_recovery(
    index: container.IndexReport,
    skipped: list[PacketDecision],
    scan: BoundaryScan,
    expected: int,
) -> RecoveryPlan:
    strategies: list[str] = []
    notes: list[str] = []
    issues: list[QCError] = []

    truncated = scan.truncated or any(
        d.reason and "truncation boundary" in d.reason for d in skipped
    )
    incomplete = index.incomplete and not scan.truncated

    if truncated:
        strategies.append("boundary_scan")
        notes.append(
            f"tail truncated: {index.file_size} bytes present, "
            f"mdat declares {index.mdat_declared_end}"
        )
    if skipped:
        strategies.append("skip_corrupt_packets")
        notes.append(f"{len(skipped)} packet(s) skipped")
    if incomplete:
        strategies.append("rebuild_index")
        notes.extend(index.issues[:4])
    if not strategies:
        strategies.append("none")

    for decision in skipped:
        if decision.error is not None:
            issues.append(decision.error)

    if incomplete:
        # Every index defect gets a located issue: the asset, and the byte
        # range of mdat the index fails to cover, so an engineer can see how
        # much media was unreferenced rather than just that "something" was.
        issues.append(
            RecoverableError(
                "incomplete sample index: "
                + "; ".join(index.issues[:3] or ["tables contradict mdat"]),
                location=MediaLocation(
                    asset=None,
                    packet_pos=index.indexed_end(),
                    detail=(
                        f"{index.unindexed_bytes()} bytes of mdat are unreferenced"
                    ),
                ),
                code="incomplete_index",
            )
        )
    if scan.truncated:
        issues.append(
            RecoverableError(
                f"truncated tail: {scan.missing_bytes} indexed bytes are absent",
                location=MediaLocation(
                    asset=None,
                    packet_pos=scan.usable_until,
                    detail="boundary scan kept only fully present samples",
                ),
                code="truncated_tail",
            )
        )

    return RecoveryPlan(
        strategies=strategies,
        truncated=truncated,
        corrupt_packets=len(skipped),
        incomplete_index=incomplete,
        repair_index=incomplete,
        expected_packets=expected,
        notes=notes,
        issues=issues,
    )


def recover_asset(
    probe: AssetProbe,
    *,
    packet_budget: int | None = None,
    deadline: float | None = None,
) -> RecoveryOutcome:
    """Run the full recovery pipeline for one asset (read-only)."""
    _, index = container.scan_file_header(probe.path)
    scan = boundary_scan(index)
    kept, skipped, total = classify_packets(
        probe, index, scan, packet_budget=packet_budget, deadline=deadline
    )
    expected = max(expected_packet_count(probe, index, kept + [d.packet for d in skipped]), len(kept), total)
    plan = plan_recovery(index, skipped, scan, expected)

    per_stream: dict[int, dict[str, int]] = {}
    for packet in kept:
        bucket = per_stream.setdefault(packet.stream, {"kept": 0, "skipped": 0})
        bucket["kept"] += 1
    for decision in skipped:
        bucket = per_stream.setdefault(decision.packet.stream, {"kept": 0, "skipped": 0})
        bucket["skipped"] += 1

    return RecoveryOutcome(
        plan=plan,
        packets=kept,
        skipped=skipped,
        recovered_packets=len(kept),
        total_packets=max(total, len(kept) + len(skipped)),
        expected_packets=expected,
        scan=scan,
        per_stream=per_stream,
    )


def decode_health(path: Path, *, max_packets: int = 400) -> dict[str, object]:
    """Decode a bounded prefix to prove the written asset is readable.

    This is a *verification* decode, never a source of output content: the
    muxed packets are already byte-identical copies.
    """
    frames: dict[int, int] = {}
    errors: list[dict[str, object]] = []
    try:
        with av.open(str(path), mode="r") as container_in:
            stream_map = {s.index: s for s in container_in.streams}
            seen = 0
            for packet in container_in.demux():
                if packet.size == 0:
                    continue
                stream = stream_map.get(packet.stream.index)
                if stream is None:
                    continue
                seen += 1
                try:
                    for _ in stream.codec_context.decode(packet):
                        frames[stream.index] = frames.get(stream.index, 0) + 1
                except av.error.FFmpegError as exc:
                    errors.append(
                        {"stream": stream.index, "pos": packet.pos, "error": str(exc)}
                    )
                if seen >= max_packets:
                    break
            for index, stream in stream_map.items():
                try:
                    for _ in stream.codec_context.decode(None):
                        frames[index] = frames.get(index, 0) + 1
                except av.error.FFmpegError:
                    pass
    except (av.error.FFmpegError, OSError, ValueError) as exc:
        errors.append({"error": str(exc)})
    return {"frames": frames, "errors": errors}
