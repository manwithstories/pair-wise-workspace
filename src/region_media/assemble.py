"""Capture scanning, timeline reordering and block assembly.

The pipeline must run in memory proportional to the block size, never the
stream length, so this module is built around two streaming primitives:

``scan_capture``
    walks the capture once, applying the shared RTP/RTCP clock model and the
    sequence tracker, and hands each accepted packet to a callback.  With
    ``store_packets=True`` it also keeps the events (used by tests and by the
    diagnostics report); the pipeline itself always scans streaming.

``BlockAssembler``
    receives packets in arrival order through a small reorder window, resolves
    overlaps, fills timeline gaps with silence and emits fixed-size source-rate
    blocks.  Only the current block plus the reorder window are resident.

``iter_source_blocks`` composes the two.
"""

from __future__ import annotations

import hashlib
from collections import deque
from dataclasses import dataclass, field
from pathlib import Path
from typing import Iterator

import numpy as np

from . import packet
from .clock import RtcpMapping, Timeline, parse_rtcp_sender_reports
from .errors import Code, Diagnostic, InputError

DEFAULT_BLOCK_MS = 1000
DEFAULT_REORDER_WINDOW = 64


@dataclass
class PacketEvent:
    """One accepted RTP packet with its position on the media timeline."""

    index: int
    sequence: int
    sequence_unwrapped: int
    timestamp: int
    timestamp_unwrapped: int
    wall_seconds: float | None
    media_pos: int  # source samples relative to the first packet
    samples: np.ndarray  # (n, channels) float32
    marker: bool = False
    status: tuple[Code, ...] = ()
    truncated: bool = False

    @property
    def length(self) -> int:
        return int(self.samples.shape[0])

    @property
    def seq_start(self) -> int:
        return self.sequence

    @property
    def seq_end(self) -> int:
        return self.sequence


@dataclass
class CaptureAnalysis:
    """Streaming scan result: counts, metadata and diagnostics (no bulk PCM)."""

    directory: Path
    spec: packet.StreamSpec
    rtcp: RtcpMapping
    diagnostics: list[Diagnostic]
    lost_sequences: list[int]
    source_samples: int
    fingerprint: str
    packets: list[PacketEvent] = field(default_factory=list)
    videos: list[packet.VideoRecord] = field(default_factory=list)
    video_end_samples: int = 0
    truncated_packets: int = 0
    duplicate_packets: int = 0
    reordered_packets: int = 0
    malformed_packets: int = 0
    sequence_wraps: int = 0
    timestamp_wraps: int = 0
    nonfinite_samples: int = 0
    record_errors: int = 0

    @property
    def lost_packets(self) -> int:
        return len(self.lost_sequences)

    def summary(self) -> dict[str, object]:
        seconds = (
            self.source_samples / self.spec.sample_rate
            if self.spec.sample_rate
            else 0.0
        )
        return {
            "fingerprint": self.fingerprint,
            "packets": len(self.packets) if self.packets else self.accepted_packets,
            "source_samples": self.source_samples,
            "source_seconds": round(seconds, 6),
            "source_rate": self.spec.sample_rate,
            "source_layout": self.spec.layout,
            "lost_packets": self.lost_packets,
            "duplicate_packets": self.duplicate_packets,
            "reordered_packets": self.reordered_packets,
            "truncated_packets": self.truncated_packets,
            "malformed_packets": self.malformed_packets,
            "nonfinite_samples": self.nonfinite_samples,
            "record_errors": self.record_errors,
            "sequence_wraps": self.sequence_wraps,
            "timestamp_wraps": self.timestamp_wraps,
            "video_records": len(self.videos),
            "rtcp": self.rtcp.summary(),
        }

    accepted_packets: int = 0


def _fingerprint(directory: Path, files: list[Path]) -> str:
    digest = hashlib.sha256()
    for path in sorted(files):
        try:
            digest.update(path.name.encode("utf-8"))
            digest.update(path.read_bytes())
        except OSError:
            digest.update(f"{path.name}:unreadable".encode("utf-8"))
    return digest.hexdigest()


def _load_spec(directory: Path) -> tuple[packet.StreamSpec, Path, Path]:
    if not directory.is_dir():
        raise InputError(
            Code.MALFORMED_HEADER,
            f"input directory not found: {directory}",
            where=str(directory),
        )
    spec_path = directory / "stream.json"
    capture_path = directory / "capture.edg"
    if not spec_path.is_file() or not capture_path.is_file():
        raise InputError(
            Code.MALFORMED_HEADER,
            "capture directory must contain stream.json and capture.edg",
            where=str(directory),
            detail={
                "has_spec": spec_path.is_file(),
                "has_capture": capture_path.is_file(),
            },
        )
    try:
        spec = packet.StreamSpec.load(spec_path)
    except packet.CaptureFormatError as exc:
        raise InputError(
            Code.MALFORMED_HEADER, str(exc), where=str(spec_path)
        ) from exc
    if spec.sample_rate <= 0 or spec.clock_rate <= 0 or spec.channels <= 0:
        raise InputError(
            Code.MALFORMED_HEADER,
            "stream.json declares non-positive rate or channel count",
            where=str(spec_path),
        )
    return spec, spec_path, capture_path


def _scan_events(
    spec: packet.StreamSpec,
    capture_path: Path,
    *,
    reorder_window: int,
    state: dict[str, object],
    diagnostics: list[Diagnostic],
) -> Iterator[PacketEvent]:
    """Yield accepted packet events; scan counters land in ``state``."""
    tracker: packet.SequenceTracker = state["tracker"]  # type: ignore[assignment]
    timeline = Timeline(spec.clock_rate)
    rtcp: RtcpMapping = state["rtcp"]  # type: ignore[assignment]
    videos = state["videos"]  # type: ignore[assignment]
    audio_rate = spec.sample_rate
    counters = state["counters"]  # type: ignore[assignment]
    max_pos = 0

    for record, rec_diags in packet.read_capture(capture_path):
        if rec_diags:
            counters["record_errors"] += len(rec_diags)
            diagnostics.extend(rec_diags)
        if record is None:
            continue
        if record.kind == packet.KIND_RTCP:
            where = f"{capture_path}:{record.offset}"
            reports, rtcp_diags = parse_rtcp_sender_reports(
                record.payload, where=where
            )
            diagnostics.extend(rtcp_diags)
            for report in reports:
                rtcp.update(report, where=where)
            continue
        if record.kind == packet.KIND_VIDEO:
            video = packet.decode_video_record(record.payload, record.index)
            if video.malformed:
                diagnostics.append(
                    Diagnostic(
                        Code.METADATA_MISSING,
                        "video metadata record could not be decoded",
                        f"{capture_path}:{record.offset}",
                    )
                )
            videos.append(video)
            state["video_end"] = max(
                int(state["video_end"]),  # type: ignore[arg-type]
                video.pts_samples + video.duration_samples,
            )
            continue
        where = f"{capture_path}:{record.offset}"
        try:
            header = packet.parse_rtp_header(record.payload)
        except ValueError as exc:
            counters["malformed"] += 1
            diagnostics.append(Diagnostic(Code.MALFORMED_HEADER, str(exc), where))
            continue
        accepted, was_reordered, unwrapped = tracker.observe(header.sequence)
        if (
            tracker.last_wrap_unwrapped is not None
            and tracker.last_wrap_unwrapped == unwrapped
        ):
            counters["seq_wraps"] += 1
            diagnostics.append(
                Diagnostic(
                    Code.SEQUENCE_WRAP,
                    "RTP sequence number wrapped at 16 bits",
                    where,
                    {"sequence": header.sequence},
                )
            )
            tracker.last_wrap_unwrapped = None
        if not accepted:
            counters["duplicate"] += 1
            diagnostics.append(
                Diagnostic(
                    Code.DUPLICATE_PACKET,
                    "duplicate RTP sequence",
                    where,
                    {"sequence": header.sequence},
                )
            )
            continue
        status: list[Code] = []
        if was_reordered:
            counters["reordered"] += 1
            status.append(Code.REORDERED_PACKET)
            diagnostics.append(
                Diagnostic(
                    Code.REORDERED_PACKET,
                    "packet arrived after a later sequence",
                    where,
                    {"sequence": header.sequence},
                )
            )
        body = record.payload[header.header_bytes :]
        if header.padding_bytes:
            body = body[: -header.padding_bytes]
        try:
            payload = packet.decode_audio_payload(body, spec.channels)
        except ValueError as exc:
            counters["malformed"] += 1
            diagnostics.append(Diagnostic(Code.MALFORMED_HEADER, str(exc), where))
            continue
        flagged_truncated = bool(record.flags & 0x01)
        if payload.truncated or flagged_truncated or payload.samples.shape[0] == 0:
            counters["truncated"] += 1
            status.append(Code.TRUNCATED_PACKET)
            diagnostics.append(
                Diagnostic(
                    Code.TRUNCATED_PACKET,
                    "packet carries fewer samples than declared",
                    where,
                    {
                        "sequence": header.sequence,
                        "declared": payload.declared_samples,
                        "actual": int(payload.samples.shape[0]),
                    },
                )
            )
        if payload.samples.size and not np.isfinite(payload.samples).all():
            bad = int((~np.isfinite(payload.samples)).sum())
            counters["nonfinite"] += bad
            diagnostics.append(
                Diagnostic(
                    Code.NONFINITE_SAMPLE,
                    "packet contains NaN/Inf samples",
                    where,
                    {"samples": bad, "sequence": header.sequence},
                )
            )
            payload.samples = np.nan_to_num(
                payload.samples, nan=0.0, posinf=0.0, neginf=0.0
            )
        event_ts = timeline.push(header.timestamp, where=where)
        if event_ts.wrap:
            counters["ts_wraps"] += 1
        pos = int(round(event_ts.sample_offset * audio_rate / spec.clock_rate))
        wall = rtcp.wallclock_at(header.timestamp) if rtcp.available else None
        event = PacketEvent(
            index=counters["accepted"],
            sequence=header.sequence,
            sequence_unwrapped=unwrapped,
            timestamp=header.timestamp,
            timestamp_unwrapped=event_ts.unwrapped,
            wall_seconds=wall,
            media_pos=pos,
            samples=payload.samples,
            marker=header.marker,
            status=tuple(status),
            truncated=payload.truncated or flagged_truncated,
        )
        counters["accepted"] += 1
        max_pos = max(max_pos, pos + event.length)
        state["max_pos"] = max_pos
        yield event
    state["timeline_diagnostics"] = timeline.diagnostics


def _finalise_scan(
    state: dict[str, object],
    diagnostics: list[Diagnostic],
    capture_path: Path,
    *,
    stopped: bool,
) -> list[int]:
    counters = state["counters"]  # type: ignore[assignment]
    tracker: packet.SequenceTracker = state["tracker"]  # type: ignore[assignment]
    rtcp: RtcpMapping = state["rtcp"]  # type: ignore[assignment]
    diagnostics.extend(state["timeline_diagnostics"])  # type: ignore[arg-type]
    diagnostics.extend(rtcp.diagnostics)
    if not rtcp.available and counters["accepted"] > 0:
        diagnostics.append(
            Diagnostic(
                Code.RTCP_MISSING,
                "no RTCP sender report; using RTP timestamps only",
                str(capture_path),
            )
        )
    lost: list[int] = []
    if stopped:
        return lost
    lost = tracker.flush_lost()
    if lost:
        diagnostics.append(
            Diagnostic(
                Code.LOST_PACKET,
                "RTP sequence gap",
                str(capture_path),
                {"count": len(lost), "sequences": lost[:32]},
            )
        )
    if counters["accepted"] == 0:
        diagnostics.append(
            Diagnostic(
                Code.EMPTY_STREAM,
                "capture contains no RTP packets",
                str(capture_path),
            )
        )
    return lost


def _build_analysis(
    directory: Path,
    spec: packet.StreamSpec,
    spec_path: Path,
    capture_path: Path,
    state: dict[str, object],
    diagnostics: list[Diagnostic],
    lost: list[int],
) -> CaptureAnalysis:
    counters = state["counters"]  # type: ignore[assignment]
    analysis = CaptureAnalysis(
        directory=directory,
        spec=spec,
        rtcp=state["rtcp"],  # type: ignore[arg-type]
        diagnostics=diagnostics,
        lost_sequences=lost,
        source_samples=int(state["max_pos"]),  # type: ignore[arg-type]
        fingerprint=_fingerprint(directory, [spec_path, capture_path]),
        video_end_samples=int(state["video_end"]),  # type: ignore[arg-type]
        truncated_packets=counters["truncated"],
        duplicate_packets=counters["duplicate"],
        reordered_packets=counters["reordered"],
        malformed_packets=counters["malformed"],
        sequence_wraps=counters["seq_wraps"],
        timestamp_wraps=counters["ts_wraps"],
        nonfinite_samples=counters["nonfinite"],
        record_errors=counters["record_errors"],
    )
    analysis.accepted_packets = counters["accepted"]
    return analysis


def _new_state(spec: packet.StreamSpec) -> dict[str, object]:
    return {
        "rtcp": RtcpMapping(clock_rate=spec.clock_rate),
        "videos": [],
        "video_end": 0,
        "max_pos": 0,
        "timeline_diagnostics": [],
        "counters": {
            "truncated": 0,
            "duplicate": 0,
            "reordered": 0,
            "malformed": 0,
            "seq_wraps": 0,
            "ts_wraps": 0,
            "nonfinite": 0,
            "record_errors": 0,
            "accepted": 0,
        },
    }


def scan_capture(
    directory: str | Path,
    *,
    reorder_window: int = DEFAULT_REORDER_WINDOW,
    store_packets: bool = False,
) -> CaptureAnalysis:
    """Scan a capture once, applying clock, sequence and payload checks."""
    directory = Path(directory)
    spec, spec_path, capture_path = _load_spec(directory)
    diagnostics: list[Diagnostic] = []
    state = _new_state(spec)
    tracker = packet.SequenceTracker(window=reorder_window)
    state["tracker"] = tracker
    packets: list[PacketEvent] = []
    try:
        for event in _scan_events(
            spec,
            capture_path,
            reorder_window=reorder_window,
            state=state,
            diagnostics=diagnostics,
        ):
            if store_packets:
                packets.append(event)
    except packet.CaptureFormatError as exc:
        raise InputError(
            Code.MALFORMED_HEADER, str(exc), where=str(capture_path)
        ) from exc
    lost = _finalise_scan(state, diagnostics, capture_path, stopped=False)
    analysis = _build_analysis(
        directory, spec, spec_path, capture_path, state, diagnostics, lost
    )
    analysis.packets = packets
    analysis.videos = state["videos"]  # type: ignore[assignment]
    return analysis


def analyse_capture(
    directory: str | Path, *, reorder_window: int = DEFAULT_REORDER_WINDOW
) -> CaptureAnalysis:
    """Convenience wrapper that keeps packet events (tests, diagnostics)."""
    return scan_capture(
        directory, reorder_window=reorder_window, store_packets=True
    )


@dataclass
class AssemblyStats:
    overlap_samples: int = 0
    overlap_packets: int = 0
    gap_samples: int = 0
    gap_events: int = 0
    dropped_packets: int = 0
    total_samples: int = 0


@dataclass
class SourceBlock:
    """A contiguous block of assembled audio at the source sample rate."""

    start: int
    samples: np.ndarray
    packets: tuple[int, ...] = field(default_factory=tuple)
    sequences: tuple[int, ...] = field(default_factory=tuple)
    timestamps: tuple[int, ...] = field(default_factory=tuple)
    timestamp_wrapped: bool = False
    wall_start: float | None = None


class Reorderer:
    """Release packets in media order from a bounded position window.

    A packet is released only once the highest media position seen is more
    than ``window_samples`` ahead of it: by then anything earlier that is
    still missing can be treated as lost rather than as reordering.  Releasing
    strictly in ``(media_pos, arrival)`` order is what keeps the assembler from
    trimming a later packet as an "overlap" of an earlier one that merely
    arrived first.  Memory is bounded by the window, never the stream.
    """

    def __init__(self, window_samples: int = 48000) -> None:
        if window_samples < 0:
            raise ValueError("window_samples must not be negative")
        self.window_samples = window_samples
        self._buffer: list[PacketEvent] = []
        self._max_pos: int | None = None
        self.max_pos: int | None = None

    def push(self, event: PacketEvent) -> list[PacketEvent]:
        self._buffer.append(event)
        if self._max_pos is None or event.media_pos > self._max_pos:
            self._max_pos = event.media_pos
        self.max_pos = self._max_pos
        cutoff = self._max_pos - self.window_samples
        ready = [e for e in self._buffer if e.media_pos <= cutoff]
        if not ready:
            return []
        ready.sort(key=lambda e: (e.media_pos, e.index))
        release_ids = {id(e) for e in ready}
        self._buffer = [e for e in self._buffer if id(e) not in release_ids]
        return ready

    def flush(self) -> list[PacketEvent]:
        ready = sorted(self._buffer, key=lambda e: (e.media_pos, e.index))
        self._buffer = []
        return ready


class BlockAssembler:
    """Turn ordered packet events into fixed-size source blocks."""

    def __init__(self, block_samples: int, channels: int) -> None:
        if block_samples <= 0:
            raise ValueError("block_samples must be positive")
        self.block_samples = block_samples
        self.channels = channels
        self.buffer = np.zeros((block_samples, channels), dtype=np.float32)
        self.filled = 0
        self.cursor = 0
        self.out_cursor = 0
        self.stats = AssemblyStats()
        self._block_sequences: list[int] = []
        self._block_timestamps: list[int] = []
        self._block_wrapped = False
        self._block_first_seq: int | None = None
        self._block_last_seq: int | None = None
        self._block_wall: float | None = None
        self._started = False

    def _flush(self) -> SourceBlock:
        block = SourceBlock(
            start=self.out_cursor,
            samples=self.buffer[: self.filled].copy(),
            sequences=tuple(self._block_sequences),
            timestamps=tuple(self._block_timestamps),
            timestamp_wrapped=self._block_wrapped,
            packets=tuple(range(self._block_first_seq or 0, (self._block_last_seq or 0) + 1))
            if self._block_first_seq is not None
            else (),
            wall_start=self._block_wall,
        )
        self.out_cursor += self.filled
        self.stats.total_samples = self.out_cursor
        self.filled = 0
        self._block_sequences = []
        self._block_timestamps = []
        self._block_wrapped = False
        self._block_first_seq = None
        self._block_last_seq = None
        self._block_wall = None
        return block

    def _write_silence(self, count: int, out: list[SourceBlock]) -> None:
        while count > 0:
            take = min(self.block_samples - self.filled, count)
            self.buffer[self.filled : self.filled + take] = 0.0
            self.filled += take
            self.cursor += take
            count -= take
            if self.filled == self.block_samples:
                out.append(self._flush())

    def _write_samples(
        self, samples: np.ndarray, event: PacketEvent, out: list[SourceBlock]
    ) -> None:
        offset = 0
        total = samples.shape[0]
        while offset < total:
            take = min(self.block_samples - self.filled, total - offset)
            self.buffer[self.filled : self.filled + take] = samples[
                offset : offset + take
            ]
            if self._block_wall is None:
                self._block_wall = event.wall_seconds
            if self._block_first_seq is None:
                self._block_first_seq = event.sequence
            self._block_last_seq = event.sequence
            self._block_sequences.append(event.sequence)
            self._block_timestamps.append(event.timestamp_unwrapped)
            if event.timestamp_unwrapped != event.timestamp:
                self._block_wrapped = True
            self.filled += take
            self.cursor += take
            offset += take
            if self.filled == self.block_samples:
                out.append(self._flush())

    def push(self, event: PacketEvent) -> list[SourceBlock]:
        out: list[SourceBlock] = []
        length = event.length
        if length == 0:
            return out
        if not self._started:
            self._started = True
            if event.media_pos > 0:
                self._write_silence(event.media_pos, out)
        if event.media_pos < self.cursor:
            overlap = self.cursor - event.media_pos
            if overlap >= length:
                self.stats.overlap_packets += 1
                self.stats.dropped_packets += 1
                return out
            self.stats.overlap_packets += 1
            self.stats.overlap_samples += overlap
            event = PacketEvent(
                index=event.index,
                sequence=event.sequence,
                sequence_unwrapped=event.sequence_unwrapped,
                timestamp=event.timestamp,
                timestamp_unwrapped=event.timestamp_unwrapped,
                wall_seconds=event.wall_seconds,
                media_pos=self.cursor,
                samples=event.samples[overlap:],
                marker=event.marker,
                status=event.status,
                truncated=event.truncated,
            )
            length = event.length
        elif event.media_pos > self.cursor:
            gap = event.media_pos - self.cursor
            self.stats.gap_samples += gap
            self.stats.gap_events += 1
            self._write_silence(gap, out)
        self._write_samples(event.samples, event, out)
        return out

    def flush(self) -> list[SourceBlock]:
        if self.filled:
            return [self._flush()]
        return []


def iter_source_blocks(
    directory: str | Path,
    *,
    block_samples: int,
    reorder_window: int = DEFAULT_REORDER_WINDOW,
    stats: AssemblyStats | None = None,
    analysis: CaptureAnalysis | None = None,
) -> Iterator[SourceBlock]:
    """Stream assembled source blocks from a capture directory.

    Only the assembler's current block and the reorder window are resident, so
    memory grows with the block size, not the capture length.  ``analysis``
    (when given) is filled with the scan result.  Diagnostics are attached to
    ``analysis.diagnostics`` because the iterator itself must stay a pure media
    stream.
    """
    directory_path = Path(directory)
    spec, spec_path, capture_path = _load_spec(directory_path)
    diagnostics: list[Diagnostic] = []
    state = _new_state(spec)
    state["tracker"] = packet.SequenceTracker(window=reorder_window)
    reorder = Reorderer(
        window_samples=max(block_samples, spec.sample_rate // 2)
    )
    assembler = BlockAssembler(block_samples, spec.channels)
    try:
        for event in _scan_events(
            spec,
            capture_path,
            reorder_window=reorder_window,
            state=state,
            diagnostics=diagnostics,
        ):
            for released in reorder.push(event):
                yield from assembler.push(released)
        for released in reorder.flush():
            yield from assembler.push(released)
        yield from assembler.flush()
    except packet.CaptureFormatError as exc:
        raise InputError(
            Code.MALFORMED_HEADER, str(exc), where=str(capture_path)
        ) from exc
    lost = _finalise_scan(state, diagnostics, capture_path, stopped=False)
    if analysis is not None:
        built = _build_analysis(
            directory_path, spec, spec_path, capture_path, state, diagnostics, lost
        )
        built.videos = state["videos"]  # type: ignore[assignment]
        _copy_analysis(built, analysis)
    if stats is not None:
        stats.overlap_samples = assembler.stats.overlap_samples
        stats.overlap_packets = assembler.stats.overlap_packets
        stats.gap_samples = assembler.stats.gap_samples
        stats.gap_events = assembler.stats.gap_events
        stats.dropped_packets = assembler.stats.dropped_packets
        stats.total_samples = assembler.stats.total_samples


def _copy_analysis(source: CaptureAnalysis, target: CaptureAnalysis) -> None:
    for name in (
        "spec",
        "rtcp",
        "videos",
        "lost_sequences",
        "source_samples",
        "fingerprint",
        "video_end_samples",
        "truncated_packets",
        "duplicate_packets",
        "reordered_packets",
        "malformed_packets",
        "sequence_wraps",
        "timestamp_wraps",
        "nonfinite_samples",
        "record_errors",
    ):
        setattr(target, name, getattr(source, name))
    target.diagnostics = list(source.diagnostics)
    target.accepted_packets = source.accepted_packets


def assemble_all(
    packets: list[PacketEvent], *, block_samples: int = 48000
) -> tuple[np.ndarray, AssemblyStats]:
    """Concatenate all blocks from an in-memory packet list (tests)."""
    if not packets:
        return np.zeros((0, 1), dtype=np.float32), AssemblyStats()
    channels = packets[0].samples.shape[1]
    ordered = sorted(packets, key=lambda p: (p.media_pos, p.index))
    assembler = BlockAssembler(block_samples, channels)
    blocks: list[SourceBlock] = []
    for event in ordered:
        blocks.extend(assembler.push(event))
    blocks.extend(assembler.flush())
    if not blocks:
        return np.zeros((0, channels), dtype=np.float32), assembler.stats
    return np.concatenate([b.samples for b in blocks], axis=0), assembler.stats


def packet_status_counts(packets: list[PacketEvent]) -> dict[str, int]:
    counts: dict[str, int] = {}
    for event in packets:
        for code in event.status:
            counts[code.value] = counts.get(code.value, 0) + 1
    return counts


def video_interval(analysis: CaptureAnalysis) -> dict[str, float] | None:
    if not analysis.videos:
        return None
    rate = analysis.spec.clock_rate
    start = min(v.pts_samples for v in analysis.videos) / rate
    end = max(v.pts_samples + v.duration_samples for v in analysis.videos) / rate
    return {"start_sec": start, "end_sec": end}


__all__ = [
    "AssemblyStats",
    "BlockAssembler",
    "CaptureAnalysis",
    "PacketEvent",
    "Reorderer",
    "SourceBlock",
    "analyse_capture",
    "assemble_all",
    "iter_source_blocks",
    "packet_status_counts",
    "scan_capture",
    "video_interval",
]
