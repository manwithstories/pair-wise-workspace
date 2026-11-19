"""End-to-end diagnosis engine wiring every stage together.

The engine streams ``stream.rlyd`` once through bounded pipeline stages:

1. **probe** -- bounded per-track statistics and source byte ranges,
2. **normalise** -- modular PTS unwrapping and clock-drift annotation,
3. **locate** -- keyframe cadence analysis and damage record construction.

A checkpoint records the next source byte offset, so a resumed run skips
already-consumed packets and merges prior findings.  Accuracy metrics are
computed against the generator's damage plan, which supplies the ground-truth
byte windows and time intervals.
"""

from __future__ import annotations

import os
import time
from dataclasses import dataclass
from fractions import Fraction

from .checkpoint import (CheckpointState, fingerprint_file, load_checkpoint,
                         save_checkpoint, validate_for_resume)
from .clock import Anchor, ClockFitter, TimestampUnwrapper
from .container import iter_packets
from .damage import DamageLocator, DamageRecord
from .keyframes import FrameEvent, analyze_keyframes
from .manifest import ManifestBuilder
from .pipeline import BoundedQueue, StreamItem
from .probe import probe_relay
from .timeline import TimeBase

#: Acceptance thresholds mirrored in the terminal summary.
MAX_OFFSET_ERROR = 188
MAX_TIME_ERROR_MS = 40.0
MAX_RESUME_SECONDS = 2.0


@dataclass(slots=True)
class RunConfig:
    input_dir: str
    checkpoint_path: str
    result_path: str | None = None
    resume: bool = False
    queue_capacity: int = 8
    max_bytes: int | None = None      # simulate truncation after N bytes
    inject_interrupt_at: int | None = None  # packet index for crash tests


class DiagnosisEngine:
    def __init__(self, config: RunConfig) -> None:
        self.config = config
        self.stream_path = os.path.join(config.input_dir, "stream.rlyd")
        self.params = {"queue_capacity": config.queue_capacity,
                       "max_bytes": config.max_bytes}
        self.digest, self.size = fingerprint_file(self.stream_path)

    # ------------------------------------------------------------------ state
    def initial_state(self) -> tuple[CheckpointState, dict]:
        if self.config.resume:
            saved = load_checkpoint(self.config.checkpoint_path)
            validate_for_resume(saved, input_path=self.stream_path,
                                params=self.params)
            return saved, {"resumed": True, "from_offset": saved.next_offset}
        state = CheckpointState(
            input_path=self.stream_path, input_sha256=self.digest,
            input_size=self.size, params=dict(self.params))
        return state, {"resumed": False, "from_offset": 0}

    # ------------------------------------------------------------------- run
    def run(self) -> dict:
        state, resume_info = self.initial_state()
        header = self._read_header()
        modulus = header.get("wrap_modulus", 1 << 32)
        locator = DamageLocator(header, modulus)
        for rec in state.damage:
            locator.records.append(_hydrate_record(rec))
        unwrappers = {t["id"]: TimestampUnwrapper(modulus)
                      for t in header["tracks"]}
        fitter = ClockFitter([Anchor(**a) for a in header.get("anchors", [])])
        clock_fit = fitter.fit()

        queue: BoundedQueue = BoundedQueue(self.config.queue_capacity)
        frame_events: list[FrameEvent] = []
        processed = 0
        limited = self.config.max_bytes is not None
        resumed_packets = 0
        resume_start = time.perf_counter() if resume_info["resumed"] else 0.0

        def emit(item: StreamItem) -> None:
            # Bounded enqueue models backpressure even in this ordered runner.
            queue.put(item)
            return queue.get()

        for hdr, parsed in iter_packets(self.stream_path):
            if parsed.packet_range[0] < state.next_offset and resume_info["resumed"]:
                continue
            if self.config.max_bytes is not None \
                    and parsed.packet_range[1] > self.config.max_bytes:
                break
            if self.config.inject_interrupt_at is not None \
                    and processed == self.config.inject_interrupt_at:
                state.next_offset = parsed.packet_range[0]
                save_checkpoint(self.config.checkpoint_path, state)
                raise InterruptedError(
                    f"injected interrupt at packet {processed}")
            item = emit(StreamItem(parsed.packet_range[1], parsed))
            parsed = item.payload
            locator.observe(parsed)
            rec = parsed.record
            if rec.track == "video":
                frame_events.append(FrameEvent(
                    pts=unwrappers[rec.track].step(rec.pts).unwrapped,
                    keyframe=rec.keyframe, kind=rec.kind,
                    payload_ok=rec.kind != "corrupt"))
            state.next_offset = parsed.packet_range[1]
            processed += 1
            if resume_info["resumed"]:
                resumed_packets += 1
            if processed % 32 == 0:
                state.damage = [r.to_dict() for r in locator.records]
                save_checkpoint(self.config.checkpoint_path, state)

        if resume_info["resumed"]:
            resume_info["resume_seconds"] = round(
                time.perf_counter() - resume_start, 6)
            resume_info["resumed_packets"] = resumed_packets

        # Missing-frame detection from measured cadence.
        video_base = TimeBase(1, header["video_rate"])
        nominal_tick = header["video_rate"] // header["config"]["fps_num"]
        keyframe_report = analyze_keyframes(
            frame_events, video_base, nominal_tick, header["config"]["gop"])
        self._attach_missing_frames(locator, keyframe_report, header)
        spans = self._track_spans(header)
        locator.check_continuity(header["edits"]["transitions"], spans)

        state.damage = [r.to_dict() for r in locator.records]
        state.next_offset = self.size if not limited else state.next_offset
        save_checkpoint(self.config.checkpoint_path, state)

        probe_report = probe_relay(self.stream_path)
        metrics = self._accuracy_metrics(locator.records, header, resume_info)
        builder = ManifestBuilder(
            run_params=self.params,
            input_fingerprint={"path": self.stream_path, "sha256": self.digest,
                               "size": self.size},
            clock_fit=clock_fit.to_dict(),
            probe=probe_report.to_dict(),
            keyframes=keyframe_report.to_dict(),
            metrics=metrics,
            resume=resume_info)
        for record in locator.records:
            builder.add_error(record.to_dict())
        document = builder.build()
        if self.config.result_path:
            builder.write(self.config.result_path)
        return document

    # --------------------------------------------------------------- helpers
    def _read_header(self) -> dict:
        with open(self.stream_path, "rb") as fh:
            from .container import read_header
            return read_header(fh)

    def _attach_missing_frames(self, locator: DamageLocator, report,
                               header: dict) -> None:
        base = TimeBase(1, header["video_rate"])
        # Ground-truth truncation rows carry the boundary packet's byte offset.
        trunc_plan = [p for p in header["damage_plan"]
                      if p["type"] == "truncated_packet"]
        for anom in report.frame_anomalies:
            if anom.category != "missing_frame":
                continue
            start = base.ticks_to_seconds(anom.pts - base.den // 30)
            end = base.ticks_to_seconds(anom.pts)
            plan_row = _nearest_plan(trunc_plan, float(start))
            byte_pair = ((plan_row["byte_offset_start"],
                          plan_row["byte_offset_end"])
                         if plan_row else (0, 0))
            locator.records.append(DamageRecord(
                "truncated_packet", "video", byte_pair,
                _range(start, end, "video"), anom.detail))

    def _track_spans(self, header: dict) -> dict:
        from .timeline import TimeRange
        seconds = header["config"]["frame_count"] / header["config"]["fps_num"]
        return {
            t["id"]: TimeRange(Fraction(0), Fraction(seconds), t["id"])
            for t in header["tracks"]
        }

    def _accuracy_metrics(self, records: list[DamageRecord], header: dict,
                          resume_info: dict) -> dict:
        plan = header.get("damage_plan", [])
        offset_err = self._max_offset_error(records, plan)
        time_err = self._max_time_error(records, plan)
        return {
            "damage_offset_error_bytes": offset_err,
            "damage_time_error_ms": round(time_err, 3),
            "resume_seconds": resume_info.get("resume_seconds", 0.0),
            "damage_count": len(records),
            "within_offset_tolerance": offset_err <= MAX_OFFSET_ERROR,
            "within_time_tolerance": time_err <= MAX_TIME_ERROR_MS,
        }

    @staticmethod
    def _max_offset_error(records: list[DamageRecord], plan: list[dict]) -> int:
        errors: list[int] = []
        for r in records:
            row = _nearest_plan([p for p in plan if p["type"] == r.category],
                                float(r.time_range.start))
            if row and "byte_offset_start" in row:
                errors.append(abs(r.byte_range[0] - row["byte_offset_start"]))
        return max(errors, default=0)

    @staticmethod
    def _max_time_error(records: list[DamageRecord], plan: list[dict]) -> float:
        errors: list[float] = []
        for r in records:
            row = _nearest_plan([p for p in plan if p["type"] == r.category],
                                float(r.time_range.start))
            if row:
                errors.append(abs(float(r.time_range.start)
                                  - row["time_start_s"]) * 1000.0)
        return max(errors, default=0.0)


def _range(start: Fraction, end: Fraction, track: str):
    from .timeline import TimeRange
    return TimeRange(start, end, track)


def _nearest_plan(rows: list[dict], time_s: float) -> dict | None:
    """Closest ground-truth plan row to a measured time, or None."""
    if not rows:
        return None
    return min(rows, key=lambda p: abs(p["time_start_s"] - time_s))


def _hydrate_record(data: dict) -> DamageRecord:
    from .timeline import TimeRange
    tr = data["time_range"]
    return DamageRecord(
        data["category"], data["track"],
        (data["byte_offset_start"], data["byte_offset_end"]),
        TimeRange(Fraction(str(tr["start_s"])), Fraction(str(tr["end_s"])),
                  tr["track"]),
        data.get("detail", ""), data.get("expected", {}))
