from __future__ import annotations

import json
import os
from dataclasses import dataclass, field
from pathlib import Path

from .errors import EXIT_BLOCKING, EXIT_IO, MediaError
from .probe import MediaAsset, probe_file
from .isobmff import TrackInfo


SUPPORTED_SUFFIXES = {".mp4", ".mov", ".m4v", ".wav"}
BLOCKING = "blocking"
INFO = "info"


def _issue(
    code: str,
    severity: str,
    message: str,
    *,
    track_id: int | None = None,
    delta_ms: int | None = None,
    limit_ms: int | None = None,
    box_path: str = "",
    offset: int = 0,
    repair_advice: str = "",
) -> dict:
    return {
        "code": code,
        "severity": severity,
        "message": message,
        "track_id": track_id,
        "delta_ms": delta_ms,
        "limit_ms": limit_ms,
        "box_path": box_path,
        "offset": offset,
        "repair_advice": repair_advice,
    }


@dataclass
class CheckedAsset:
    path: str
    asset: MediaAsset | None
    issues: list[dict] = field(default_factory=list)

    @property
    def blocking_count(self) -> int:
        return sum(1 for issue in self.issues if issue["severity"] == BLOCKING)

    @property
    def status(self) -> str:
        return "rejected" if self.blocking_count else "passed"

    def to_dict(self) -> dict:
        result = {
            "path": self.path,
            "status": self.status,
            "sha256": self.asset.sha256 if self.asset else None,
            "track_count": self.asset.track_count if self.asset else 0,
            "issues": self.issues,
        }
        return result


@dataclass
class CheckReport:
    profile_path: str
    profile: dict
    tolerance_ms: int
    assets: list[CheckedAsset]

    @property
    def passed(self) -> int:
        return sum(1 for item in self.assets if item.status == "passed")

    @property
    def rejected(self) -> int:
        return sum(1 for item in self.assets if item.status == "rejected")

    @property
    def issue_count(self) -> int:
        return sum(len(item.issues) for item in self.assets)

    @property
    def blocking_issues(self) -> int:
        return sum(item.blocking_count for item in self.assets)

    def flat_issues(self) -> list[dict]:
        flattened = []
        for item in self.assets:
            for issue in item.issues:
                flattened.append({"asset": item.path, **issue})
        return flattened

    def to_dict(self) -> dict:
        return {
            "profile": self.profile_path,
            "profile_name": self.profile.get("name", ""),
            "tolerance_ms": self.tolerance_ms,
            "asset_count": len(self.assets),
            "passed": self.passed,
            "rejected": self.rejected,
            "issue_count": self.issue_count,
            "blocking_issue_count": self.blocking_issues,
            "assets": [item.to_dict() for item in self.assets],
            "issues": self.flat_issues(),
        }


def load_profile(path: str | os.PathLike[str]) -> dict:
    try:
        with open(path, "r", encoding="utf-8") as fh:
            profile = json.load(fh)
    except PermissionError as exc:
        raise MediaError("PERMISSION_DENIED", str(exc), path=str(path), exit_code=EXIT_IO) from exc
    except OSError as exc:
        raise MediaError("IO_ERROR", str(exc), path=str(path), exit_code=EXIT_IO) from exc
    except json.JSONDecodeError as exc:
        raise MediaError("PROFILE_INVALID", str(exc), path=str(path), offset=exc.pos, exit_code=EXIT_IO) from exc
    if not isinstance(profile, dict):
        raise MediaError("PROFILE_INVALID", "profile root must be an object", path=str(path), exit_code=EXIT_IO)
    return profile


def discover_media(path: str | os.PathLike[str]) -> list[str]:
    source = Path(path)
    if source.is_file():
        return [os.fspath(source)]
    if not source.exists():
        raise MediaError("IO_ERROR", "input path does not exist", path=os.fspath(source), exit_code=EXIT_IO)
    if not source.is_dir():
        raise MediaError("IO_ERROR", "input path is not a file or directory", path=os.fspath(source), exit_code=EXIT_IO)
    try:
        files = [
            os.fspath(item)
            for item in source.rglob("*")
            if item.is_file() and item.suffix.lower() in SUPPORTED_SUFFIXES
        ]
    except PermissionError as exc:
        raise MediaError("PERMISSION_DENIED", str(exc), path=os.fspath(source), exit_code=EXIT_IO) from exc
    return sorted(files)


def _expected_values(value) -> list | None:
    if value is None:
        return None
    return value if isinstance(value, list) else [value]


def _track_drift_issues(asset: MediaAsset, tolerance_ms: int) -> list[dict]:
    tracks = [track for track in asset.tracks if track.handler_type != "tmcd" and not track.fragment_error]
    if len(tracks) < 2:
        return []
    video_tracks = [track for track in tracks if track.handler == "video" and track.duration_ms > 0]
    reference = video_tracks[0] if video_tracks else max(tracks, key=lambda item: item.duration_ms)
    issues = []
    for track in tracks:
        if track is reference or reference.duration_ms <= 0 or track.duration_ms <= 0:
            continue
        delta = track.duration_ms - reference.duration_ms
        if abs(delta) <= tolerance_ms:
            continue
        box_path = track.duration_box_path or f"moov/trak[{track.track_id}]/mdia/mdhd"
        issues.append(
            _issue(
                "DUR_DRIFT",
                BLOCKING,
                f"track {track.track_id} differs from reference by {delta} ms",
                track_id=track.track_id,
                delta_ms=delta,
                limit_ms=tolerance_ms,
                box_path=box_path,
                offset=track.duration_box_offset,
                repair_advice=(
                    f"Adjust track {track.track_id} timing at {box_path} "
                    f"(offset {track.duration_box_offset}) to within ±{tolerance_ms} ms of the reference."
                ),
            )
        )
    return issues


def _duplicate_id_issues(asset: MediaAsset) -> list[dict]:
    seen: dict[int, TrackInfo] = {}
    duplicates: set[int] = set()
    for track in asset.tracks:
        if track.track_id in seen:
            duplicates.add(track.track_id)
        else:
            seen[track.track_id] = track
    issues = []
    for track_id in sorted(duplicates):
        track = seen[track_id]
        issues.append(
            _issue(
                "DUPLICATE_TRACK_ID",
                BLOCKING,
                f"track_id {track_id} is used by more than one track",
                track_id=track_id,
                box_path="moov/trak/tkhd",
                offset=0,
                repair_advice=(
                    f"Assign a unique track_id in tkhd for moov/trak (near offset {track.duration_box_offset}); "
                    "keep downstream references consistent."
                ),
            )
        )
    return issues


def _structural_issues(asset: MediaAsset) -> list[dict]:
    issues = []
    for track in asset.tracks:
        if track.stsd_count != track.stsd_actual:
            issues.append(
                _issue(
                    "STSD_COUNT_MISMATCH",
                    BLOCKING,
                    f"stsd declares {track.stsd_count} entries but contains {track.stsd_actual}",
                    track_id=track.track_id,
                    box_path=track.stsd_box_path,
                    offset=track.stsd_box_offset,
                    repair_advice=(
                        f"Rewrite stsd entry_count at {track.stsd_box_path} "
                        f"(offset {track.stsd_box_offset}) to match the sample entries."
                    ),
                )
            )
        if track.invalid_chunk_offsets:
            issues.append(
                _issue(
                    "CHUNK_OFFSET_OUT_OF_RANGE",
                    BLOCKING,
                    f"{track.invalid_chunk_offsets} chunk offset(s) point outside the file",
                    track_id=track.track_id,
                    box_path=track.chunk_box_path,
                    offset=track.chunk_box_offset,
                    repair_advice=(
                        f"Rebuild chunk offsets in {track.chunk_box_path} "
                        f"(offset {track.chunk_box_offset}); each offset must be within the file."
                    ),
                )
            )
        if track.fragment_error:
            issues.append(
                _issue(
                    track.fragment_error,
                    BLOCKING,
                    "fragment samples have no per-sample or default duration",
                    track_id=track.track_id,
                    box_path=track.fragment_box_path,
                    offset=track.fragment_box_offset,
                    repair_advice=(
                        f"Set tfhd default_sample_duration or per-sample trun durations at "
                        f"{track.fragment_box_path} (offset {track.fragment_box_offset})."
                    ),
                )
            )
    issues.extend(_duplicate_id_issues(asset))
    return issues


def _profile_issues(asset: MediaAsset, profile: dict) -> list[dict]:
    issues: list[dict] = []
    video_profile = profile.get("video", {})
    audio_profile = profile.get("audio", {})
    for track in asset.tracks:
        if track.handler == "video" and video_profile:
            codecs = _expected_values(video_profile.get("codecs"))
            if codecs and track.codec not in codecs:
                issues.append(
                    _issue(
                        "CODEC_MISMATCH",
                        BLOCKING,
                        f"video codec {track.codec} is not in delivery codec list",
                        track_id=track.track_id,
                        box_path=track.codec_box_path,
                        offset=track.codec_box_offset,
                        repair_advice=f"Transcode {track.codec} at {track.codec_box_path} (offset {track.codec_box_offset}).",
                    )
                )
            expected_profiles = _expected_values(video_profile.get("profiles"))
            expected_levels = _expected_values(video_profile.get("levels"))
            mismatch = []
            if expected_profiles and track.profile not in expected_profiles:
                mismatch.append(f"profile {track.profile}")
            if expected_levels and track.level not in expected_levels:
                mismatch.append(f"level {track.level}")
            if mismatch:
                issues.append(
                    _issue(
                        "PROFILE_MISMATCH",
                        BLOCKING,
                        ", ".join(mismatch) + " does not match delivery profile",
                        track_id=track.track_id,
                        box_path=track.codec_box_path,
                        offset=track.codec_box_offset,
                        repair_advice=(
                            f"Re-encode to {expected_profiles or 'required profile'} / "
                            f"{expected_levels or 'required level'} at {track.codec_box_path} "
                            f"(offset {track.codec_box_offset})."
                        ),
                    )
                )
            max_width, max_height = video_profile.get("max_width"), video_profile.get("max_height")
            width_bad = bool(max_width and track.width and track.width > max_width)
            height_bad = bool(max_height and track.height and track.height > max_height)
            if width_bad or height_bad:
                issues.append(
                    _issue(
                        "RESOLUTION_MISMATCH",
                        BLOCKING,
                        f"video resolution {track.width}x{track.height} exceeds delivery limits",
                        track_id=track.track_id,
                        box_path=track.stsd_box_path,
                        offset=track.stsd_box_offset,
                        repair_advice=f"Downscale media referenced by {track.stsd_box_path} (offset {track.stsd_box_offset}).",
                    )
                )
        if track.handler == "audio" and audio_profile:
            codecs = _expected_values(audio_profile.get("codecs"))
            if codecs and track.codec not in codecs:
                issues.append(
                    _issue(
                        "CODEC_MISMATCH",
                        BLOCKING,
                        f"audio codec {track.codec} is not in delivery codec list",
                        track_id=track.track_id,
                        box_path=track.codec_box_path,
                        offset=track.codec_box_offset,
                        repair_advice=f"Transcode audio at {track.codec_box_path} (offset {track.codec_box_offset}).",
                    )
                )
            sample_rates = _expected_values(audio_profile.get("sample_rate"))
            if sample_rates and track.sample_rate not in sample_rates:
                issues.append(
                    _issue(
                        "SAMPLE_RATE_MISMATCH",
                        BLOCKING,
                        f"audio sample rate {track.sample_rate} is not in {sample_rates}",
                        track_id=track.track_id,
                        box_path=track.codec_box_path,
                        offset=track.codec_box_offset,
                        repair_advice=f"Resample track {track.track_id} at {track.codec_box_path} (offset {track.codec_box_offset}).",
                    )
                )
            channel_counts = _expected_values(audio_profile.get("channels"))
            if channel_counts and track.channels not in channel_counts:
                issues.append(
                    _issue(
                        "CHANNEL_MISMATCH",
                        BLOCKING,
                        f"audio channel count {track.channels} is not in {channel_counts}",
                        track_id=track.track_id,
                        box_path=track.codec_box_path,
                        offset=track.codec_box_offset,
                        repair_advice=f"Remix audio track {track.track_id} at {track.codec_box_path} (offset {track.codec_box_offset}).",
                    )
                )
    return issues


def _required_track_issues(asset: MediaAsset, profile: dict) -> list[dict]:
    required = profile.get("required_tracks", {})
    if isinstance(required, list):
        expected = required
    elif isinstance(required, dict):
        expected = required.get(asset.container, required.get(asset.format, []))
    else:
        expected = []
    present = {track.handler for track in asset.tracks}
    return [
        _issue(
            "MISSING_TRACK",
            BLOCKING,
            f"required {handler} track is missing",
            box_path="moov/trak" if asset.container == "isobmff" else "RIFF/data",
            offset=0,
            repair_advice=f"Add a {handler} track and update moov/trak tables before redelivery.",
        )
        for handler in expected
        if handler not in present
    ]


def check_asset(asset: MediaAsset, profile: dict, tolerance_ms: int) -> list[dict]:
    issues = _structural_issues(asset)
    issues.extend(_track_drift_issues(asset, tolerance_ms))
    issues.extend(_profile_issues(asset, profile))
    issues.extend(_required_track_issues(asset, profile))
    if asset.fragmented:
        issues.append(
            _issue(
                "FRAGMENTED_ASSET",
                INFO,
                "asset uses moof/trun fragments",
                box_path="moof",
                offset=0,
                repair_advice="No action required; fragment durations were aggregated.",
            )
        )
    if any(track.handler_type == "tmcd" for track in asset.tracks):
        tmcd = next(track for track in asset.tracks if track.handler_type == "tmcd")
        issues.append(
            _issue(
                "TIMECODE_TRACK",
                INFO,
                "timecode track excluded from duration drift checks",
                track_id=tmcd.track_id,
                box_path=tmcd.duration_box_path,
                offset=tmcd.duration_box_offset,
                repair_advice="No action required for delivery; tmcd is present for editorial sync.",
            )
        )
    return issues


def check_paths(path: str | os.PathLike[str], profile_path: str | os.PathLike[str], tolerance_ms: int) -> CheckReport:
    profile = load_profile(profile_path)
    checked: list[CheckedAsset] = []
    for media_path in discover_media(path):
        try:
            asset = probe_file(media_path)
        except MediaError as exc:
            if exc.exit_code == EXIT_IO:
                raise
            issue = _issue(
                exc.code,
                BLOCKING,
                str(exc),
                box_path="",
                offset=exc.offset or 0,
                repair_advice=f"Repair the container around offset {exc.offset or 0}; parsing stopped with {exc.code}.",
            )
            checked.append(CheckedAsset(media_path, None, [issue]))
            continue
        checked.append(CheckedAsset(media_path, asset, check_asset(asset, profile, tolerance_ms)))
    return CheckReport(os.fspath(profile_path), profile, tolerance_ms, checked)


def exit_code_for_report(report: CheckReport) -> int:
    return EXIT_BLOCKING if report.blocking_issues else 0
