"""Local HLS media playlist and DASH MPD parsing.

Only explicitly named local manifests are accepted.  Every segment
reference is resolved underneath the manifest directory and path
traversal (``..`` escapes, absolute URIs) is rejected before any file is
opened.  The parser validates sequence continuity, segment durations and
the manifest-declared bitrate, and produces an immutable segment plan
plus a digest used by checkpoint restart verification.
"""

from __future__ import annotations

import hashlib
import os
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

from .config import Limits, MAX_MANIFEST_BYTES
from .errors import ErrorCode, FatalError, RecoverableError

HLS = "hls"
DASH = "dash"


@dataclass(frozen=True, slots=True)
class Segment:
    """One planned media segment."""

    sequence: int
    path: Path
    duration: Fraction          # declared duration, exact seconds
    declared_bitrate: int       # bps from the manifest (0 if unknown)
    byte_size: int


@dataclass(slots=True)
class ManifestPlan:
    """Normalized view of a manifest, independent of its original format."""

    format: str
    manifest_path: Path
    target_duration: Fraction
    declared_bitrate: int
    segments: list[Segment] = field(default_factory=list)
    manifest_digest: str = ""
    input_digest: str = ""
    continuity_errors: list = field(default_factory=list)

    @property
    def total_duration(self) -> Fraction:
        return sum((s.duration for s in self.segments), Fraction(0))

    def digest_inputs(self) -> str:
        """Hash manifest bytes and every referenced segment's bytes.

        Missing segments contribute only their name and an explicit
        ``<missing>`` marker, so adding or restoring a file still changes
        the digest without requiring the file to be readable.
        """
        h = hashlib.sha256()
        h.update(self.manifest_digest.encode())
        for seg in self.segments:
            h.update(seg.path.name.encode())
            if not seg.path.is_file():
                h.update(b"<missing>")
                continue
            with open(seg.path, "rb") as fh:
                for chunk in iter(lambda: fh.read(1 << 20), b""):
                    h.update(chunk)
        return h.hexdigest()


def _safe_resolve(manifest_dir: Path, uri: str, limits: Limits) -> Path:
    """Resolve a manifest-relative URI, rejecting traversal/abs paths."""
    if not uri or uri.startswith(("http://", "https://", "file://")):
        raise FatalError(
            ErrorCode.PATH_TRAVERSAL,
            f"remote/URI scheme references are not allowed: {uri!r}",
        )
    candidate = Path(uri)
    if candidate.is_absolute() or candidate.drive:
        raise FatalError(
            ErrorCode.PATH_TRAVERSAL, f"absolute segment path rejected: {uri!r}"
        )
    # Normalize without requiring the file to exist yet.
    parts: list[str] = []
    depth = 0
    for part in candidate.parts:
        if part in ("", "."):
            continue
        if part == "..":
            if not parts:
                raise FatalError(
                    ErrorCode.PATH_TRAVERSAL,
                    f"segment escapes manifest directory: {uri!r}",
                )
            parts.pop()
            depth -= 1
            continue
        parts.append(part)
        depth += 1
        if depth > limits.max_path_depth:
            raise FatalError(
                ErrorCode.LIMIT_EXCEEDED,
                f"path depth exceeds {limits.max_path_depth}: {uri!r}",
            )
    return (manifest_dir / Path(*parts)).resolve()


def _stat_segment(
    sequence: int, path: Path, duration: Fraction, bitrate: int,
    limits: Limits,
) -> Segment:
    # Missing or zero-length files are kept as zero-size placeholder
    # segments so the pipeline can apply fail / gap-fill / skip policy;
    # only traversal and hard size limits abort at parse time.
    if not path.is_file() or path.stat().st_size == 0:
        return Segment(sequence, path, duration, bitrate, 0)
    size = path.stat().st_size
    if size > limits.max_segment_bytes:
        raise FatalError(
            ErrorCode.LIMIT_EXCEEDED,
            f"segment {path.name} is {size} bytes, limit {limits.max_segment_bytes}",
            segment=path.name,
        )
    return Segment(sequence, path, duration, bitrate, size)


def _parse_hls(manifest_path: Path, text: str, limits: Limits) -> ManifestPlan:
    lines = [ln.strip() for ln in text.splitlines() if ln.strip()]
    if not lines or lines[0] != "#EXTM3U":
        raise FatalError(
            ErrorCode.MANIFEST_PARSE, "missing #EXTM3U header",
            segment=manifest_path.name,
        )
    target = Fraction(0)
    sequence = 0
    bitrate = 0
    pending_duration: Fraction | None = None
    raw_entries: list[tuple[int, str, Fraction, int]] = []
    for line in lines:
        if line.startswith("#EXT-X-TARGETDURATION:"):
            target = Fraction(line.split(":", 1)[1].strip())
        elif line.startswith("#EXT-X-MEDIA-SEQUENCE:"):
            sequence = int(line.split(":", 1)[1].strip())
        elif line.startswith("#EXT-X-STREAM-INF:"):
            raise FatalError(
                ErrorCode.MANIFEST_PARSE,
                "master playlists are not supported; pass a media playlist",
            )
        elif line.startswith("#X-DECLARED-BITRATE:"):
            bitrate = int(line.split(":", 1)[1].strip())
        elif line.startswith("#EXTINF:"):
            value = line.split(":", 1)[1].split(",", 1)[0].strip()
            pending_duration = Fraction(value)
        elif line.startswith("#"):
            continue
        else:
            if pending_duration is None:
                raise FatalError(
                    ErrorCode.MANIFEST_PARSE,
                    f"segment URI without EXTINF: {line!r}",
                )
            raw_entries.append((sequence, line, pending_duration, bitrate))
            sequence += 1
            pending_duration = None
    if target == 0:
        target = Fraction(2)
    if len(raw_entries) > limits.max_segments:
        raise FatalError(
            ErrorCode.LIMIT_EXCEEDED,
            f"{len(raw_entries)} segments exceed limit {limits.max_segments}",
        )
    manifest_dir = manifest_path.parent
    plan = ManifestPlan(HLS, manifest_path, target, bitrate)
    for idx, uri, dur, br in raw_entries:
        path = _safe_resolve(manifest_dir, uri, limits)
        plan.segments.append(_stat_segment(idx, path, dur, br, limits))
    return plan


def validate_continuity(plan: ManifestPlan) -> list[RecoverableError]:
    """Check sequence numbers and per-segment duration declarations."""
    errors: list[RecoverableError] = []
    expected = plan.segments[0].sequence if plan.segments else 0
    for seg in plan.segments:
        if seg.sequence != expected:
            errors.append(RecoverableError(
                ErrorCode.MANIFEST_ALIGNMENT,
                f"sequence gap: expected {expected}, got {seg.sequence}",
                offset=0, segment=seg.path.name,
            ))
        expected = seg.sequence + 1
        if plan.target_duration and seg.duration > plan.target_duration * 2:
            errors.append(RecoverableError(
                ErrorCode.MANIFEST_ALIGNMENT,
                f"segment duration {seg.duration}s exceeds 2x target "
                f"{plan.target_duration}s",
                segment=seg.path.name,
            ))
    return errors


def parse_manifest(path: str | os.PathLike, limits: Limits | None = None) -> ManifestPlan:
    """Parse a local HLS playlist or DASH MPD into a segment plan."""
    limits = limits or Limits()
    manifest_path = Path(path).resolve()
    if not manifest_path.is_file():
        raise FatalError(
            ErrorCode.MANIFEST_PARSE, f"manifest not found: {manifest_path}"
        )
    if manifest_path.stat().st_size > MAX_MANIFEST_BYTES:
        raise FatalError(
            ErrorCode.LIMIT_EXCEEDED, "manifest file exceeds size limit"
        )
    text = manifest_path.read_text(encoding="utf-8")
    digest = hashlib.sha256(text.encode("utf-8")).hexdigest()
    suffix = manifest_path.suffix.lower()
    if suffix in (".m3u8", ".m3u") or text.lstrip().startswith("#EXTM3U"):
        plan = _parse_hls(manifest_path, text, limits)
    elif suffix == ".mpd" or "<MPD" in text:
        from .dash_manifest import parse_dash
        plan = parse_dash(manifest_path, text, limits)
    else:
        raise FatalError(
            ErrorCode.MANIFEST_PARSE,
            f"unrecognized manifest format: {manifest_path.name}",
        )
    plan.manifest_digest = digest
    plan.continuity_errors = validate_continuity(plan)
    plan.input_digest = plan.digest_inputs()
    return plan
