"""Timestamp diagnosis and a single repair plan shared by report and mux."""

from __future__ import annotations

import os
import tempfile
from collections import Counter, deque
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path
from statistics import median
from typing import Optional

from .models import FrameRecord, MediaInfo, PacketMeta, RepairEntry, seconds_to_ms


@dataclass(slots=True)
class _Draft:
    field: str
    reason: str
    packet_seq: Optional[int]
    frame_index: Optional[int]
    before_ms: Optional[float]
    delta_ms: Optional[float]


@dataclass
class RepairPlan:
    info: MediaInfo
    frame_duration: Fraction
    target_time_base: Fraction
    packet_pts: dict[int, int] = field(default_factory=dict)
    packet_dts: dict[int, int] = field(default_factory=dict)
    entries: list[RepairEntry] = field(default_factory=list)
    reasons: Counter[str] = field(default_factory=Counter)
    max_jump_ms: float = 0.0
    decoded_frames: int = 0
    packet_count: int = 0
    unresolved_frames: int = 0
    pts_lag_frames: int = 0

    @property
    def fixed_entries(self) -> int:
        return len(self.entries)

    @property
    def unrecoverable(self) -> bool:
        return self.decoded_frames == 0 or self.unresolved_frames > 0

    def entry_dicts(self) -> list[dict[str, object]]:
        return [entry.as_dict() for entry in self.entries]


class TimestampAnalyzer:
    """Streams frame and packet metadata into one timestamp repair plan."""

    def __init__(
        self,
        info: MediaInfo,
        max_jump_ms: float = 250.0,
        *,
        capture_mapping: bool = False,
    ) -> None:
        self.info = info
        self.max_jump_ms = float(max_jump_ms)
        self.capture_mapping = capture_mapping
        self._drafts: list[_Draft] = []
        self._last_frame_pts: Optional[float] = None
        self._last_packet_dts: Optional[int] = None
        self._packet_seq = 0
        self._frame_count = 0
        self._first_frame = True
        self._pt_deltas: deque[float] = deque(maxlen=101)
        self._duration_hist: Counter[int] = Counter()
        self._packet_display_index: dict[int, int] = {}
        self.max_jump_ms = 0.0
        self._configured_jump_ms = float(max_jump_ms)

        if info.average_rate and info.average_rate > 0:
            self.frame_duration = Fraction(1, 1) / info.average_rate
        else:
            self.frame_duration = Fraction(1, 25)

    def add_frame(self, record: FrameRecord) -> None:
        self._frame_count += 1
        if record.packet_seq is not None:
            self._packet_display_index[record.packet_seq] = record.index
        pts_ms = record.pts_ms
        if pts_ms is not None:
            if self._last_frame_pts is not None:
                delta = pts_ms - self._last_frame_pts
                if 0 < delta <= 1000:
                    self._pt_deltas.append(delta)
                self.max_jump_ms = max(self.max_jump_ms, abs(delta))
                if delta < 0:
                    self._draft("pts", "pts_rollback", record, pts_ms, delta)
                if delta == 0:
                    self._draft("pts", "pts_duplicate", record, pts_ms, delta)
                if abs(delta) > self._configured_jump_ms:
                    self._draft("pts", "jump", record, pts_ms, delta)
            self._last_frame_pts = pts_ms

    def add_packet(self, packet: PacketMeta) -> None:
        self._packet_seq = max(self._packet_seq, packet.seq + 1)
        if packet.duration is not None and packet.duration > 0:
            self._duration_hist[int(packet.duration)] += 1
        dts = packet.dts
        if dts is None:
            self._draft("dts", "missing_dts", packet, None, None)
        elif self._last_packet_dts is None:
            if dts != 0:
                reason = "negative_initial_dts" if dts < 0 else "first_dts_nonzero"
                self._draft("dts", reason, packet, packet.dts_ms, None)
        else:
            delta_ticks = dts - self._last_packet_dts
            delta_ms = float(Fraction(delta_ticks) * packet.time_base) * 1000.0
            self.max_jump_ms = max(self.max_jump_ms, abs(delta_ms))
            if delta_ticks == 0:
                self._draft("dts", "dts_duplicate", packet, packet.dts_ms, 0.0)
            elif delta_ticks < 0:
                self._draft("dts", "negative_interval", packet, packet.dts_ms, delta_ms)
            if abs(delta_ms) > self._configured_jump_ms:
                self._draft("dts", "jump", packet, packet.dts_ms, delta_ms)
        if dts is not None:
            self._last_packet_dts = dts
        if packet.pts is None:
            self._draft("pts", "missing_pts", packet, None, None)

    def _draft(
        self,
        field_name: str,
        reason: str,
        source: FrameRecord | PacketMeta,
        before_ms: Optional[float],
        delta_ms: Optional[float],
    ) -> None:
        if isinstance(source, FrameRecord):
            self._drafts.append(
                _Draft(field_name, reason, source.packet_seq, source.index, before_ms, delta_ms)
            )
        else:
            self._drafts.append(_Draft(field_name, reason, source.seq, None, before_ms, delta_ms))

    def _final_frame_duration(self) -> Fraction:
        if self._duration_hist:
            ticks, _count = self._duration_hist.most_common(1)[0]
            return Fraction(ticks) * self.info.time_base
        if self.info.average_rate and self.info.average_rate > 0:
            return Fraction(1, 1) / self.info.average_rate
        if self._pt_deltas:
            return Fraction(str(median(self._pt_deltas))).limit_denominator(10000) / 1000
        return Fraction(1, 25)

    def _corrected_seconds(self, ordinal: int, duration: Fraction) -> Fraction:
        return Fraction(ordinal) * duration

    def _ticks(self, seconds: Fraction) -> int:
        return int(round(seconds / self.info.time_base))

    def finish(self) -> RepairPlan:
        duration = self._final_frame_duration()
        plan = RepairPlan(
            info=self.info,
            frame_duration=duration,
            target_time_base=self.info.time_base,
            max_jump_ms=round(self.max_jump_ms, 3),
            decoded_frames=self._frame_count,
            packet_count=self._packet_seq,
            unresolved_frames=max(0, self._frame_count - len(self._packet_display_index)),
        )

        if self.capture_mapping:
            lag = max(
                (seq - display_index for seq, display_index in self._packet_display_index.items()),
                default=0,
            )
            plan.pts_lag_frames = max(0, lag)
            for packet_seq, display_index in self._packet_display_index.items():
                corrected_pts = self._corrected_seconds(
                    display_index + plan.pts_lag_frames, duration
                )
                plan.packet_pts[packet_seq] = self._ticks(corrected_pts)
            for packet_seq in range(self._packet_seq):
                corrected_dts = self._corrected_seconds(packet_seq, duration)
                plan.packet_dts[packet_seq] = self._ticks(corrected_dts)

        for draft in self._drafts:
            if draft.field == "dts" and draft.packet_seq is not None:
                after = seconds_to_ms(self._corrected_seconds(draft.packet_seq, duration))
            elif draft.field == "pts" and draft.frame_index is not None:
                after = seconds_to_ms(
                    self._corrected_seconds(draft.frame_index + plan.pts_lag_frames, duration)
                )
            else:
                after = None
            entry = RepairEntry(
                packet_seq=draft.packet_seq,
                frame_index=draft.frame_index,
                field=draft.field,
                reason=draft.reason,
                before_ms=draft.before_ms,
                after_ms=after,
                delta_ms=None if draft.delta_ms is None else round(draft.delta_ms, 3),
            )
            plan.entries.append(entry)
            plan.reasons[draft.reason] += 1
        return plan


@dataclass(slots=True)
class RepairOutcome:
    exit_code: int
    report: dict[str, object]
    output_written: bool
    passes: int


def repair_media(
    input_path: str,
    output_path: str,
    report_path: str,
    *,
    max_jump_ms: float = 250.0,
    window: int = 64,
    max_passes: int = 5,
) -> RepairOutcome:
    """Analyze, iteratively remux, verify pixels, and write a JSON report."""
    from .mux import mux_with_plan
    from .report import analyze_media, write_json

    source = Path(input_path)
    target = Path(output_path)
    if source.resolve() == target.resolve():
        raise ValueError("repair output must differ from input")
    target.parent.mkdir(parents=True, exist_ok=True)

    original = analyze_media(
        input_path, max_jump_ms=max_jump_ms, window=window, capture_plan=True
    )
    report: dict[str, object] = {
        "input": str(source),
        "output": str(target),
        "fixed_entries": original.plan.fixed_entries,
        "reasons": dict(original.plan.reasons),
        "entries": original.plan.entry_dicts(),
        "max_jump_ms": original.plan.max_jump_ms,
        "pixel_digest_match": False,
        "pixel_sequence_before": original.pixels.hexdigest,
        "pixel_sequence_after": None,
        "passes": 0,
        "residual_entries": None,
        "output_written": False,
        "scan_before": original.as_dict(),
        "scan_after": None,
        "exit_code": original.status_code,
    }

    if original.status_code == 3 or original.plan.unrecoverable:
        report["pixel_digest_match"] = False
        payload = {**report, "exit_code": 3}
        write_json(report_path, payload)
        return RepairOutcome(3, payload, False, 0)

    passes = 0
    current = input_path
    final_analysis = original
    output_written = False
    with tempfile.TemporaryDirectory(prefix="framefix-repair-", dir=str(target.parent)) as work:
        stage_paths: list[str] = []
        plan = original.plan
        while passes < max_passes:
            stage = str(Path(work) / f"pass-{passes + 1}{target.suffix or '.mp4'}")
            mux_with_plan(current, stage, plan)
            stage_paths.append(stage)
            passes += 1
            current = stage
            final_analysis = analyze_media(
                current, max_jump_ms=max_jump_ms, window=window, capture_plan=True
            )
            if final_analysis.plan.fixed_entries == 0 or final_analysis.status_code == 3:
                break
            if final_analysis.plan.unrecoverable:
                break
            plan = final_analysis.plan

        pixel_match = (
            final_analysis.pixels.hexdigest == original.pixels.hexdigest
            and final_analysis.pixels.frames == original.pixels.frames
        )
        residual_entries = final_analysis.plan.fixed_entries
        report.update(
            {
                "pixel_digest_match": pixel_match,
                "pixel_sequence_after": final_analysis.pixels.hexdigest,
                "passes": passes,
                "residual_entries": residual_entries,
                "scan_after": final_analysis.as_dict(),
            }
        )
        if not pixel_match:
            exit_code = 4
        elif final_analysis.status_code == 3 or residual_entries:
            exit_code = 3
        else:
            exit_code = 0
            os.replace(current, target)
            output_written = True
        report["output_written"] = output_written
        report["exit_code"] = exit_code
        write_json(report_path, report)

    return RepairOutcome(int(report["exit_code"]), report, output_written, passes)

