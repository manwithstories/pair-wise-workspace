"""Human-readable terminal summaries for the three CLI commands."""

from __future__ import annotations


def render(command: str, payload: dict[str, object]) -> str:
    lines: list[str] = []
    if command == "inspect":
        _render_inspect(payload, lines)
    elif command == "remux":
        _render_remux(payload, lines)
    elif command == "verify":
        _render_verify(payload, lines)
    return "\n".join(lines)


def _render_inspect(payload: dict[str, object], lines: list[str]) -> None:
    summary = _as_dict(payload.get("summary"))
    lines.append(f"Matroska: {payload.get('input')}")
    lines.append(
        "elements={element_count} unknown-size={unknown_length_count} "
        "tracks={track_count} clusters={cluster_count} blocks={block_count} "
        "samples={sample_count}".format(**summary)
    )
    info = _as_dict(payload.get("info"))
    lines.append(
        f"timecode_scale={info.get('timecode_scale_ns')}ns "
        f"duration={info.get('duration_ns')}"
    )
    for track in payload.get("tracks") or []:
        lines.append(
            f"  track {track['number']}: {track['type']} {track['codec_id']} "
            f"lang={track['language']} uid={track['uid']} ({track['uid_source']})"
        )
    plan = payload.get("timestamp_plan")
    if plan:
        for entry in plan["tracks"]:
            lines.append(
                f"  timing track {entry['track_number']}: "
                f"{entry['rewritten_first_pts']}s .. {entry['rewritten_last_pts']}s "
                f"repairs={entry['repairs']['total']}"
            )


def _render_remux(payload: dict[str, object], lines: list[str]) -> None:
    lines.append(f"Wrote {payload['output']} ({payload['size']} bytes)")
    lines.append(f"sha256 {payload['sha256']}")
    for entry in _as_dict(payload.get("track_mapping")).get("tracks", []):
        lines.append(
            f"  track {entry['source_track_number']} -> mp4 {entry['mp4_track_id']} "
            f"{entry['type']} {entry['sample_entry']} samples={entry['sample_count']}"
        )
    for entry in _as_dict(payload.get("timestamps")).get("tracks", []):
        lines.append(
            f"  {entry['track_type']} track {entry['track_number']}: "
            f"{entry['rewritten_first_pts']}s .. {entry['rewritten_last_pts']}s"
        )
    lines.append(f"boxes={payload['box_count']} mdat_offset={payload['mdat_offset']}")
    verification = payload.get("verification")
    if verification:
        lines.append(
            f"verify: {'ok' if verification['ok'] else 'FAILED'} "
            f"({verification['failure_count']} failure(s))"
        )


def _render_verify(payload: dict[str, object], lines: list[str]) -> None:
    lines.append(f"MP4: {payload['path']} ({payload['size']} bytes)")
    lines.append(f"sha256 {payload['sha256']}")
    lines.append(f"boxes={payload['box_count']} tracks={len(payload.get('tracks', []))}")
    for check in payload.get("checks", []):
        mark = "ok  " if check["ok"] else "FAIL"
        lines.append(f"  [{mark}] {check['name']}: {check['detail']}")


def _as_dict(value: object) -> dict[str, object]:
    return value if isinstance(value, dict) else {}


__all__ = ["render"]
