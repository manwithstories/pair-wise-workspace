"""DASH SegmentTemplate / SegmentList parsing."""

from __future__ import annotations

import posixpath
import re
from pathlib import Path
from xml.etree import ElementTree as ET

from .errors import ErrorCollector, ErrorStage, fatal
from .manifest_common import (
    ManifestError,
    ParsedManifest,
    Representation,
    SegmentRef,
    _codec_kind,
    safe_resolve,
)


def parse_iso8601_duration(text: str) -> int:
    match = re.match(r"PT(?:(\d+)H)?(?:(\d+)M)?(?:([\d.]+)S)?", text)
    if not match:
        return 0
    hours, minutes, seconds = (float(g or 0) for g in match.groups())
    return round((hours * 3600 + minutes * 60 + seconds) * 1_000_000)


def _dash_ref(root: Path, base: Path, uri: str, index: int, start_us: int,
              dur_us: int, asset: str, errors: ErrorCollector) -> SegmentRef:
    try:
        resolved = safe_resolve(root, base, uri)
    except ManifestError as exc:
        errors.add(fatal(ErrorStage.MANIFEST, asset, str(exc), segment_index=index, path=uri))
        resolved = base / uri
    return SegmentRef(index, resolved, dur_us, start_us, uri=uri)


def _dash_rep(element: ET.Element, base: Path, root: Path, path: Path,
              errors: ErrorCollector, kind: str, base_url: str) -> Representation:
    asset = str(path)
    rep_id = element.get("id", "rep")
    codec = element.get("codecs", "")
    bandwidth = int(element.get("bandwidth", "0"))
    template = element.find("SegmentTemplate")
    seg_list_first = element.find("SegmentList")
    timescale = int((template.get("timescale") if template is not None else None)
                    or (seg_list_first.get("timescale") if seg_list_first is not None else None)
                    or 1)
    segs: list[SegmentRef] = []
    if template is not None:
        media_tpl = template.get("media", "")
        start_no = int(template.get("startNumber", "1"))
        timeline = template.find("SegmentTimeline")
        idx = start_no
        if timeline is not None:
            t = 0
            for s in timeline.findall("S"):
                t0 = int(s.get("t", t))
                dur = int(s.get("d"))
                repeat = int(s.get("r", "0"))
                for _ in range(repeat + 1):
                    uri = (media_tpl.replace("$Number$", str(idx))
                           .replace("$Time$", str(t0)).replace("$RepresentationID$", rep_id))
                    segs.append(_dash_ref(root, base, uri, idx,
                                          t0 * 1_000_000 // timescale,
                                          dur * 1_000_000 // timescale, asset, errors))
                    t0 += dur
                    idx += 1
                t = t0
        else:
            dur = int(template.get("duration", "0"))
            count = int(template.get("count", "0")) or 1
            for n in range(count):
                number = start_no + n
                uri = media_tpl.replace("$Number$", str(number)).replace("$RepresentationID$", rep_id)
                segs.append(_dash_ref(root, base, uri, number,
                                      (number - start_no) * dur * 1_000_000 // timescale,
                                      dur * 1_000_000 // timescale, asset, errors))
    for seg_list in element.findall("SegmentList"):
        dur = int(seg_list.get("duration", "0"))
        for n, item in enumerate(seg_list.findall("SegmentURL")):
            rng = item.get("mediaRange")
            offset = length = None
            if rng:
                start_s, _, end_s = rng.partition("-")
                offset, length = int(start_s), int(end_s) - int(start_s) + 1
            ref = _dash_ref(root, base, posixpath.join(base_url, item.get("media", "")), n,
                            n * dur * 1_000_000 // timescale,
                            dur * 1_000_000 // timescale, asset, errors)
            ref.byte_offset, ref.byte_length = offset, length
            segs.append(ref)
    target = None
    if template is not None and template.get("duration"):
        target = int(template.get("duration", "0")) * 1_000_000 // timescale
    return Representation(rep_id, kind, bandwidth, codec, base, segs, target)


def parse_dash(path: Path, root: Path) -> ParsedManifest:
    errors = ErrorCollector()
    tree = ET.parse(path)
    mpd = tree.getroot()
    total_us = parse_iso8601_duration(mpd.get("mediaPresentationDuration", "PT0S")) or None
    base = path.parent
    base_url = mpd.findtext("BaseURL", default="")
    if base_url:
        base = path.parent / posixpath.dirname(base_url)
    reps: list[Representation] = []
    for adaptation in mpd.iter("AdaptationSet"):
        mime = (adaptation.get("mimeType") or adaptation.get("contentType") or "").lower()
        fallback = "audio" if "audio" in mime else "video"
        for element in adaptation.findall("Representation"):
            kind = _codec_kind(element.get("codecs", ""), fallback)
            reps.append(_dash_rep(element, base, root, path, errors, kind, base_url))
    return ParsedManifest("dash", path, root, reps, total_us, errors)
