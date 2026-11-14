"""Aggregate the streaming pipeline and render JSON or terminal reports."""

from __future__ import annotations

import json
from dataclasses import dataclass
from typing import Optional

from .classify import TypeClassifier
from .decode import PixelSequence, iter_frame_records
from .errors import DecodeError as FramefixDecodeError
from .models import MediaInfo
from .probe import iter_packet_meta, probe_file
from .reorder import ReorderBuffer, ReorderSummary
from .repair import RepairPlan, TimestampAnalyzer


@dataclass
class AnalysisResult:
    info: MediaInfo
    classifier: TypeClassifier
    plan: RepairPlan
    pixels: PixelSequence
    reorder: ReorderSummary
    packet_count: int
    decode_error: Optional[str]
    packet_error: Optional[str]
    status_code: int

    @property
    def max_jump_ms(self) -> float:
        return self.plan.max_jump_ms

    def as_dict(self) -> dict[str, object]:
        return {
            "decoded_frames": self.plan.decoded_frames,
            "i_frames": self.classifier.i_frames,
            "p_frames": self.classifier.p_frames,
            "b_frames": self.classifier.b_frames,
            "gop_count": self.classifier.gop_count,
            "repaired_ts_count": self.plan.fixed_entries,
            "max_jump_ms": self.plan.max_jump_ms,
            "type_mismatch": self.classifier.type_mismatch,
            "packet_count": self.packet_count,
            "codec": self.info.codec_name,
            "time_base": str(self.info.time_base),
            "frame_duration_ms": round(float(self.plan.frame_duration) * 1000, 4),
            "pixel_sequence_sha256": self.pixels.hexdigest,
            "reasons": dict(self.plan.reasons),
            "type_mismatch_details": self.classifier.mismatch_details,
            "gop_sequence": self.classifier.gop_sequence,
            "metadata_missing": self.classifier.metadata_missing,
            "packet_i_frames": self.classifier.packet_i_frames,
            "dts_duplicate": self.reorder.duplicate_dts,
            "dts_regressed": self.reorder.regressed_dts,
            "max_reorder_depth": self.reorder.max_buffer_depth,
            "decode_error": self.decode_error,
            "packet_error": self.packet_error,
            "exit_code": self.status_code,
        }


def analyze_media(
    path: str,
    *,
    max_jump_ms: float = 250.0,
    window: int = 64,
    capture_plan: bool = False,
) -> AnalysisResult:
    """Run probe, decode, classify, reorder, and timestamp repair analysis once."""
    info = probe_file(path)
    classifier = TypeClassifier()
    pixels = PixelSequence()
    analyzer = TimestampAnalyzer(info, max_jump_ms, capture_mapping=capture_plan)
    reorder_buffer = ReorderBuffer(window=window)
    decode_error: Optional[str] = None
    packet_error: Optional[str] = None

    try:
        for record in iter_frame_records(path):
            classifier.add(record)
            pixels.update(record)
            analyzer.add_frame(record)
    except FramefixDecodeError as exc:
        decode_error = str(exc)
    classifier.finalize()

    packet_count = 0
    try:
        for packet in iter_packet_meta(path):
            packet_count += 1
            analyzer.add_packet(packet)
            list(reorder_buffer.push(packet))
        for _packet in reorder_buffer.flush():
            pass
    except Exception as exc:
        packet_error = f"{type(exc).__name__}: {exc}"

    plan = analyzer.finish()
    plan.packet_count = max(plan.packet_count, packet_count)
    partial = decode_error is not None or packet_error is not None
    if plan.decoded_frames == 0 or partial or plan.unresolved_frames > 0:
        status_code = 3
    else:
        status_code = 0
    return AnalysisResult(
        info=info,
        classifier=classifier,
        plan=plan,
        pixels=pixels,
        reorder=reorder_buffer.summary,
        packet_count=packet_count,
        decode_error=decode_error,
        packet_error=packet_error,
        status_code=status_code,
    )


def scan_json(result: AnalysisResult) -> str:
    return json.dumps(result.as_dict(), ensure_ascii=False, indent=2)


def render_scan(result: AnalysisResult) -> str:
    data = result.as_dict()
    rows = [
        ("decoded_frames", data["decoded_frames"]),
        ("i_frames", data["i_frames"]),
        ("p_frames", data["p_frames"]),
        ("b_frames", data["b_frames"]),
        ("gop_count", data["gop_count"]),
        ("repaired_ts_count", data["repaired_ts_count"]),
        ("max_jump_ms", data["max_jump_ms"]),
        ("type_mismatch", data["type_mismatch"]),
        ("packet_count", data["packet_count"]),
        ("codec", data["codec"]),
        ("time_base", data["time_base"]),
        ("exit_code", result.status_code),
    ]
    width = max(len(name) for name, _value in rows)
    lines = [f"{'metric'.ljust(width)}  value", f"{'-' * width}  {'-' * 24}"]
    lines.extend(f"{name.ljust(width)}  {value}" for name, value in rows)
    if result.plan.reasons:
        lines.append("")
        lines.append("timestamp reasons")
        for reason, count in sorted(result.plan.reasons.items()):
            lines.append(f"  {reason}: {count}")
    if result.classifier.mismatch_details:
        lines.append("")
        lines.append("first type mismatches")
        for detail in result.classifier.mismatch_details:
            lines.append(
                "  frame={frame_index} pts_ms={pts_ms} decoded={decoded_type} "
                "packet_keyframe={packet_keyframe}".format(**detail)
            )
    if result.decode_error:
        lines.append("")
        lines.append(f"decode_error: {result.decode_error}")
    return "\n".join(lines)


def write_json(path: str, payload: dict[str, object]) -> None:
    from pathlib import Path

    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
