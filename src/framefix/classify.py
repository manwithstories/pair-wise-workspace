"""Cross-check packet keyframe flags against decoded picture types."""

from __future__ import annotations

from dataclasses import dataclass, field

from .models import FrameRecord


@dataclass
class GopSummary:
    start_frame: int
    length: int = 0
    runs: list[list[object]] = field(default_factory=list)

    def add(self, pict_type: str) -> None:
        self.length += 1
        if self.runs and self.runs[-1][0] == pict_type:
            self.runs[-1][1] = int(self.runs[-1][1]) + 1
        else:
            self.runs.append([pict_type, 1])

    @property
    def type_code(self) -> str:
        return "".join(f"{kind}{count}" for kind, count in self.runs)

    def as_dict(self) -> dict[str, object]:
        return {
            "start_frame": self.start_frame,
            "length": self.length,
            "types": self.type_code,
        }


class TypeClassifier:
    """Counts I/P/B frames and aligns GOPs with packet keyframe flags."""

    def __init__(self) -> None:
        self.i_frames = 0
        self.p_frames = 0
        self.b_frames = 0
        self.unknown_frames = 0
        self.packet_i_frames = 0
        self.metadata_missing = 0
        self.type_mismatch = 0
        self.mismatch_details: list[dict[str, object]] = []
        self.gops: list[GopSummary] = []
        self._current: GopSummary | None = None

    def add(self, record: FrameRecord) -> None:
        if record.pict_type == "I":
            self.i_frames += 1
        elif record.pict_type == "P":
            self.p_frames += 1
        elif record.pict_type == "B":
            self.b_frames += 1
        else:
            self.unknown_frames += 1

        if record.packet_keyframe is True:
            self.packet_i_frames += 1
        if record.packet_keyframe is None:
            self.metadata_missing += 1
        elif bool(record.packet_keyframe) != (record.pict_type == "I"):
            self.type_mismatch += 1
            if len(self.mismatch_details) < 10:
                self.mismatch_details.append(
                    {
                        "frame_index": record.index,
                        "pts_ms": record.pts_ms,
                        "decoded_type": record.pict_type,
                        "packet_keyframe": record.packet_keyframe,
                    }
                )

        if self._current is None or record.pict_type == "I":
            self._close_gop()
            self._current = GopSummary(start_frame=record.index)
        self._current.add(record.pict_type)

    def _close_gop(self) -> None:
        if self._current is not None and self._current.length:
            self.gops.append(self._current)
        self._current = None

    def finalize(self) -> "TypeClassifier":
        self._close_gop()
        return self

    @property
    def gop_count(self) -> int:
        return len(self.gops)

    @property
    def gop_sequence(self) -> list[dict[str, object]]:
        return [gop.as_dict() for gop in self.gops]

