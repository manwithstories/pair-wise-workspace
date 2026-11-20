"""DASH SegmentTemplate / SegmentTimeline parsing and round-trip rendering.

Only the SegmentTemplate + SegmentTimeline profile is accepted; templates
without a timeline are refused rather than guessed.  An ``S`` entry without
an explicit ``t`` attribute is a missing index entry: its start is
synthesized from the predecessor and reported, so the gap is auditable.
"""

from __future__ import annotations

import xml.etree.ElementTree as ET
from pathlib import Path

from .errors import Code, Diagnostic, ManifestSyntaxError
from .manifest_core import SegmentPlan, SegmentRef, locator_for, normalize_ref
from .timeline import rational, to_float_seconds

_DASH_NS = "urn:mpeg:dash:schema:mpd:2011"


def _q(tag: str) -> str:
    return f"{{{_DASH_NS}}}{tag}"


def parse_dash(text: str, manifest_path: Path, root: Path) -> SegmentPlan:
    try:
        root_el = ET.fromstring(text)
    except ET.ParseError as exc:
        raise ManifestSyntaxError(
            Code.MANIFEST_SYNTAX, f"invalid DASH XML: {exc}",
            locator=locator_for(manifest_path, "", None),
        ) from exc
    manifest_dir = manifest_path.parent
    segments: list[SegmentRef] = []
    diagnostics: list[Diagnostic] = []
    timeline_seen: set[int] = set()

    for adaptation in root_el.iter(_q("AdaptationSet")):
        template = adaptation.find(_q("SegmentTemplate"))
        if template is None:
            continue
        timescale = int(template.get("timescale", "1"))
        time_base = rational(1) / timescale
        media_pattern = template.get("media", "")
        timeline_el = template.find(_q("SegmentTimeline"))
        if timeline_el is None:
            raise ManifestSyntaxError(
                Code.MANIFEST_SYNTAX,
                "SegmentTemplate without SegmentTimeline is not supported",
                locator=locator_for(manifest_path, "", None),
            )
        for rep_el in adaptation.findall(_q("Representation")):
            rep_id = rep_el.get("id", "default")
            pattern = media_pattern.replace("$RepresentationID$", rep_id)
            seq = int(template.get("startNumber", "1")) - 1
            cursor = 0
            for s_el in timeline_el.findall(_q("S")):
                repeat = int(s_el.get("r", "0"))
                for _ in range(repeat + 1):
                    seq += 1
                    has_t = "t" in s_el.attrib
                    if has_t:
                        tick = int(s_el.get("t"))  # type: ignore[arg-type]
                        if tick in timeline_seen and tick != cursor:
                            diagnostics.append(
                                Diagnostic(
                                    Code.TIMESTAMP_COLLISION,
                                    "SegmentTimeline reuses a t value",
                                    locator_for(manifest_path, rep_id, seq),
                                    {"t": tick},
                                )
                            )
                        timeline_seen.add(tick)
                        start = rational(tick) * time_base
                    else:
                        start = rational(cursor) * time_base
                    if "d" not in s_el.attrib:
                        raise ManifestSyntaxError(
                            Code.MANIFEST_SYNTAX,
                            "SegmentTimeline S entry lacks d attribute",
                            locator=locator_for(manifest_path, rep_id, seq),
                        )
                    duration = rational(int(s_el.get("d"))) * time_base  # type: ignore[arg-type]
                    raw = pattern.replace("$Number$", str(seq)).replace(
                        "$Time$", str(int(start / time_base))
                    )
                    locator = locator_for(manifest_path, rep_id, seq, raw)
                    resolved = normalize_ref(raw, manifest_dir, root, locator)
                    seg = SegmentRef(
                        seq, raw, resolved, rep_id, start, duration, time_base,
                        synthesized=not has_t,
                    )
                    if not has_t:
                        seg.diagnostics.append(
                            Diagnostic(
                                Code.MISSING_INDEX_ENTRY,
                                "S entry has no t; start synthesized from predecessor",
                                locator,
                                {"start_seconds": to_float_seconds(start)},
                            )
                        )
                    segments.append(seg)
                    cursor = int(start / time_base) + int(duration / time_base)
    if not segments:
        raise ManifestSyntaxError(
            Code.MANIFEST_SYNTAX, "DASH manifest contains no usable segments",
            locator=locator_for(manifest_path, "", None),
        )
    return SegmentPlan(
        "dash", manifest_path, root, sorted({s.representation for s in segments}),
        segments, diagnostics, text,
    )


def render_dash(plan: SegmentPlan) -> str:
    """Serialize a plan back to DASH text (round-trip counterpart of parse)."""
    timescale = int(1 / plan.segments[0].time_base)
    ns = _DASH_NS
    mpd = ET.Element(
        f"{{{ns}}}MPD",
        {"profiles": "urn:mpeg:dash:profile:isoff-on-demand:2011"},
    )
    adaptation = ET.SubElement(mpd, f"{{{ns}}}AdaptationSet")
    template = ET.SubElement(
        adaptation, f"{{{ns}}}SegmentTemplate",
        {
            "timescale": str(timescale),
            "media": "seg-$RepresentationID$-$Number$.m4s",
            "startNumber": str(plan.segments[0].sequence),
        },
    )
    timeline = ET.SubElement(template, f"{{{ns}}}SegmentTimeline")
    first_rep = plan.representations[0]
    for seg in plan.segments:
        if seg.representation != first_rep:
            continue
        ET.SubElement(
            timeline, f"{{{ns}}}S",
            {
                "t": str(int(seg.start / seg.time_base)),
                "d": str(int(seg.duration / seg.time_base)),
            },
        )
    for rep_id in plan.representations:
        ET.SubElement(adaptation, f"{{{ns}}}Representation", {"id": rep_id})
    ET.indent(mpd, space="  ")
    return ET.tostring(mpd, encoding="unicode") + "\n"


__all__ = ["parse_dash", "render_dash"]
