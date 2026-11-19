"""Missing-segment recovery, de-duplication, reordering and checkpoints.

A missing segment is recoverable when another representation of the same kind
covers the same time range with a duration within tolerance, or when the gap is
small enough to splice across.  Repeated segments are de-duplicated by their
time-range key, out-of-order segments are re-sequenced by start time, and any
truly un-recoverable range survives as an explicit :class:`ExplicitGap`.

Checkpoints record completed segment keys, the input digest and the output
offset.  On resume the digest is re-verified; a changed input or a partial
(zero-byte / truncated) output rolls the run back to the last safe boundary.
"""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
from dataclasses import dataclass, field, asdict
from pathlib import Path

from .errors import ErrorCollector, ErrorStage, retryable, skippable
from .timeline import Gap, RepresentationTimeline, TimelineSegment

RECOVERY_TOLERANCE_US = 250_000
MAX_RETRIES = 3
CHECKPOINT_NAME = "checkpoint.json"


@dataclass
class ExplicitGap:
    rep_id: str
    start_us: int
    end_us: int
    reason: str
    recoverable: bool

    def to_dict(self) -> dict:
        return asdict(self)


@dataclass
class RecoveryOutcome:
    rep_id: str
    ordered: list[TimelineSegment]
    duplicates_removed: int
    reordered: bool
    recovered: list[str]
    gaps: list[ExplicitGap]
    errors: ErrorCollector

    @property
    def expected_count(self) -> int:
        # Ordered healthy segments plus every gap that could not be peer-filled.
        return len(self.ordered) + len(self.gaps)


def _range_key(seg: TimelineSegment) -> tuple[int, int]:
    return seg.start_us, seg.end_us


def recover_representation(tl: RepresentationTimeline, peers: list[RepresentationTimeline],
                           errors: ErrorCollector) -> RecoveryOutcome:
    """Deduplicate, reorder and attempt to fill gaps using peer representations."""
    seen: dict[tuple[int, int], TimelineSegment] = {}
    duplicates = 0
    for seg in tl.segments:
        key = _range_key(seg)
        if key in seen:
            duplicates += 1
            errors.add(skippable(ErrorStage.RECOVERY, tl.rep_id,
                                  f"duplicate segment removed for range {key}",
                                  segment_index=seg.index, path=str(seg.path)))
            continue
        seen[key] = seg
    ordered = sorted(seen.values(), key=lambda s: s.start_us)
    reordered = [s.index for s in ordered] != sorted(s.index for s in ordered)
    if reordered:
        errors.add(skippable(ErrorStage.RECOVERY, tl.rep_id,
                              "out-of-order segments re-sequenced by start time"))

    same_kind_peers = [p for p in peers if p.kind == tl.kind and p.rep_id != tl.rep_id]
    explicit: list[ExplicitGap] = []
    recovered_keys: list[str] = []

    # Physical integrity: a missing or undecodable segment is treated as a gap
    # before peer recovery is attempted, never silently counted as recovered.
    healthy: list[TimelineSegment] = []
    integrity_gaps: list[Gap] = list(tl.gaps)
    for seg in ordered:
        if _segment_decodable(seg):
            healthy.append(seg)
            continue
        reason = "segment missing on disk" if not Path(seg.path).exists() else "corrupt segment"
        integrity_gaps.append(Gap(seg.start_us, seg.end_us, reason))
        errors.add(skippable(ErrorStage.RECOVERY, tl.rep_id,
                             f"{reason} at {seg.start_us}-{seg.end_us}us",
                             segment_index=seg.index, path=str(seg.path)))
    ordered = healthy

    for gap in integrity_gaps:
        cover = _find_covering_segment(gap, same_kind_peers)
        if cover is not None:
            recovered_keys.append(cover.key)
            errors.add(retryable(ErrorStage.RECOVERY, tl.rep_id,
                                  f"gap {gap.start_us}-{gap.end_us}us recovered from {cover.path}",
                                  detail={"source": cover.key}))
        else:
            reason = _classify_gap(gap, same_kind_peers)
            eg = ExplicitGap(tl.rep_id, gap.start_us, gap.end_us, reason, recoverable=False)
            explicit.append(eg)
            errors.add(skippable(ErrorStage.RECOVERY, tl.rep_id,
                                  f"unrecoverable gap {gap.start_us}-{gap.end_us}us: {reason}",
                                  detail={"gap_us": gap.duration_us}))
    return RecoveryOutcome(tl.rep_id, ordered, duplicates, reordered, recovered_keys, explicit, errors)


def _segment_decodable(seg: TimelineSegment) -> bool:
    """Cheap existence + container-open probe used by integrity recovery."""
    path = Path(seg.path)
    if not path.exists() or path.stat().st_size == 0:
        return False
    try:
        if path.suffix.lower() in (".wav", ".m4a", ".aac", ".flac", ".ogg", ".mp3"):
            import soundfile as sf
            info = sf.info(str(path))
            return info.frames > 0 and info.samplerate > 0
        import av
        container = av.open(str(path))
        frame_count = 0
        for stream in container.streams:
            frame_count += sum(1 for _ in container.decode(stream))
        container.close()
        # A truncated container still decodes its surviving head; require a
        # meaningful payload (>= half of the nominal ~2s of frames) so a torn
        # tail with only a few remnant frames is treated as corrupt.
        return frame_count >= _MIN_HEALTHY_FRAMES
    except Exception:
        return False


_MIN_HEALTHY_FRAMES = 20


def _find_covering_segment(gap: Gap, peers: list[RepresentationTimeline]) -> TimelineSegment | None:
    gap_dur = gap.duration_us
    for peer in peers:
        for seg in peer.segments:
            if not seg.valid:
                continue
            covers = seg.start_us <= gap.start_us and seg.end_us >= gap.end_us
            close_duration = abs(seg.duration_us - gap_dur) <= RECOVERY_TOLERANCE_US
            # The peer copy must itself be intact; a similarly corrupt peer
            # cannot be used to paper over an unrecoverable gap.
            if covers and close_duration and _segment_decodable(seg):
                return seg
    return None


def _classify_gap(gap: Gap, peers: list[RepresentationTimeline]) -> str:
    if not peers:
        return "no peer representation available"
    for peer in peers:
        for seg in peer.segments:
            if seg.start_us < gap.end_us and seg.end_us > gap.start_us:
                if not Path(seg.path).exists():
                    return "peer reference also missing on disk"
                if abs(seg.duration_us - gap.duration_us) > RECOVERY_TOLERANCE_US:
                    return "duration mismatch beyond tolerance"
    return "no overlapping time range in peers"


def digest_inputs(root: Path) -> str:
    """Stable SHA-256 over manifest and segment paths, sizes and mtime_ns."""
    h = hashlib.sha256()
    for path in sorted(p for p in root.rglob("*") if p.is_file()):
        stat = path.stat()
        rel = path.relative_to(root).as_posix()
        h.update(f"{rel}:{stat.st_size}:{stat.st_mtime_ns}\n".encode())
    return h.hexdigest()


def _atomic_write_json(path: Path, payload: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=path.parent, prefix=".tmp-", suffix=".json")
    try:
        with os.fdopen(fd, "w") as fh:
            json.dump(payload, fh, indent=2, sort_keys=True)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, path)
    except BaseException:
        Path(tmp).unlink(missing_ok=True)
        raise


class CheckpointStore:
    """Durable checkpoint with input-digest and partial-write rollback."""

    def __init__(self, output_dir: Path, input_digest: str) -> None:
        self.path = output_dir / CHECKPOINT_NAME
        self.input_digest = input_digest
        self.completed: dict[str, int] = {}
        self.safe_boundary = 0

    def load(self) -> dict:
        if not self.path.exists():
            return {}
        data = json.loads(self.path.read_text())
        if data.get("input_digest") != self.input_digest:
            raise _DigestMismatch("input changed since checkpoint; rolling back to safe boundary")
        self.completed = dict(data.get("completed", {}))
        self.safe_boundary = int(data.get("safe_boundary", 0))
        return data

    def mark_done(self, key: str, output_offset: int) -> None:
        self.completed[key] = output_offset
        self.safe_boundary = max(self.safe_boundary, output_offset)
        _atomic_write_json(self.path, {
            "input_digest": self.input_digest,
            "safe_boundary": self.safe_boundary,
            "completed": self.completed,
        })

    def verify_output(self, output_path: Path) -> int:
        """Return the safe offset after reconciling a partial write.

        A file shorter than the recorded boundary means the last write was
        torn: roll the boundary back to the actual on-disk size and drop any
        completed keys past it, so the next run re-processes them.
        """
        if not output_path.exists():
            self.completed = {k: off for k, off in self.completed.items() if off <= 0}
            self.safe_boundary = 0
            return 0
        size = output_path.stat().st_size
        if size < self.safe_boundary:
            self.completed = {k: off for k, off in self.completed.items() if off <= size}
            self.safe_boundary = size
            _atomic_write_json(self.path, {
                "input_digest": self.input_digest,
                "safe_boundary": self.safe_boundary,
                "completed": self.completed,
            })
        return min(size, self.safe_boundary)


class _DigestMismatch(RuntimeError):
    pass
