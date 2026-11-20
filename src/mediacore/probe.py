"""Media scanning: packet timestamps, damage classification, first fault.

For one track the probe records its rational timebase, real start, every
packet's PTS/DTS in unified microseconds, and classifies any damage:

* **truncated** - the file ends early: ``structural_integrity`` reports a
  top-level box running past EOF, or libavformat aborts mid-demux.
* **index gap** - the file is structurally complete but the sample table lost
  entries: fewer packets than ``stts`` declares, or a PTS interval of two or
  more packet durations.
* **index missing** - the sample-to-chunk table is gone; nothing demuxes.
* **corrupt** - a packet's payload is rejected by the decoder.

Localisation: each anomaly reports the *first* offending packet ordinal, its
timestamp and ``locator_error_frames``.  The locator error bounds how far the
reported position could be from the true fault; this implementation locates
by observed evidence, so it is zero.  Outcomes above consume the plain
:class:`~mediacore.probe_model.TrackScan` dataclasses, not PyAV objects.
"""

from __future__ import annotations

from pathlib import Path
from typing import Any, Sequence

from .errors import ProbeError
from .probe_model import DamageReport, PacketInfo, TrackScan
from .probe_container import (
    _av,
    _declared_sample_total,
    _first_corrupt_packet,
    _is_demux_fault,
    _nominal_step,
    _open,
    _pick_stream,
    _short,
    _Unwrapper,
)
from .integrity import structural_integrity
from .timebase import Timebase, Us, round_half_away

__all__ = [
    "PacketInfo",
    "TrackScan",
    "DamageReport",
    "DamageKind",
    "probe_track",
    "scan_tracks",
]

DamageKind = str  # "truncated" | "corrupt" | "index_gap" | "index_missing"

# ---------------------------------------------------------------------------
# scanning
# ---------------------------------------------------------------------------


def probe_track(
    path: str | Path,
    *,
    track_id: str,
    kind: str,
    stream_index: int | None = None,
    timebase: str | None = None,
    origin_us: int = 0,
    wrap_at: int | None = None,
    max_packets: int | None = None,
) -> TrackScan:
    """Scan one stream and record its packets, timing and damage.

    The scan always demuxes the *whole* stream (unless ``max_packets`` is set)
    because truncation is only visible at the tail.  Memory is bounded by
    keeping only :class:`PacketInfo` records (a few dozen bytes each), never
    decoded frames.
    """
    path = str(path)
    if not Path(path).is_file():
        raise ProbeError(
            "input file not found",
            code="probe.missing_file",
            path=path,
            recoverable=False,
        )
    av = _av()
    try:
        container = _open(path)
    except Exception as exc:  # av.error.* are not stable across versions
        raise ProbeError(
            "container cannot be opened",
            code="probe.unreadable",
            path=path,
            reason=_short(exc),
            recoverable=False,
        ) from exc

    scan: TrackScan | None = None
    try:
        stream = _pick_stream(container, kind=kind, stream_index=stream_index)
        tb = Timebase.parse(timebase) if timebase else Timebase.parse(stream.time_base)
        declared_frames = int(stream.frames or 0)
        declared_duration = int(stream.duration or 0)
        duration_us = tb.to_us(declared_duration) if declared_duration else 0
        start_us = tb.to_us(stream.start_time or 0)
        scan = TrackScan(
            track_id=track_id,
            path=path,
            stream_index=stream.index,
            kind=kind,
            timebase=tb,
            start_us=start_us,
            duration_us=duration_us,
            declared_frames=declared_frames,
            declared_samples=_declared_sample_total(path, stream.index),
            sample_rate=stream.codec_context.sample_rate if kind == "audio" else None,
            layout=getattr(stream.codec_context, "layout", None).name
            if kind == "audio" and stream.codec_context.layout
            else None,
            codec=stream.codec_context.name,
        )

        origin_ticks = tb.from_us(origin_us) if origin_us else 0
        ordinal = 0
        step_ticks = _nominal_step(stream, tb)
        demux_error: Exception | None = None
        demux_reason = ""
        unwrap = _Unwrapper(wrap_at)
        try:
            for packet in container.demux(stream):
                if packet.size == 0 and packet.pts is None:
                    continue  # flush marker
                raw_pts = packet.pts
                raw_dts = packet.dts if packet.dts is not None else raw_pts
                if raw_pts is None and raw_dts is None:
                    continue
                base = raw_pts if raw_pts is not None else raw_dts
                pts_ticks = unwrap.feed(base)
                dts_ticks = unwrap.feed(raw_dts) if raw_dts is not None else pts_ticks
                rel_pts = pts_ticks - origin_ticks
                exact = tb.to_us_fraction(rel_pts)
                stored = round_half_away(exact)
                scan.drift.record(exact, stored, at=pts_ticks)
                scan.packets.append(
                    PacketInfo(
                        ordinal=ordinal,
                        pts_ticks=pts_ticks,
                        dts_ticks=dts_ticks,
                        pts_us=stored,
                        dts_us=round_half_away(tb.to_us_fraction(dts_ticks - origin_ticks)),
                        size=packet.size,
                        keyframe=bool(packet.is_keyframe),
                        pos=int(packet.pos) if packet.pos is not None else None,
                    )
                )
                ordinal += 1
                if max_packets is not None and ordinal >= max_packets:
                    break
        except Exception as exc:
            # A demux failure at the tail is expected for truncated media;
            # anything else is a programming error and must not be silently
            # converted into a "truncated" diagnosis.
            if not _is_demux_fault(exc):
                raise
            demux_error = exc
            demux_reason = _short(exc)

        _locate_faults(
            scan,
            stream=stream,
            step_ticks=step_ticks,
            demux_error=demux_error,
            demux_reason=demux_reason,
        )
    finally:
        container.close()
    assert scan is not None
    return scan

def _locate_faults(
    scan: TrackScan,
    *,
    stream,
    step_ticks: int | None,
    demux_error: Exception | None,
    demux_reason: str = "",
) -> None:
    """Populate ``scan.damage`` in priority order (gap, truncation, corrupt)."""
    declared = scan.expected_packets
    demuxed = scan.packet_count

    # --- index gaps vs truncation ---------------------------------------
    # Both look like "fewer packets than the sample table promises".  The
    # decisive question is whether the *file* ends early: a truncation leaves
    # the last top-level box extending past EOF (structural_integrity says so
    # without trusting libavformat), while an interior sample-table hole
    # leaves a structurally complete file whose packets demux out short.
    shortfall = declared > demuxed
    incomplete_file = not structural_integrity(scan.path).get("complete", True)

    first_gap = None
    # A wholly demuxable-nothing file is "index missing", not a gap: reporting
    # both would give recovery two contradictory repair strategies.
    nothing_demuxed = demuxed == 0 and declared > 0
    if shortfall and not nothing_demuxed and demux_error is None and not incomplete_file:
        ordinal, pts_us, missing = _first_missing_index(scan, declared)
        first_gap = (ordinal, pts_us, missing)
    if first_gap is None and not nothing_demuxed and demux_error is None:
        # A PTS discontinuity in an otherwise complete file is also an index
        # gap, even when the sample count happens to match.
        first_gap = _find_first_gap(scan)
    if first_gap is not None:
        ordinal, pts_us, missing = first_gap
        scan.damage.append(
            ProbeError(
                "index gap: sample table lost an entry",
                code="probe.index_gap",
                track=scan.track_id,
                packet_ordinal=ordinal,
                pts_us=pts_us,
                missing_packets=int(missing),
                declared_samples=declared,
                demuxed_samples=demuxed,
                locator_error_frames=0,
                locator_error_us=0,
                strategy="scan_continue",
            )
        )

    # --- truncation: the file itself ends early -------------------------
    truncated = (
        demux_error is not None
        or incomplete_file
        or (shortfall and first_gap is None and not nothing_demuxed)
    )
    if truncated:
        ordinal, pts_us, missing = _first_missing_index(scan, declared)
        scan.damage.append(
            ProbeError(
                "truncated tail: declared media ends during demux",
                code="probe.truncated",
                track=scan.track_id,
                packet_ordinal=ordinal,
                pts_us=pts_us,
                missing_packets=missing if missing is not None else max(declared - demuxed, 1),
                declared_frames=declared,
                demuxed_frames=demuxed,
                locator_error_frames=0,
                locator_error_us=0,
                reason=demux_reason or "declared count not reached",
                strategy="trim_to_last_good_packet",
            )
        )

    # --- corruption: decode each packet until the first failure -------------
    # Only meaningful for wholly-present media: when the tail is truncated,
    # the index is missing, or a sample-table hole was already located, a
    # decoder error is a *symptom* of that damage rather than an independent
    # corrupt packet.
    if not truncated and not (demuxed == 0 and declared > 0) and first_gap is None:
        corrupt = _first_corrupt_packet(
            scan.path, stream.index, _corruption_scan_limit(scan)
        )
        if corrupt is not None:
            ordinal, pts_us = corrupt
            scan.damage.append(
                ProbeError(
                    "corrupt packet: decoder rejected payload",
                    code="probe.corrupt",
                    track=scan.track_id,
                    packet_ordinal=ordinal,
                    pts_us=pts_us,
                    locator_error_frames=0,
                    locator_error_us=0,
                    strategy="drop_packet_continue",
                )
            )

    # A file whose sample table is missing entirely demuxes nothing at all.
    if demuxed == 0 and declared > 0:
        scan.damage.append(
            ProbeError(
                "index missing: no packet offsets available",
                code="probe.index_missing",
                track=scan.track_id,
                pts_us=scan.start_us,
                packet_ordinal=0,
                declared_frames=declared,
                locator_error_frames=0,
                locator_error_us=0,
                strategy="fail_isolated",
            )
        )

def _find_first_gap(scan: TrackScan) -> tuple[int, Us, int] | None:
    """Locate a presentation jump: a gap in the sample-time table.

    Each packet's PTS is derived from the sample-duration table, so a
    genuinely missing sample shows up as an interval of two or more packet
    durations.  That is the one frame-accurate signal, and it is reported with
    a locator error of zero.  Byte-position heuristics are deliberately *not*
    used here: interleaved tracks share ``mdat``, and payload-size variation
    makes "hole between my packets" indistinguishable from normal
    interleaving.
    """
    packets = scan.packets
    if len(packets) < 4:
        return None

    spacings = sorted(
        b.pts_us - a.pts_us
        for a, b in zip(packets, packets[1:])
        if b.pts_us > a.pts_us
    )
    if not spacings:
        return None
    nominal = spacings[len(spacings) // 2]
    if nominal <= 0:
        return None
    for previous, current in zip(packets, packets[1:]):
        delta = current.pts_us - previous.pts_us
        if delta > nominal + nominal // 2:
            missing = max(1, round(delta / nominal) - 1)
            return previous.ordinal + 1, previous.pts_us + nominal, missing
    return None

def _corruption_scan_limit(scan: TrackScan) -> int:
    """Upper bound on packets to decode when looking for corruption.

    Decoding is far more expensive than demuxing, so a 10-minute, 30 fps file
    (18 000 packets) is not decoded in full during a probe.  The limit is the
    smaller of the packet count and a fixed budget, so demo-sized media are
    scanned exhaustively while long files stay bounded.  Corruption later than
    the budget is caught when that packet is actually rendered.
    """
    return min(scan.packet_count, 4000) or 4000

def _first_missing_index(scan: TrackScan, declared: int) -> tuple[int, Us, int | None]:
    """Locate the first absent packet: ordinal, expected PTS, missing count.

    The first packet that did *not* demux has ordinal ``demuxed`` (one past
    the last good one), and its expected PTS is one interval after the last
    good packet's.  Reporting the last *present* packet instead would point
    the operator one frame behind the actual fault.
    """
    if not scan.packets:
        return 0, scan.start_us, declared or None
    last = scan.packets[-1]
    step = scan.interval_us()
    missing = max(declared - scan.packet_count, 0)
    return last.ordinal + 1, last.pts_us + step, missing

def scan_tracks(tracks: Sequence[Any], *, max_packets: int | None = None) -> DamageReport:
    """Probe every :class:`~mediacore.timeline.TrackSpec` in order."""
    report = DamageReport()
    for track in tracks:
        scan = probe_track(
            track.path,
            track_id=track.id,
            kind=track.kind,
            stream_index=track.stream_index,
            timebase=track.timebase,
            origin_us=track.origin_us,
            wrap_at=track.wrap_at,
            max_packets=max_packets,
        )
        report.add(scan)
    return report

# ---------------------------------------------------------------------------
# structural integrity (container box walk, independent of libavformat)
# ---------------------------------------------------------------------------

