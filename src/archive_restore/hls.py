"""HLS master and media playlist parsing."""

from __future__ import annotations

import json
import re
from pathlib import Path

from .errors import ErrorCollector, ErrorStage, fatal, retryable, skippable
from .manifest_common import (
    ManifestError,
    ParsedManifest,
    Representation,
    SegmentRef,
    _codec_kind,
    safe_resolve,
)


def _parse_attrs(line: str) -> dict[str, str]:
    attrs: dict[str, str] = {}
    for match in re.finditer(r'([A-Z0-9-]+)=("([^"]*)"|[^,]+)', line):
        attrs[match.group(1)] = match.group(3) if match.group(3) is not None else match.group(2)
    return attrs


def parse_hls_attributes(path: Path, root: Path, errors: ErrorCollector) -> list[Representation]:
    lines = path.read_text(errors="replace").splitlines()
    if not lines or not lines[0].lstrip().startswith("#EXTM3U"):
        raise ManifestError("not an HLS playlist (missing #EXTM3U)")
    is_master = any(l.startswith("#EXT-X-STREAM-INF") or l.startswith("#EXT-X-MEDIA") for l in lines)
    if not is_master:
        return [hls_media_representation(lines, path, root, errors)]
    reps: list[Representation] = []
    pending: dict[str, str] | None = None
    for raw in lines:
        line = raw.strip()
        if line.startswith("#EXT-X-STREAM-INF"):
            pending = _parse_attrs(line)
        elif line.startswith("#EXT-X-MEDIA"):
            attrs = _parse_attrs(line)
            if attrs.get("TYPE") == "AUDIO" and "URI" in attrs:
                reps.append(load_hls_media(attrs["URI"], path, root, errors, "audio",
                                           attrs.get("NAME", attrs["URI"]),
                                           attrs.get("CODECS", "mp4a")))
        elif line and not line.startswith("#") and pending is not None:
            reps.append(load_hls_media(line, path, root, errors,
                                       _codec_kind(pending.get("CODECS", "")),
                                       pending.get("VIDEO", line), pending.get("CODECS", ""),
                                       int(pending.get("BANDWIDTH", "0"))))
            pending = None
    return reps


def load_hls_media(uri: str, parent: Path, root: Path, errors: ErrorCollector,
                   kind: str, rep_id: str, codec: str, bandwidth: int = 0) -> Representation:
    try:
        media_path = safe_resolve(root, parent.parent, uri)
        lines = media_path.read_text(errors="replace").splitlines()
    except (ManifestError, OSError) as exc:
        errors.add(fatal(ErrorStage.MANIFEST, str(parent), str(exc), path=uri))
        return Representation(rep_id, kind, bandwidth, codec, parent.parent)
    wrap_points: set[int] = set()
    sidecar = media_path.with_name("timeline.json")
    if sidecar.exists():
        meta = json.loads(sidecar.read_text())
        if meta.get("mode") == "wraparound":
            wrap_points = {i for i, s in enumerate(meta.get("start_us", [])) if s == 0 and i > 0}
    return hls_media_representation(lines, media_path, root, errors, rep_id, kind,
                                    codec, bandwidth, wrap_points)


def hls_media_representation(lines: list[str], path: Path, root: Path,
                             errors: ErrorCollector, rep_id: str | None = None,
                             kind: str = "video", codec: str = "",
                             bandwidth: int = 0,
                             wrap_points: set[int] | None = None) -> Representation:
    asset = str(path)
    sequence = 0
    target_us: int | None = None
    pending_dur: float | None = None
    pending_range: tuple[int, int | None] | None = None
    segs: list[SegmentRef] = []
    cursor_us = 0
    sidecar = path.with_name("timeline.json")
    explicit_starts: list[int] = []
    explicit_indices: list[int] = []
    wrap_points = set(wrap_points or set())
    if sidecar.exists():
        meta = json.loads(sidecar.read_text())
        explicit_starts = meta.get("start_us", [])
        explicit_indices = meta.get("indices", [])
        if meta.get("mode") == "wraparound":
            wrap_points = {i for i, s in enumerate(explicit_starts) if s == 0 and i > 0}
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
                errors.add(fatal(ErrorStage.MANIFEST, asset, str(exc),
                                 segment_index=sequence, path=line))
                pending_dur, pending_range = None, None
                sequence += 1
                continue
            dur_us = round(pending_dur * 1_000_000)
            seg_index = len(segs)
            has_explicit_start = bool(explicit_starts and seg_index < len(explicit_starts))
            if has_explicit_start:
                cursor_us = explicit_starts[seg_index]
            if explicit_indices and seg_index < len(explicit_indices):
                sequence = explicit_indices[seg_index]
            offset, length = None, None
            if pending_range is not None:
                length, offset = pending_range
            ref = SegmentRef(sequence, seg_path, dur_us, cursor_us, offset, length, line)
            ref.position = seg_index
            same_range = any(s.start_us == cursor_us and s.duration_us == dur_us for s in segs)
            shuffled = bool(explicit_indices and seg_index < len(explicit_indices)
                            and explicit_indices[seg_index] != seg_index)
            prev_start = segs[-1].start_us if segs else 0
            goes_backwards = bool(segs) and cursor_us < prev_start
            ref.disorder = shuffled and goes_backwards and not same_range
            segs.append(ref)
            if not (explicit_indices and seg_index < len(explicit_indices)):
                sequence += 1
            if not has_explicit_start:
                cursor_us += dur_us
            pending_dur, pending_range = None, None
    if pending_dur is not None:
        errors.add(retryable(ErrorStage.MANIFEST, asset,
                             "truncated playlist: EXTINF without segment URI",
                             segment_index=sequence))
    if not any(l.strip() == "#EXT-X-ENDLIST" for l in lines):
        errors.add(retryable(ErrorStage.MANIFEST, asset,
                             "truncated playlist: missing #EXT-X-ENDLIST"))
    return Representation(rep_id or path.stem, kind, bandwidth, codec, path.parent,
                          segs, target_us, wrap_points)


def parse_hls(path: Path, root: Path) -> ParsedManifest:
    errors = ErrorCollector()
    reps = parse_hls_attributes(path, root, errors)
    target = next((r.target_duration_us for r in reps if r.target_duration_us), None)
    return ParsedManifest("hls", path, root, reps, target, errors)
