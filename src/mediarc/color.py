"""Colour metadata normalisation with an audit trail.

The goal is *not* to produce tidy colour tags.  It is to record what the source
actually claimed, decide which claim the archive trusts, and keep the evidence
for that decision.  Consequently:

* a field that the source does not carry stays ``null`` — nothing is invented;
* a codec-level value and a container tag that disagree are both preserved and
  the codec value wins, because that is the value a decoder acts on;
* a numeric value outside our enum table is kept verbatim and marked
  unsupported rather than being coerced to a nearby name.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from .errors import MediaError
from .probe import StreamProbe

# FFmpeg enumerations (libavutil/pixfmt.h).  Names follow FFmpeg's spelling.
PRIMARIES = {
    0: "reserved", 1: "bt709", 2: "unspecified", 3: "reserved",
    4: "bt470m", 5: "bt470bg", 6: "smpte170m", 7: "smpte240m", 8: "film",
    9: "bt2020", 10: "smpte428", 11: "smpte431", 12: "smpte432", 22: "ebu3213",
}
TRANSFER = {
    0: "reserved", 1: "bt709", 2: "unspecified", 3: "reserved",
    4: "gamma22", 5: "gamma28", 6: "smpte170m", 7: "smpte240m", 8: "linear",
    9: "log", 10: "log_sqrt", 11: "iec61966-2-4", 12: "bt1361-ecg",
    13: "iec61966-2-1", 14: "bt2020-10", 15: "bt2020-12", 16: "smpte2084",
    17: "smpte428", 18: "arib-std-b67",
}
SPACE = {
    0: "rgb", 1: "bt709", 2: "unspecified", 3: "reserved", 4: "fcc",
    5: "bt470bg", 6: "smpte170m", 7: "smpte240m", 8: "ycgco",
    9: "bt2020-ncl", 10: "bt2020-cl", 11: "smpte2085",
    12: "chroma-derived-ncl", 13: "chroma-derived-cl", 14: "ictcp",
}
RANGE = {0: "unspecified", 1: "tv", 2: "pc"}

FIELDS: dict[str, dict[int, str]] = {
    "primaries": PRIMARIES,
    "transfer": TRANSFER,
    "space": SPACE,
    "range": RANGE,
}

#: Container tags that may restate the same fact, keyed by our field name.
TAG_KEYS = {
    "primaries": "tag:COLOR_PRIMARIES",
    "transfer": "tag:COLOR_TRANSFER",
    "space": "tag:COLOR_SPACE",
    "range": "tag:COLOR_RANGE",
}

_TAG_ALIASES = {
    "smpte240m": "smpte240m",
    "bt2020-10": "bt2020-10",
    "bt2020-12": "bt2020-12",
    "iec61966-2-1": "iec61966-2-1",
    "iec61966-2-4": "iec61966-2-4",
    "arib-std-b67": "arib-std-b67",
    "bt2020nc": "bt2020-ncl",
    "bt2020c": "bt2020-cl",
    "bt2020_ncl": "bt2020-ncl",
    "bt2020_cl": "bt2020-cl",
    "smpte2084": "smpte2084",
    "smpte428": "smpte428",
    "bt709": "bt709",
    "bt470m": "bt470m",
    "bt470bg": "bt470bg",
    "smpte170m": "smpte170m",
    "gamma22": "gamma22",
    "gamma28": "gamma28",
    "linear": "linear",
    "log": "log",
    "log_sqrt": "log_sqrt",
    "film": "film",
    "reserved": "reserved",
    "unspecified": "unspecified",
    "unknown": "unspecified",
    "mpeg": "tv",
    "tv": "tv",
    "limited": "tv",
    "jpeg": "pc",
    "pc": "pc",
    "full": "pc",
    "rgb": "rgb",
    "fcc": "fcc",
    "ycgco": "ycgco",
    "smpte431": "smpte431",
    "smpte432": "smpte432",
    "ebu3213": "ebu3213",
    "ictcp": "ictcp",
    "smpte2085": "smpte2085",
    "chroma-derived-ncl": "chroma-derived-ncl",
    "chroma-derived-cl": "chroma-derived-cl",
    "bt1361-ecg": "bt1361-ecg",
    "log_sqrt ": "log_sqrt",
}


@dataclass(slots=True)
class ColorField:
    """One colour fact plus where it came from and whether we trust it."""

    name: str
    value: int | None
    label: str | None
    source: str  # "codec" | "tag" | "absent"
    supported: bool
    raw: dict[str, str] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "value": self.value,
            "label": self.label,
            "source": self.source,
            "supported": self.supported,
            "raw": dict(self.raw),
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "ColorField":
        return cls(
            name=str(data["name"]),
            value=None if data.get("value") is None else int(data["value"]),
            label=data.get("label"),
            source=str(data.get("source", "absent")),
            supported=bool(data.get("supported", False)),
            raw={str(k): str(v) for k, v in (data.get("raw") or {}).items()},
        )


@dataclass(slots=True)
class ColorRecord:
    """Colour metadata for one video stream, with every decision recorded."""

    stream_index: int
    fields: dict[str, ColorField] = field(default_factory=dict)
    decisions: list[dict[str, Any]] = field(default_factory=list)
    warnings: list[MediaError] = field(default_factory=list)

    def get(self, name: str) -> ColorField:
        return self.fields[name]

    def label(self, name: str) -> str | None:
        entry = self.fields.get(name)
        return entry.label if entry else None

    def is_complete(self) -> bool:
        return all(
            entry.source != "absent" for entry in self.fields.values()
        )

    def unresolved(self) -> list[str]:
        return [name for name, entry in self.fields.items() if entry.source == "absent"]

    def to_dict(self) -> dict[str, Any]:
        return {
            "stream_index": self.stream_index,
            "fields": {name: entry.to_dict() for name, entry in self.fields.items()},
            "decisions": [dict(d) for d in self.decisions],
            "warnings": [w.to_dict() for w in self.warnings],
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "ColorRecord":
        return cls(
            stream_index=int(data["stream_index"]),
            fields={
                name: ColorField.from_dict(entry)
                for name, entry in (data.get("fields") or {}).items()
            },
            decisions=[dict(d) for d in data.get("decisions", [])],
            warnings=[MediaError.from_dict(w) for w in data.get("warnings", [])],
        )


def _tag_label(field_name: str, raw: str) -> str | None:
    text = raw.strip().strip('"').lower()
    if field_name == "range":
        return _TAG_ALIASES.get(text, text or None)
    return _TAG_ALIASES.get(text, text or None)


def _known_label(field_name: str, label: str | None) -> bool:
    if label is None:
        return False
    table = FIELDS[field_name]
    return label in set(table.values()) and label not in ("reserved", "unspecified")


def resolve(
    stream: StreamProbe, *, path: str | None = None
) -> ColorRecord:
    """Decide the archived colour values for one video stream."""
    record = ColorRecord(stream_index=stream.index)
    raw = stream.color_raw
    for name in FIELDS:
        codec_raw = raw.get(name)
        tag_raw = raw.get(TAG_KEYS[name])
        evidence = {k: v for k, v in raw.items() if k in (name, TAG_KEYS[name])}
        codec_label = None
        codec_value = None
        if codec_raw is not None:
            try:
                codec_value = int(codec_raw)
            except ValueError:
                codec_value = None
            if codec_value is not None:
                codec_label = FIELDS[name].get(codec_value)
        tag_label = _tag_label(name, tag_raw) if tag_raw else None

        if codec_label and _known_label(name, codec_label):
            if tag_label and tag_label != codec_label and _known_label(name, tag_label):
                record.decisions.append({
                    "field": name,
                    "choice": codec_label,
                    "source": "codec",
                    "reason": "codec-and-tag-conflict",
                    "codec": codec_label,
                    "tag": tag_label,
                })
                record.warnings.append(
                    MediaError(
                        "color-conflict",
                        f"{name}: codec says {codec_label!r}, tag says {tag_label!r}; "
                        f"kept the codec value a decoder acts on",
                        stage="color", path=path, stream_index=stream.index,
                        detail={"codec": codec_label, "tag": tag_label},
                    )
                )
            elif tag_label and tag_label == codec_label:
                record.decisions.append({
                    "field": name, "choice": codec_label, "source": "codec",
                    "reason": "codec-and-tag-agree",
                })
            else:
                record.decisions.append({
                    "field": name, "choice": codec_label, "source": "codec",
                    "reason": "codec-only",
                })
            entry = ColorField(name, codec_value, codec_label, "codec", True, evidence)
        elif tag_label and _known_label(name, tag_label):
            codec_note = "absent" if codec_label is None else f"unsupported({codec_label})"
            record.decisions.append({
                "field": name, "choice": tag_label, "source": "tag",
                "reason": "tag-only", "codec": codec_note,
            })
            entry = ColorField(name, None, tag_label, "tag", True, evidence)
        elif codec_raw is not None and codec_label is None:
            record.decisions.append({
                "field": name, "choice": codec_raw, "source": "codec",
                "reason": "unsupported-value-kept",
            })
            entry = ColorField(name, codec_value, None, "codec", False, evidence)
            record.warnings.append(
                MediaError(
                    "color-unsupported",
                    f"{name} value {codec_raw!r} is not a known enumeration; kept as-is",
                    stage="color", path=path, stream_index=stream.index,
                    detail={"raw": codec_raw},
                )
            )
        elif tag_raw:
            record.decisions.append({
                "field": name, "choice": tag_raw, "source": "tag",
                "reason": "unsupported-tag-kept",
            })
            entry = ColorField(name, None, tag_raw, "tag", False, evidence)
            record.warnings.append(
                MediaError(
                    "color-unsupported",
                    f"{name} tag {tag_raw!r} is not a known enumeration; kept as-is",
                    stage="color", path=path, stream_index=stream.index,
                    detail={"raw": tag_raw},
                )
            )
        else:
            record.decisions.append({
                "field": name, "choice": None, "source": "absent",
                "reason": "not-in-source",
            })
            entry = ColorField(name, None, None, "absent", False, evidence)
        record.fields[name] = entry
    return record


def summary(record: ColorRecord) -> dict[str, Any]:
    """Compact, human-readable colour block for terminal output."""
    return {
        "primaries": record.label("primaries"),
        "transfer": record.label("transfer"),
        "space": record.label("space"),
        "range": record.label("range"),
        "conflicts": sum(
            1 for d in record.decisions if d["reason"] == "codec-and-tag-conflict"
        ),
        "absent": record.unresolved(),
    }
