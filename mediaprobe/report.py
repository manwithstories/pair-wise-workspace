from __future__ import annotations

import json
import os
import tempfile
from pathlib import Path

from .checks import CheckReport
from .errors import EXIT_IO, MediaError
from .probe import MediaAsset


def render_table(headers: list[str], rows: list[list[object]]) -> str:
    values = [[str(cell) if cell is not None else "" for cell in row] for row in rows]
    widths = [len(header) for header in headers]
    for row in values:
        for index, cell in enumerate(row):
            widths[index] = max(widths[index], len(cell))
    line = "  ".join(header.ljust(widths[index]) for index, header in enumerate(headers))
    separator = "  ".join("-" * width for width in widths)
    body = [
        "  ".join(cell.ljust(widths[index]) for index, cell in enumerate(row)).rstrip()
        for row in values
    ]
    return "\n".join([line, separator, *body])


def print_probe(asset: MediaAsset) -> None:
    print(f"Asset: {asset.path}")
    print(
        f"Container: {asset.container}/{asset.format}  size={asset.size_bytes}  "
        f"duration_ms={asset.duration_ms}  tracks={asset.track_count}  "
        f"fragmented={str(asset.fragmented).lower()}"
    )
    rows = []
    for track in asset.tracks:
        params = []
        if track.profile:
            params.append(f"{track.profile}/{track.level}" if track.level else track.profile)
        if track.sample_rate:
            params.append(f"{track.sample_rate}Hz")
        if track.channels:
            params.append(f"{track.channels}ch")
        if track.width and track.height:
            params.append(f"{track.width}x{track.height}")
        rows.append(
            [
                track.track_id,
                track.handler,
                track.codec,
                track.duration_ms,
                track.language or "-",
                " ".join(params),
            ]
        )
    print(render_table(["id", "handler", "codec", "ms", "lang", "params"], rows))


def print_check(report: CheckReport) -> None:
    print(
        f"profile={report.profile.get('name', report.profile_path)}  "
        f"assets={len(report.assets)}  passed={report.passed}  rejected={report.rejected}  "
        f"issues={report.issue_count}  blocking={report.blocking_issues}"
    )
    rows = []
    for item in report.assets:
        rows.append([item.path, item.status, item.asset.track_count if item.asset else 0, len(item.issues)])
    print(render_table(["asset", "status", "tracks", "issues"], rows))
    blocking = [issue for issue in report.flat_issues() if issue["severity"] == "blocking"]
    if blocking:
        print("Blocking issues:")
        for issue in blocking:
            print(
                f"  {issue['asset']}  {issue['code']}  track={issue['track_id']}  "
                f"{issue['box_path'] or '-'}@{issue['offset']}  {issue['message']}"
            )


def print_pack(result: dict) -> None:
    print(
        f"bundle={result['bundle']}  assets={result['asset_count']}  passed={result['passed']}  "
        f"rejected={result['rejected']}  tracks={result['track_count']}  "
        f"tracks_written={result['tracks_written']}"
    )
    print(f"manifest_sha256={result['manifest_sha256']}")


def print_issues(rows: list[dict]) -> None:
    if not rows:
        print("No matching issues.")
        return
    print(
        render_table(
            ["asset", "severity", "code", "track", "delta_ms", "path"],
            [
                [
                    row.get("asset", ""),
                    row.get("severity", ""),
                    row.get("code", ""),
                    row.get("track_id"),
                    row.get("delta_ms"),
                    f"{row.get('box_path', '')}@{row.get('offset', 0)}",
                ]
                for row in rows
            ],
        )
    )


def write_json(path: str | os.PathLike[str], payload: object) -> None:
    destination = Path(path)
    try:
        destination.parent.mkdir(parents=True, exist_ok=True)
        text = json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=False) + "\n"
        fd, temporary = tempfile.mkstemp(prefix=".mediaprobe-", dir=destination.parent)
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as fh:
                fh.write(text)
            os.replace(temporary, destination)
        finally:
            if os.path.exists(temporary):
                os.unlink(temporary)
    except PermissionError as exc:
        raise MediaError("PERMISSION_DENIED", str(exc), path=os.fspath(destination), exit_code=EXIT_IO) from exc
    except OSError as exc:
        raise MediaError("IO_ERROR", str(exc), path=os.fspath(destination), exit_code=EXIT_IO) from exc
