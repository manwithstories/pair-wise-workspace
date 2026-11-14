"""Vectorized timestamp conversion and strict DTS normalization."""

from __future__ import annotations

from dataclasses import dataclass, field

import numpy as np

from mkvtomp4.mkv.tracks import BlockSample, MediaIndex, TrackInfo


class TimestampNormalizeError(ValueError):
    """Raised when samples cannot be converted to a monotonic timeline."""


@dataclass
class NormalizedTrack:
    track: TrackInfo
    pts: np.ndarray
    dts: np.ndarray
    durations: np.ndarray
    keyframes: np.ndarray
    source_offsets: np.ndarray
    sizes: np.ndarray
    kept_source_indices: np.ndarray
    dropped_dup_dts: int = 0
    reordered_pts: int = 0

    @property
    def sample_count(self) -> int:
        return int(self.pts.size)

    @property
    def first_pts(self) -> int:
        return int(self.pts[0]) if self.pts.size else 0

    @property
    def last_pts(self) -> int:
        return int(self.pts[-1]) if self.pts.size else 0

    @property
    def first_dts(self) -> int:
        return int(self.dts[0]) if self.dts.size else 0

    @property
    def last_dts(self) -> int:
        return int(self.dts[-1]) if self.dts.size else 0

    @property
    def keyframe_count(self) -> int:
        return int(np.count_nonzero(self.keyframes))


@dataclass
class NormalizedMedia:
    source: MediaIndex
    tracks: list[NormalizedTrack]
    negative_shift_ms: float
    warnings: list[dict[str, object]] = field(default_factory=list)

    @property
    def dropped_dup_dts(self) -> int:
        return sum(track.dropped_dup_dts for track in self.tracks)

    @property
    def reordered_pts(self) -> int:
        return sum(track.reordered_pts for track in self.tracks)

    def timestamp_summary(self) -> dict[str, object]:
        pts_first = min((track.first_pts for track in self.tracks), default=0)
        pts_last = max((track.last_pts for track in self.tracks), default=0)
        dts_first = min((track.first_dts for track in self.tracks), default=0)
        dts_last = max((track.last_dts for track in self.tracks), default=0)
        return {
            "first_pts": pts_first,
            "last_pts": pts_last,
            "first_dts": dts_first,
            "last_dts": dts_last,
            "negative_shift_ms": round(self.negative_shift_ms, 6),
            "dropped_dup_dts": self.dropped_dup_dts,
            "reordered_pts": self.reordered_pts,
        }


def _to_units(samples: list[BlockSample], timescale: int, *, field_name: str) -> np.ndarray:
    values = np.fromiter(
        (getattr(sample, field_name) for sample in samples),
        dtype=np.float64,
        count=len(samples),
    )
    return np.rint(values * timescale / 1000.0).astype(np.int64)


def _drop_duplicate_dts(dts: np.ndarray) -> tuple[np.ndarray, int]:
    if dts.size < 2:
        return dts, 0
    keep = np.empty(dts.size, dtype=bool)
    keep[0] = True
    keep[1:] = dts[1:] != dts[:-1]
    dropped = int(dts.size - np.count_nonzero(keep))
    return keep, dropped


def _strict_monotonic(dts: np.ndarray) -> np.ndarray:
    if dts.size < 2 or np.all(dts[1:] > dts[:-1]):
        return dts
    result = dts.copy()
    for index in range(1, result.size):
        if result[index] <= result[index - 1]:
            if result[index - 1] == np.iinfo(np.int64).max:
                raise TimestampNormalizeError("DTS overflow while making monotonic")
            result[index] = result[index - 1] + 1
    return result


def _normalize_one(
    track: TrackInfo,
    samples: list[BlockSample],
    shift_units: int,
) -> tuple[NormalizedTrack, list[dict[str, object]]]:
    if not samples:
        raise TimestampNormalizeError(f"track {track.mkv_track} has no samples")
    pts = _to_units(samples, track.timescale, field_name="pts_ms") + shift_units
    source_dts = _to_units(samples, track.timescale, field_name="dts_ms") + shift_units
    frame = track.frame_duration_units
    warnings: list[dict[str, object]] = []
    regressions = pts[1:] < pts[:-1] - frame
    reordered = int(np.count_nonzero(regressions))
    if reordered:
        order = np.argsort(pts, kind="stable")
        pts = pts[order]
        source_dts = pts.copy()
        warnings.append(
            {
                "kind": "pts_reorder",
                "track": track.mkv_track,
                "count": reordered,
                "message": "presentation timestamps regressed by more than one frame",
            }
        )
        samples = [samples[int(index)] for index in order]
    keep, dropped = _drop_duplicate_dts(source_dts)
    if not np.all(keep):
        pts = pts[keep]
        source_dts = source_dts[keep]
        samples = [sample for sample, selected in zip(samples, keep) if selected]
    dts = _strict_monotonic(source_dts)
    if dts.size == 0:
        raise TimestampNormalizeError(f"track {track.mkv_track} lost all samples")
    durations = np.empty(dts.size, dtype=np.int64)
    if dts.size > 1:
        durations[:-1] = np.diff(dts)
    durations[-1] = max(frame, 1)
    offsets = np.fromiter(
        (sample.source_offset for sample in samples), dtype=np.int64, count=len(samples)
    )
    sizes = np.fromiter((sample.size for sample in samples), dtype=np.int64, count=len(samples))
    keyframes = np.fromiter(
        (sample.keyframe for sample in samples), dtype=bool, count=len(samples)
    )
    indices = np.fromiter(
        (sample.sequence for sample in samples), dtype=np.int64, count=len(samples)
    )
    return (
        NormalizedTrack(
            track=track,
            pts=pts,
            dts=dts,
            durations=durations,
            keyframes=keyframes,
            source_offsets=offsets,
            sizes=sizes,
            kept_source_indices=indices,
            dropped_dup_dts=dropped,
            reordered_pts=reordered,
        ),
        warnings,
    )


def normalize_media(media: MediaIndex) -> NormalizedMedia:
    all_values = [
        _to_units(samples, track.timescale, field_name=field)
        for track in media.tracks
        for samples in (media.samples.get(track.mkv_track, []),)
        for field in ("pts_ms", "dts_ms")
    ]
    nonempty = [values for values in all_values if values.size]
    if not nonempty:
        raise TimestampNormalizeError("no timestamps to normalize")
    minimum = min(int(values.min()) for values in nonempty)
    shift_units = -minimum if minimum < 0 else 0
    normalized: list[NormalizedTrack] = []
    warnings: list[dict[str, object]] = []
    for track in media.tracks:
        result, track_warnings = _normalize_one(
            track, media.samples.get(track.mkv_track, []), shift_units
        )
        normalized.append(result)
        warnings.extend(track_warnings)
    timescale = normalized[0].track.timescale if normalized else 1000
    return NormalizedMedia(
        source=media,
        tracks=normalized,
        negative_shift_ms=shift_units * 1000.0 / timescale,
        warnings=warnings,
    )
