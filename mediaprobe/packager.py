from __future__ import annotations

import hashlib
import json
import os
import shutil
from pathlib import Path

from .checks import CheckedAsset, check_paths
from .errors import EXIT_IO, MediaError
from .index import IndexDB
from .probe import MediaAsset, file_sha256
from .isobmff import TrackInfo


CHUNK_SIZE = 1024 * 1024


def _copy_stream(source: str, destination: Path) -> None:
    destination.parent.mkdir(parents=True, exist_ok=True)
    temporary = destination.with_name(destination.name + ".tmp")
    try:
        with open(source, "rb") as source_fh, open(temporary, "wb") as target_fh:
            shutil.copyfileobj(source_fh, target_fh, length=CHUNK_SIZE)
        os.replace(temporary, destination)
    finally:
        if temporary.exists():
            temporary.unlink()


def _copy_asset(asset: MediaAsset, bundle: Path) -> tuple[str, bool]:
    relative = Path("assets") / asset.sha256 / Path(asset.path).name
    destination = bundle / relative
    reused = False
    if destination.is_file() and file_sha256(destination) == asset.sha256:
        reused = True
    else:
        _copy_stream(asset.path, destination)
    return relative.as_posix(), reused


def _select_tracks(asset: MediaAsset, profile: dict) -> list[TrackInfo]:
    selector = profile.get("slice") or {}
    handler = selector.get("handler")
    language = selector.get("language")
    selected = []
    for track in asset.tracks:
        if handler and track.handler != handler:
            continue
        if language and track.language != language:
            continue
        if not track.sample_ranges:
            continue
        selected.append(track)
    return selected


def _slice_track(asset: MediaAsset, track: TrackInfo, bundle: Path) -> dict:
    relative = Path("tracks") / asset.sha256 / f"{track.track_id}.es"
    destination = bundle / relative
    destination.parent.mkdir(parents=True, exist_ok=True)
    temporary = destination.with_name(destination.name + ".tmp")
    digest = hashlib.sha256()
    size = 0
    try:
        with open(asset.path, "rb") as source_fh, open(temporary, "wb") as target_fh:
            for sample_range in track.sample_ranges:
                if sample_range.offset < 0 or sample_range.offset + sample_range.size > asset.size_bytes:
                    raise MediaError(
                        "CHUNK_OFFSET_OUT_OF_RANGE",
                        f"cannot slice track {track.track_id}: sample range exceeds source",
                        offset=sample_range.offset,
                    )
                source_fh.seek(sample_range.offset)
                remaining = sample_range.size
                while remaining:
                    chunk = source_fh.read(min(CHUNK_SIZE, remaining))
                    if not chunk:
                        raise MediaError("FILE_TRUNCATED", "source ended during track slice", offset=sample_range.offset)
                    target_fh.write(chunk)
                    digest.update(chunk)
                    remaining -= len(chunk)
                    size += len(chunk)
        os.replace(temporary, destination)
    finally:
        if temporary.exists():
            temporary.unlink()
    return {
        "sha256": asset.sha256,
        "track_id": track.track_id,
        "handler": track.handler,
        "language": track.language,
        "codec": track.codec,
        "stored_path": relative.as_posix(),
        "size_bytes": size,
        "bytes_sha256": digest.hexdigest(),
        "byte_ranges": [
            {"offset": item.offset, "size": item.size, "duration": item.duration}
            for item in track.sample_ranges
        ],
    }


def _asset_manifest(checked: CheckedAsset, stored_path: str | None, reused: bool) -> dict:
    blocking = [issue for issue in checked.issues if issue["severity"] == "blocking"]
    repairs = [
        {
            "code": issue["code"],
            "box_path": issue["box_path"],
            "offset": issue["offset"],
            "advice": issue["repair_advice"],
        }
        for issue in blocking
    ]
    asset = checked.asset
    return {
        "path": checked.path,
        "sha256": asset.sha256 if asset else None,
        "size_bytes": asset.size_bytes if asset else 0,
        "track_count": asset.track_count if asset else 0,
        "status": checked.status,
        "stored_path": stored_path,
        "reused": reused,
        "repair_advice": repairs,
        "issues": checked.issues,
    }


def _manifest_sha256(body: dict) -> str:
    canonical = json.dumps(body, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(canonical).hexdigest()


def _write_manifest(path: Path, manifest: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    text = json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=False) + "\n"
    try:
        temporary.write_text(text, encoding="utf-8")
        os.replace(temporary, path)
    except PermissionError as exc:
        raise MediaError("PERMISSION_DENIED", str(exc), path=os.fspath(path), exit_code=EXIT_IO) from exc
    except OSError as exc:
        raise MediaError("IO_ERROR", str(exc), path=os.fspath(path), exit_code=EXIT_IO) from exc
    finally:
        if temporary.exists():
            temporary.unlink()


def pack(
    source: str | os.PathLike[str],
    profile_path: str | os.PathLike[str],
    bundle_path: str | os.PathLike[str],
    db_path: str | os.PathLike[str] | None = None,
    tolerance_ms: int = 40,
) -> dict:
    report = check_paths(source, profile_path, tolerance_ms)
    bundle = Path(bundle_path)
    try:
        bundle.mkdir(parents=True, exist_ok=True)
    except OSError as exc:
        raise MediaError("IO_ERROR", str(exc), path=os.fspath(bundle), exit_code=EXIT_IO) from exc
    asset_entries = []
    track_entries = []
    for checked in report.assets:
        stored_path = None
        reused = False
        try:
            if checked.asset is not None and checked.status == "passed":
                stored_path, reused = _copy_asset(checked.asset, bundle)
                for track in _select_tracks(checked.asset, report.profile):
                    track_entries.append(_slice_track(checked.asset, track, bundle))
        except PermissionError as exc:
            raise MediaError("PERMISSION_DENIED", str(exc), path=checked.path, exit_code=EXIT_IO) from exc
        except OSError as exc:
            raise MediaError("IO_ERROR", str(exc), path=checked.path, exit_code=EXIT_IO) from exc
        asset_entries.append(_asset_manifest(checked, stored_path, reused))
    body = {
        "format_version": 1,
        "bundle": os.fspath(bundle),
        "profile": report.profile_path,
        "profile_name": report.profile.get("name", ""),
        "tolerance_ms": tolerance_ms,
        "asset_count": len(report.assets),
        "passed": report.passed,
        "rejected": report.rejected,
        "track_count": sum(item.asset.track_count for item in report.assets if item.asset is not None),
        "tracks_written": len(track_entries),
        "assets": asset_entries,
        "tracks": track_entries,
        "issues": report.flat_issues(),
    }
    manifest_sha256 = _manifest_sha256(body)
    manifest = {**body, "manifest_sha256": manifest_sha256}
    manifest_path = bundle / "manifest.json"
    _write_manifest(manifest_path, manifest)
    if db_path is not None:
        run_info = {
            "run_key": f"pack:{bundle.resolve()}",
            "kind": "pack",
            "bundle_path": os.fspath(bundle),
            "manifest_sha256": manifest_sha256,
            "asset_count": body["asset_count"],
            "passed": body["passed"],
            "rejected": body["rejected"],
            "track_count": body["track_count"],
            "tracks_written": body["tracks_written"],
        }
        with IndexDB(db_path) as database:
            database.ingest_report(report, run_info)
    return manifest
