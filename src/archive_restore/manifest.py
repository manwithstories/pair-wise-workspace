"""HLS master/media playlist and DASH SegmentTemplate/SegmentList parsing.

Every segment reference is resolved to a concrete path *inside* the input
root.  Absolute URIs, directory traversal and references escaping the manifest
root are rejected with structured fatal errors rather than silently opened.
"""

from __future__ import annotations

import posixpath
import re
from dataclasses import dataclass, field
from pathlib import Path
from xml.etree import ElementTree as ET

from .errors import ErrorCollector, ErrorStage, fatal, retryable, skippable

_URI_SCHEME = re.compile(r"^[a-zA-Z][a-zA-Z0-9+.-]*:")
_VIDEO_CODECS = ("avc", "hvc", "hev", "vp0", "vp9", "av01", "mp4v")
_AUDIO_CODECS = ("mp4a", "opus", "ac-3", "ec-3", "flac")


@dataclass
class SegmentRef:
    """One segment entry resolved relative to its manifest directory."""

    index: int
    path: Path
    duration_us: int | None = None
    start_us: int | None = None
    byte_offset: int | None = None
    byte_length: int | None = None
    uri: str = ""


@dataclass
class Representation:
    rep_id: str
    kind: str  # "video" | "audio"
    bandwidth: int
    codec: str
    base_dir: Path
    segments: list[SegmentRef] = field(default_factory=list)
    target_duration_us: int | None = None


@dataclass
class ParsedManifest:
    kind: str
    path: Path
    root_dir: Path
    representations: list[Representation]
    target_duration_us: int | None
    errors: ErrorCollector = field(default_factory=ErrorCollector)

    def has_fatal(self) -> bool:
        return self.errors.has_fatal()


class ManifestError(ValueError):
    """Raised for references that must never be opened."""
def safe_resolve(root_dir: Path, base_dir: Path, uri: str) -> Path:
    """Resolve *uri* under *base_dir*, guaranteeing the result stays in root."""
    if _URI_SCHEME.match(uri):
        raise ManifestError(f"absolute URI reference rejected: {uri}")
    if uri.startswith("/"):
        raise ManifestError(f"absolute path reference rejected: {uri}")
    normalized = posixpath.normpath(uri)
    parts = normalized.split("/")
    if parts[0] in ("..", "") and normalized != ".":
        raise ManifestError(f"directory traversal rejected: {uri}")
    root = root_dir.resolve()
    candidate = (base_dir / normalized).resolve()
    if candidate != root and root not in candidate.parents:
        raise ManifestError(f"reference outside manifest root rejected: {uri}")
    return candidate


def _codec_kind(codec: str, fallback: str = "video") -> str:
    head = codec.lower()
    if head.startswith(_VIDEO_CODECS):
        return "video"
    if head.startswith(_AUDIO_CODECS):
        return "audio"
    return fallback


def _parse_attrs(line: str) -> dict[str, str]:
    attrs: dict[str, str] = {}
    for match in re.finditer(r'([A-Z0-9-]+)=("([^"]*)"|[^,]+)', line):
        attrs[match.group(1)] = match.group(3) if match.group(3) is not None else match.group(2)
    return attrs


def _parse_hls_attributes(path: Path, root: Path, errors: ErrorCollector) -> list[Representation]:
    asset = str(path)
    lines = path.read_text(errors="replace").splitlines()
    if not lines or not lines[0].lstrip().startswith("#EXTM3U"):
        raise ManifestError("not an HLS playlist (missing #EXTM3U)")
    is_master = any(l.startswith("#EXT-X-STREAM-INF") or l.startswith("#EXT-X-MEDIA") for l in lines)
    if is_master:
        reps: list[Representation] = []
        pending: dict[str, str] | None = None
        for raw in lines:
            line = raw.strip()
            if line.startswith("#EXT-X-STREAM-INF"):
                pending = _parse_attrs(line)
            elif line.startswith("#EXT-X-MEDIA"):
                attrs = _parse_attrs(line)
                if attrs.get("TYPE") == "AUDIO" and "URI" in attrs:
                    reps.append(_load_hls_media(attrs["URI"], path, root, errors, "audio",
                                                attrs.get("NAME", attrs["URI"]), attrs.get("CODECS", "mp4a")))
            elif line and not line.startswith("#") and pending is not None:
                reps.append(_load_hls_media(line, path, root, errors,
                                            _codec_kind(pending.get("CODECS", "")),
                                            pending.get("VIDEO", line), pending.get("CODECS", ""),
                                            int(pending.get("BANDWIDTH", "0"))))
                pending = None
        return reps
    return [_hls_media_representation(lines, path, root, errors)]


def _load_hls_media(uri: str, parent: Path, root: Path, errors: ErrorCollector,
                    kind: str, rep_id: str, codec: str, bandwidth: int = 0) -> Representation:
    try:
        media_path = safe_resolve(root, parent.parent, uri)
        lines = media_path.read_text(errors="replace").splitlines()
    except (ManifestError, OSError) as exc:
        errors.add(fatal(ErrorStage.MANIFEST, str(parent), str(exc), path=uri))
        return Representation(rep_id, kind, bandwidth, codec, parent.parent)
    return _hls_media_representation(lines, media_path, root, errors, rep_id, kind, codec, bandwidth)


def _hls_media_representation(lines: list[str], path: Path, root: Path,
                              errors: ErrorCollector, rep_id: str | None = None,
                              kind: str = "video", codec: str = "",
                              bandwidth: int = 0) -> Representation:
    asset = str(path)
    sequence = 0
    target_us: int | None = None
    pending_dur: float | None = None
    pending_range: tuple[int, int | None] | None = None
    segs: list[SegmentRef] = []
    cursor_us = 0
    for raw in lines:
        line = raw.strip()
        if line.startswith("#EXT-X-TARGETDURATION:"):
            target_us = int(float(line.split(":", 1)[1]) * 1_000_000)
        elif line.startswith("#EXT-X-MEDIA-SEQUENCE:"):
            sequence = int(line.split(":", 1)[1])
        elif line.startswith("#EXTINF:"):
            pending_dur = float(line.split(":", 1)[1].split(",", 1)[0])
        elif line.startswith("#EXT-X-BYTERANGE:"):
            spec = line.split(":", 1)[1]
            length, _, offset = spec.partition("@")
            pending_range = (int(length), int(offset) if offset else None)
        elif line and not line.startswith("#"):
            if pending_dur is None:
                errors.add(skippable(ErrorStage.MANIFEST, asset, "segment URI without EXTINF",
                                     segment_index=sequence, path=line))
                continue
            try:
                seg_path = safe_resolve(root, path.parent, line)
            except ManifestError as exc:
                errors.add(fatal(ErrorStage.MANIFEST, asset, str(exc), segment_index=sequence, path=line))
                pending_dur, pending_range = None, None
                sequence += 1
                continue
            dur_us = round(pending_dur * 1_000_000)
            offset, length = None, None
            if pending_range is not None:
                length, offset = pending_range
            segs.append(SegmentRef(sequence, seg_path, dur_us, cursor_us, offset, length, line))
            cursor_us += dur_us
            sequence += 1
            pending_dur, pending_range = None, None
    if pending_dur is not None:
        errors.add(retryable(ErrorStage.MANIFEST, asset, "truncated playlist: EXTINF without segment URI",
                             segment_index=sequence))
    if not any(l.strip() == "#EXT-X-ENDLIST" for l in lines):
        errors.add(retryable(ErrorStage.MANIFEST, asset, "truncated playlist: missing #EXT-X-ENDLIST"))
    return Representation(rep_id or path.stem, kind, bandwidth, codec, path.parent, segs, target_us)


def _parse_iso8601_duration(text: str) -> int:
    match = re.match(r"PT(?:(\d+)H)?(?:(\d+)M)?(?:([\d.]+)S)?", text)
    if not match:
        return 0
    hours, minutes, seconds = (float(g or 0) for g in match.groups())
    return round((hours * 3600 + minutes * 60 + seconds) * 1_000_000)


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
    template = element.find("SegmentTemplate")
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
                    uri = media_tpl.replace("$Number$", str(idx)).replace("$Time$", str(t0)).replace("$RepresentationID$", rep_id)
                    segs.append(_dash_ref(root, base, uri, idx, t0 * 1_000_000 // timescale,
                                          dur * 1_000_000 // timescale, asset, errors))
                    t0 += dur
                    idx += 1
                t = t0
        else:
            dur, count = int(template.get("duration", "0")), int(template.get("count", "0")) or 1
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
                            n * dur * 1_000_000 // timescale, dur * 1_000_000 // timescale, asset, errors)
            ref.byte_offset, ref.byte_length = offset, length
            segs.append(ref)
    target = None
    if template is not None and template.get("duration"):
        target = int(template.get("duration", "0")) * 1_000_000 // timescale
    return Representation(rep_id, kind, bandwidth, codec, base, segs, target)


def _dash_ref(root: Path, base: Path, uri: str, index: int, start_us: int,
              dur_us: int, asset: str, errors: ErrorCollector) -> SegmentRef:
    try:
        resolved = safe_resolve(root, base, uri)
    except ManifestError as exc:
        errors.add(fatal(ErrorStage.MANIFEST, asset, str(exc), segment_index=index, path=uri))
        resolved = base / uri
    return SegmentRef(index, resolved, dur_us, start_us, uri=uri)


def parse_dash(path: Path, root: Path | None = None) -> ParsedManifest:
    root = (root or path.parent).resolve()
    errors = ErrorCollector()
    tree = ET.parse(path)
    mpd = tree.getroot()
    total_us = _parse_iso8601_duration(mpd.get("mediaPresentationDuration", "PT0S")) or None
    reps: list[Representation] = []
    base = path.parent
    base_url = mpd.findtext("BaseURL", default="")
    if base_url:
        base = (path.parent / posixpath.dirname(base_url))
    for adaptation in mpd.iter("AdaptationSet"):
        mime = (adaptation.get("mimeType") or adaptation.get("contentType") or "").lower()
        fallback = "audio" if "audio" in mime else "video"
        for element in adaptation.findall("Representation"):
            kind = _codec_kind(element.get("codecs", ""), fallback)
            reps.append(_dash_rep(element, base, root, path, errors, kind, base_url))
    return ParsedManifest("dash", path, root, reps, total_us, errors)


def parse_hls(path: Path, root: Path | None = None) -> ParsedManifest:
    root = (root or path.parent).resolve()
    errors = ErrorCollector()
    reps = _parse_hls_attributes(path, root, errors)
    target = next((r.target_duration_us for r in reps if r.target_duration_us), None)
    return ParsedManifest("hls", path, root, reps, target, errors)


def parse_manifest(path: Path, root: Path | None = None) -> ParsedManifest:
    """Auto-detect HLS vs DASH and parse into the unified representation model."""
    head = path.read_text(errors="replace")[:512].lstrip()
    if head.startswith("<?xml") or head.startswith("<MPD") or path.suffix == ".mpd":
        return parse_dash(path, root)
    return parse_hls(path, root)


def find_manifests(root: Path) -> list[Path]:
    """List entry manifests (m3u8 / mpd), skipping nested media playlists."""
    return sorted(p for p in root.rglob("*") if p.suffix in (".m3u8", ".mpd")
                  and "media" not in p.name.lower().replace("_", "-"))
