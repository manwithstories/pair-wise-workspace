"""Streaming verification of the JSONL index against the CFR proxy."""

from __future__ import annotations

import json
from fractions import Fraction
from pathlib import Path
from typing import Any, Iterator

import av

from frameaudit.errors import InputError
from frameaudit.stats import plane_hash


REQUIRED_FIELDS = (
    "decode_idx",
    "present_idx",
    "pts_raw_us",
    "pts_fixed_us",
    "pict_type",
    "keyframe",
    "plane_hash",
)


def _rows(path: Path) -> Iterator[tuple[int, dict[str, Any] | None, str | None]]:
    with path.open("r", encoding="utf-8") as stream:
        for line_number, line in enumerate(stream, start=1):
            try:
                row = json.loads(line)
            except Exception as exc:
                yield line_number, None, f"index line {line_number}: invalid JSON: {exc}"
                continue
            if not isinstance(row, dict) or any(field not in row for field in REQUIRED_FIELDS):
                yield line_number, None, f"index line {line_number}: missing required fields"
                continue
            if not isinstance(row["pts_fixed_us"], int) or isinstance(row["pts_fixed_us"], bool):
                yield line_number, None, f"index line {line_number}: pts_fixed_us is not an integer"
                continue
            yield line_number, row, None


def _frame_pts_us(frame: av.VideoFrame, stream: av.VideoStream) -> int | None:
    if frame.pts is None:
        return None
    time_base = frame.time_base or stream.time_base or Fraction(1, 1000)
    return round(frame.pts * time_base.numerator * 1_000_000 / time_base.denominator)


def verify_outputs(index_path: str | Path, proxy_path: str | Path) -> dict[str, Any]:
    index_file = Path(index_path)
    proxy_file = Path(proxy_path)
    if not index_file.is_file():
        raise InputError(f"index does not exist: {index_file}")
    if not proxy_file.is_file():
        raise InputError(f"proxy does not exist: {proxy_file}")

    errors: list[str] = []
    rows = _rows(index_file)
    index_rows = 0
    proxy_frames = 0
    hash_mismatches = 0
    index_monotonic = True
    proxy_monotonic = True
    cfr = True
    previous_index_pts: int | None = None
    previous_proxy_pts: int | None = None
    first_index_pts: int | None = None
    last_index_pts: int | None = None
    first_proxy_pts: int | None = None
    last_proxy_pts: int | None = None
    cfr_step: int | None = None

    try:
        container = av.open(str(proxy_file))
    except Exception as exc:
        raise InputError(f"cannot open proxy: {proxy_file}: {exc}") from exc
    try:
        if not container.streams.video:
            raise InputError(f"proxy has no video stream: {proxy_file}")
        stream = container.streams.video[0]
        frame_iterator = container.decode(stream)
        try:
            for frame_number, frame in enumerate(frame_iterator):
                proxy_frames += 1
                line_number, row, parse_error = next(rows, (None, None, None))
                if line_number is not None:
                    index_rows += 1
                if parse_error:
                    errors.append(parse_error)
                row = row or {}

                proxy_pts = _frame_pts_us(frame, stream)
                if first_proxy_pts is None:
                    first_proxy_pts = proxy_pts
                if proxy_pts is not None:
                    last_proxy_pts = proxy_pts
                    if previous_proxy_pts is not None:
                        delta = proxy_pts - previous_proxy_pts
                        if delta <= 0:
                            proxy_monotonic = False
                        if cfr_step is None:
                            cfr_step = delta
                        elif delta != cfr_step:
                            cfr = False
                    previous_proxy_pts = proxy_pts

                fixed_pts = row.get("pts_fixed_us")
                if isinstance(fixed_pts, int) and not isinstance(fixed_pts, bool):
                    if first_index_pts is None:
                        first_index_pts = fixed_pts
                    last_index_pts = fixed_pts
                    if previous_index_pts is not None and fixed_pts <= previous_index_pts:
                        index_monotonic = False
                    previous_index_pts = fixed_pts
                    if row.get("present_idx") != frame_number:
                        errors.append(f"index line {line_number}: present_idx is not contiguous")
                    if proxy_pts is None or abs(proxy_pts - fixed_pts) > 1:
                        errors.append(f"frame {frame_number}: proxy/index PTS mismatch")

                expected_hash = row.get("plane_hash")
                actual_hash = plane_hash(frame)
                if expected_hash != actual_hash:
                    hash_mismatches += 1
                    if hash_mismatches <= 8:
                        errors.append(f"frame {frame_number}: plane hash mismatch")
        except Exception as exc:
            errors.append(f"proxy decode failed: {exc}")

        for line_number, _row, parse_error in rows:
            index_rows += 1
            if parse_error:
                errors.append(parse_error)
            elif index_rows > proxy_frames:
                errors.append(f"index line {line_number}: no matching proxy frame")
    finally:
        container.close()

    if index_rows != proxy_frames:
        errors.append(f"frame count mismatch: index={index_rows} proxy={proxy_frames}")
    if index_rows == 0:
        errors.append("index is empty")
    if proxy_frames == 0:
        errors.append("proxy is empty")
    if not index_monotonic:
        errors.append("index pts_fixed_us is not strictly monotonic")
    if not proxy_monotonic:
        errors.append("proxy PTS is not strictly monotonic")
    if not cfr:
        errors.append("proxy is not constant frame rate")
    if first_index_pts != first_proxy_pts:
        errors.append("first PTS differs between index and proxy")
    if last_index_pts != last_proxy_pts:
        errors.append("last PTS differs between index and proxy")

    first_pts = first_index_pts if first_index_pts is not None else first_proxy_pts
    last_pts = last_index_pts if last_index_pts is not None else last_proxy_pts
    return {
        "ok": not errors,
        "index_rows": index_rows,
        "proxy_frames": proxy_frames,
        "first_pts_us": first_pts,
        "last_pts_us": last_pts,
        "index_first_pts_us": first_index_pts,
        "index_last_pts_us": last_index_pts,
        "proxy_first_pts_us": first_proxy_pts,
        "proxy_last_pts_us": last_proxy_pts,
        "index_monotonic": index_monotonic,
        "proxy_monotonic": proxy_monotonic,
        "proxy_cfr": cfr,
        "hash_mismatches": hash_mismatches,
        "errors": errors,
    }

