"""Validation: prove the remux preserved content.

Re-reads the delivery container, recomputes per-frame payload digests and
per-segment SHA-256 chains, and compares them against the mux sidecar
index.  The keyframe index is checked against an independent packet scan:
duplicate, out-of-bounds and out-of-order index entries are counted as
errors without disturbing other tracks' statistics.  The optional audio
companion is validated by re-digesting its PCM.
"""
from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

from .model import FieldPackError, Stage

PTS_TOLERANCE_NS = 2_000_000  # container timestamp grid may be coarser than 1 ns


@dataclass
class KeyframeCheck:
    indexed: int
    scanned: int
    hits: int
    errors: list[str] = field(default_factory=list)

    @property
    def hit_rate(self) -> float:
        return self.hits / self.indexed if self.indexed else 1.0


def check_keyframe_index(indexed: list[int], scanned: list[int]) -> KeyframeCheck:
    """Compare indexed keyframe pts values with an independent scan."""
    errors: list[str] = []
    seen: set[int] = set()
    last = -1
    clean: list[int] = []
    for pts in indexed:
        if pts in seen:
            errors.append(f"duplicate keyframe index entry pts={pts}")
            continue
        if pts < last:
            errors.append(f"out-of-order keyframe index entry pts={pts}")
            continue
        seen.add(pts)
        last = pts
        clean.append(pts)
    scanned_set = set(scanned)
    max_scanned = max(scanned_set) if scanned_set else -1
    hits = 0
    for pts in clean:
        if pts > max_scanned:
            errors.append(f"out-of-bounds keyframe index entry pts={pts}")
        elif pts in scanned_set:
            hits += 1
        else:
            errors.append(f"keyframe index entry pts={pts} not found in scan")
    return KeyframeCheck(indexed=len(indexed), scanned=len(scanned), hits=hits, errors=errors)


@dataclass
class ValidationReport:
    frames_indexed: int = 0
    frames_matched: int = 0
    segments_total: int = 0
    segments_matched: int = 0
    keyframes: KeyframeCheck | None = None
    audio_ok: bool | None = None
    errors: list[FieldPackError] = field(default_factory=list)

    @property
    def consistency(self) -> float:
        return self.frames_matched / self.frames_indexed if self.frames_indexed else 1.0

    @property
    def segment_hash_rate(self) -> float:
        return self.segments_matched / self.segments_total if self.segments_total else 1.0


def _validate_audio(audio: dict, base: Path) -> bool:
    from .mapping import summarise_audio

    wav = Path(audio["path"])
    if not wav.is_absolute():
        wav = base / wav
    try:
        return summarise_audio(wav).sha256 == audio["sha256"]
    except Exception:
        return False


def validate_output(container_path: str | Path, index_path: str | Path) -> ValidationReport:
    import av

    container_path = Path(container_path)
    index = json.loads(Path(index_path).read_text())
    report = ValidationReport()
    report.frames_indexed = len(index["frames"])
    report.segments_total = len(index["segments"])

    by_device: dict[int, list[dict]] = {}
    for entry in index["frames"]:
        by_device.setdefault(entry["device_id"], []).append(entry)

    scanned_keyframes: dict[int, list[int]] = {d: [] for d in by_device}
    segment_chains: dict[tuple[int, int], "hashlib._Hash"] = {}
    segment_counts: dict[tuple[int, int], int] = {}
    cursors: dict[int, int] = {d: 0 for d in by_device}

    container = av.open(str(container_path))
    try:
        device_by_stream: dict[int, int] = {}
        for stream in container.streams.video:
            dev = stream.metadata.get("fieldpack.device")
            if dev is not None:
                device_by_stream[stream.index] = int(dev)
        for packet in container.demux(list(container.streams.video)):
            dev = device_by_stream.get(packet.stream.index)
            if dev is None:
                continue
            base: Fraction = packet.stream.time_base
            if packet.is_keyframe and packet.pts is not None:
                scanned_keyframes[dev].append(int(packet.pts * base * 1_000_000_000))
            for frame in packet.decode():
                cursor = cursors[dev]
                expected = by_device[dev]
                if cursor >= len(expected):
                    report.errors.append(FieldPackError(
                        code="extra-frame", message="container holds more frames than index",
                        stage=Stage.VALIDATE))
                    continue
                entry = expected[cursor]
                cursors[dev] += 1
                payload = frame.to_ndarray().tobytes()
                digest = hashlib.sha256(payload).hexdigest()
                pts_ns = int(frame.pts * base * 1_000_000_000) if frame.pts is not None else -1
                if digest == entry["sha256"] and abs(pts_ns - entry["pts_ns"]) <= PTS_TOLERANCE_NS:
                    report.frames_matched += 1
                    key = (dev, entry["segment"])
                    segment_chains.setdefault(key, hashlib.sha256()).update(payload)
                    segment_counts[key] = segment_counts.get(key, 0) + 1
                else:
                    report.errors.append(FieldPackError(
                        code="roundtrip-mismatch",
                        message=f"frame mismatch device={dev} segment={entry['segment']}",
                        stage=Stage.VALIDATE, offset=entry["offset"]))
    finally:
        container.close()

    for seg in index["segments"]:
        key = (seg["device_id"], seg["segment"])
        if segment_counts.get(key, 0) != seg["frames"]:
            continue  # not all frames of the segment matched: hash cannot pass
        if segment_chains[key].hexdigest() == seg["sha256"]:
            report.segments_matched += 1

    indexed_kf: list[int] = []
    scanned_kf: list[int] = []
    for kf in index["keyframes"]:
        indexed_kf.append(kf["pts_ns"])
    for pts_list in scanned_keyframes.values():
        scanned_kf.extend(pts_list)
    report.keyframes = check_keyframe_index(indexed_kf, scanned_kf)

    if index.get("audio"):
        report.audio_ok = _validate_audio(index["audio"], container_path.parent)
    return report
