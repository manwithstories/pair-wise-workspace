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


def _evidence(name: str) -> list[tuple[str, str, str, str]]:
    """Ordered evidence kinds for one field: (kind, raw_key, label_key, precedence)."""
    return [
        ("codec", name, name, "bitstream VUI, acted on by decoders"),
        ("colr", f"colr:{name}", name, "container sample-entry colr box"),
        ("tag", TAG_KEYS[name], name, "container stream tag"),
    ]


def _known_label(field_name: str, label: str | None) -> bool:
    if label is None:
        return False
    table = FIELDS[field_name]
    return label in set(table.values()) and label not in ("reserved", "unspecified")


def resolve(
    stream: StreamProbe, *, path: str | None = None
) -> ColorRecord:
    """Decide the archived colour values for one video stream.

    Precedence is bitstream VUI > MP4 ``colr`` box > container tag, because
    that is the order in which a real decoder consumes the information.  Every
    disagreement is recorded with both values, and the winner is named in a
    ``choice``/``reason`` pair so the decision can be re-audited.
    """
    record = ColorRecord(stream_index=stream.index)
    raw = stream.color_raw
    for name in FIELDS:
        claims: list[tuple[str, int | None, str | None, str]] = []
        evidence: dict[str, str] = {}
        for kind, raw_key, _label_key, _why in _evidence(name):
            if raw_key not in raw:
                continue
            text = raw[raw_key]
            evidence[raw_key] = text
            value: int | None = None
            label: str | None = None
            if kind in ("codec", "colr"):
                try:
                    value = int(text)
                except ValueError:
                    value = None
                if value is not None:
                    label = FIELDS[name].get(value)
            else:
                label = _tag_label(name, text)
            claims.append((kind, value, label, text))

        trusted = [c for c in claims if _known_label(name, c[2])]
        if trusted:
            kind, value, label, _text = trusted[0]
            entry = ColorField(name, value, label, kind, True, evidence)
            if kind == "colr":
                # Numeric colr values look trustworthy but the enum may differ.
                entry.value = value
            others = [
                {"source": c[0], "value": c[2] if c[2] is not None else c[3]}
                for c in trusted[1:]
                if c[2] != label
            ]
            if others:
                record.decisions.append({
                    "field": name,
                    "choice": label,
                    "source": kind,
                    "reason": "source-conflict",
                    "conflicts": others,
                })
                record.warnings.append(
                    MediaError(
                        "color-conflict",
                        f"{name}: {kind} says {label!r}, "
                        + ", ".join(f"{o['source']} says {o['value']!r}" for o in others)
                        + f"; kept the {kind} value",
                        stage="color", path=path, stream_index=stream.index,
                        detail={"choice": label, "conflicts": others},
                    )
                )
            else:
                record.decisions.append({
                    "field": name, "choice": label, "source": kind,
                    "reason": f"{kind}-only" if len(claims) == 1 else f"{kind}-unanimous",
                })
            record.fields[name] = entry
            continue

        if claims:
            kind, value, _label, text = claims[0]
            record.decisions.append({
                "field": name, "choice": text, "source": kind,
                "reason": "unsupported-value-kept",
            })
            record.warnings.append(
                MediaError(
                    "color-unsupported",
                    f"{name} value {text!r} from {kind} is not a known enumeration; kept as-is",
                    stage="color", path=path, stream_index=stream.index,
                    detail={"raw": text, "source": kind},
                )
            )
            record.fields[name] = ColorField(name, value, None, kind, False, evidence)
            continue

        record.decisions.append({
            "field": name, "choice": None, "source": "absent",
            "reason": "not-in-source",
        })
        record.fields[name] = ColorField(name, None, None, "absent", False, evidence)
    return record


def summary(record: ColorRecord) -> dict[str, Any]:
    """Compact, human-readable colour block for terminal output."""
    return {
        "primaries": record.label("primaries"),
        "transfer": record.label("transfer"),
        "space": record.label("space"),
        "range": record.label("range"),
        "conflicts": sum(
            1 for d in record.decisions if d["reason"] == "source-conflict"
        ),
        "absent": record.unresolved(),
    }
