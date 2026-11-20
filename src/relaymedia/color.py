"""Video colour metadata: read everything, invent nothing.

Two independent pieces of evidence are collected for every video source:

1. the MP4/MOV ``colr`` box (``nclx`` primaries/transfer/matrix), walked
   manually because the container box is not exposed through PyAV's codec
   context;
2. the codec context's own colour fields (bitstream VUI when present).

When the two disagree the raw values are both retained and a
:class:`relaymedia.errors.ErrorRecord` reports the conflict; relaymedia never
fabricates a conversion to "fix" it. Missing fields stay missing (``None``)
and are reported as such rather than being defaulted to BT.709.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path

from .errors import ErrorRecord, Stage

# Subset of ITU-T H.273 / ISO/IEC 23091-2 colour primaries / TRC / matrix.
_PRIMARIES = {1: "bt709", 9: "bt2020", 6: "bt601-smpte", 5: "bt601-pal"}
_TRANSFERS = {1: "bt709", 16: "pq", 18: "hlg", 14: "bt2020-10"}
_MATRICES = {1: "bt709", 9: "bt2020-ncl", 6: "bt601-smpte"}
_RANGES = {0: "unspecified", 1: "tv", 2: "pc"}


def _name(table: dict[int, str], value: int | None) -> str | None:
    if value is None:
        return None
    return table.get(value, f"unknown({value})")


@dataclass(frozen=True, slots=True)
class ColrBox:
    """Raw ``nclx`` triple parsed from an MP4/MOV ``colr`` box."""

    colour_type: str
    primaries: int
    transfer: int
    matrix: int
    range_flag: int

    def to_dict(self) -> dict:
        return {
            "colour_type": self.colour_type,
            "primaries": self.primaries,
            "transfer": self.transfer,
            "matrix": self.matrix,
            "range_flag": self.range_flag,
        }


@dataclass(slots=True)
class ColorReport:
    source: str
    box: ColrBox | None = None
    context_primaries: int | None = None
    context_transfer: int | None = None
    context_matrix: int | None = None
    context_range: int | None = None
    conflicts: list[ErrorRecord] = field(default_factory=list)
    missing: list[str] = field(default_factory=list)

    @property
    def has_conflict(self) -> bool:
        return bool(self.conflicts)

    def summary(self) -> dict:
        return {
            "source": self.source,
            "colr_box": self.box.to_dict() if self.box else None,
            "context": {
                "primaries": _name(_PRIMARIES, self.context_primaries),
                "transfer": _name(_TRANSFERS, self.context_transfer),
                "matrix": _name(_MATRICES, self.context_matrix),
                "range": _name(_RANGES, self.context_range),
            },
            "missing_fields": list(self.missing),
            "conflicts": [e.to_dict() for e in self.conflicts],
        }


def _iter_boxes(data: bytes, start: int, end: int):
    """Yield (box_type, payload_start, payload_end) for MP4 boxes in a range."""
    pos = start
    while pos + 8 <= end:
        size = int.from_bytes(data[pos:pos + 4], "big")
        box_type = data[pos + 4:pos + 8].decode("latin-1")
        header = 8
        if size == 1:  # 64-bit largesize
            if pos + 16 > end:
                break
            size = int.from_bytes(data[pos + 8:pos + 16], "big")
            header = 16
        if size < header:
            break
        yield box_type, pos + header, pos + size
        pos += size


# FullBox types carry a 4-byte version/flags header; stsd adds a 4-byte
# entry count, and visual sample entries carry 78 fixed bytes before children.
_FULL_BOXES = {"meta"}
_STSD_HEADER = 8
_SAMPLE_ENTRY_HEADER = 78
_VIDEO_SAMPLE_ENTRIES = {"avc1", "avc3", "hev1", "hvc1", "mp4v", "encv"}


def _find_box(data: bytes, target: str, start: int, end: int) -> int | None:
    """Recursively locate a box type; returns the payload start offset."""
    for btype, p_start, p_end in _iter_boxes(data, start, end):
        if btype == target and p_end - p_start >= 11:
            return p_start
        # Container boxes and video sample entries hold nested children; the
        # colr box lives inside the avc1/hev1 sample entry in practice.
        if btype in {
            "moov", "trak", "mdia", "minf", "stbl", "stsd", "udta", "edts", "meta",
        } | _VIDEO_SAMPLE_ENTRIES:
            if btype == "stsd":
                child_start = p_start + _STSD_HEADER
            elif btype in _VIDEO_SAMPLE_ENTRIES:
                child_start = p_start + _SAMPLE_ENTRY_HEADER
            elif btype in _FULL_BOXES:
                child_start = p_start + 4
            else:
                child_start = p_start
            found = _find_box(data, target, child_start, p_end)
            if found is not None:
                return found
    return None


def parse_colr_box(path: str | Path) -> ColrBox | None:
    """Find and parse the first ``colr`` box (nested under stsd in practice)."""
    data = Path(path).read_bytes()
    payload_start = _find_box(data, "colr", 0, len(data))
    if payload_start is None:
        return None
    payload = data[payload_start:payload_start + 11]
    return ColrBox(
        colour_type=payload[0:4].decode("latin-1"),
        primaries=int.from_bytes(payload[4:6], "big"),
        transfer=int.from_bytes(payload[6:8], "big"),
        matrix=int.from_bytes(payload[8:10], "big"),
        range_flag=payload[10] >> 7,
    )


def _conflict(report: ColorReport, field_name: str, box_value: int, ctx_value: int) -> None:
    report.conflicts.append(
        ErrorRecord(
            stage=Stage.COLOR.value,
            kind="ColorMetadataConflict",
            message=(
                f"{field_name}: colr box says {box_value} but codec context says {ctx_value}"
            ),
            source=report.source,
            extra={"field": field_name, "box": box_value, "context": ctx_value},
        )
    )


def probe_color(path: str | Path, video_stream) -> ColorReport:
    """Compare the colr box against the codec context, retaining raw values."""
    path = Path(path)
    report = ColorReport(source=str(path), box=parse_colr_box(path))
    ctx = video_stream.codec_context
    # 2 == "unspecified" in the H.273 enums PyAV exposes; treat as missing.
    def _raw(value: int) -> int | None:
        return None if value in (0, 2) else int(value)

    report.context_primaries = _raw(ctx.color_primaries)
    report.context_transfer = _raw(ctx.color_trc)
    report.context_matrix = _raw(ctx.colorspace)
    report.context_range = None if ctx.color_range in (0, 2) else int(ctx.color_range)
    for field_name in ("context_primaries", "context_transfer", "context_matrix", "context_range"):
        if getattr(report, field_name) is None:
            report.missing.append(field_name)
    if report.box is None:
        report.missing.append("colr_box")
        return report
    checks = (
        ("primaries", report.box.primaries, report.context_primaries),
        ("transfer", report.box.transfer, report.context_transfer),
        ("matrix", report.box.matrix, report.context_matrix),
    )
    for field_name, box_value, ctx_value in checks:
        # An unspecified context field is "missing", not a conflict.
        if ctx_value is not None and box_value != ctx_value:
            _conflict(report, field_name, box_value, ctx_value)
    return report
