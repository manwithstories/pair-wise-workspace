"""DASH MPD parsing (SegmentTemplate / SegmentList).

Split from :mod:`relay_timeline.manifest` to keep each module under the
project size limit.  Segment references go through the same path-safety
and stat helpers as HLS playlists.
"""

from __future__ import annotations

from fractions import Fraction
from pathlib import Path
from xml.etree import ElementTree as ET

from .config import Limits
from .errors import ErrorCode, FatalError
from .manifest import (DASH, ManifestPlan, _safe_resolve, _stat_segment)

_DASH_NS = "urn:mpeg:dash:schema:mpd:2011"


def parse_dash(manifest_path: Path, text: str,
               limits: Limits) -> ManifestPlan:
    try:
        root = ET.fromstring(text)
    except ET.ParseError as exc:
        raise FatalError(
            ErrorCode.MANIFEST_PARSE, f"invalid MPD XML: {exc}",
            segment=manifest_path.name,
        ) from exc
    ns = {"m": _DASH_NS}
    media_present = root.find(".//m:AdaptationSet/m:Representation", ns)
    if media_present is None:
        raise FatalError(
            ErrorCode.MANIFEST_PARSE, "MPD has no Representation",
            segment=manifest_path.name,
        )
    rep = media_present
    bitrate = int(rep.get("bandwidth", "0"))
    timescale = int(rep.get("timescale", "1"))
    template = rep.find("m:SegmentTemplate", ns)
    manifest_dir = manifest_path.parent
    plan = ManifestPlan(DASH, manifest_path, Fraction(0), bitrate)
    if template is not None:
        media_tmpl = template.get("media", "")
        start_no = int(template.get("startNumber", "1"))
        tl_timescale = int(template.get("timescale", timescale))
        timeline = template.find("m:SegmentTimeline", ns)
        if timeline is None:
            raise FatalError(
                ErrorCode.MANIFEST_PARSE,
                "only SegmentTimeline-based templates are supported",
            )
        seq = start_no
        for entry in timeline.findall("m:S", ns):
            count = int(entry.get("r", "0")) + 1
            dur_ticks = int(entry.get("d"))
            for _ in range(count):
                uri = media_tmpl.replace("$Number$", str(seq)).replace(
                    "$RepresentationID$", rep.get("id", "0")
                )
                path = _safe_resolve(manifest_dir, uri, limits)
                duration = Fraction(dur_ticks, tl_timescale)
                plan.segments.append(
                    _stat_segment(seq, path, duration, bitrate, limits)
                )
                seq += 1
    else:
        seq_list = rep.find("m:SegmentList", ns)
        if seq_list is None:
            raise FatalError(
                ErrorCode.MANIFEST_PARSE,
                "MPD Representation needs SegmentTemplate or SegmentList",
            )
        duration = Fraction(int(seq_list.get("duration", "0")),
                            int(seq_list.get("timescale", "1")))
        seq = int(seq_list.get("startNumber", "1"))
        for item in seq_list.findall("m:SegmentURL", ns):
            uri = item.get("media", "")
            path = _safe_resolve(manifest_dir, uri, limits)
            plan.segments.append(
                _stat_segment(seq, path, duration, bitrate, limits))
            seq += 1
    if len(plan.segments) > limits.max_segments:
        raise FatalError(
            ErrorCode.LIMIT_EXCEEDED,
            f"{len(plan.segments)} segments exceed limit {limits.max_segments}",
        )
    if plan.segments:
        plan.target_duration = max(s.duration for s in plan.segments)
    return plan
