"""Read back MP4 boxes and compare their sample tables with a remux manifest."""

from __future__ import annotations

import io
import json
import os
import struct
from dataclasses import dataclass, field
from typing import BinaryIO


class VerifyError(RuntimeError):
    pass


CONTAINERS = {b"moov", b"trak", b"mdia", b"minf", b"stbl", b"edts", b"dinf", b"udta"}


@dataclass(slots=True)
class Box:
    type: str
    start: int
    size: int
    header_size: int
    data_start: int
    children: list["Box"] = field(default_factory=list)

    @property
    def end(self) -> int:
        return self.start + self.size


def _header(stream: BinaryIO, offset: int, file_size: int) -> tuple[str, int, int]:
    stream.seek(offset)
    raw = stream.read(16)
    if len(raw) < 8:
        raise VerifyError(f"truncated box header at {offset}")
    size, box_type = struct.unpack(">I4s", raw[:8])
    header_size = 8
    if size == 1:
        if len(raw) < 16:
            raise VerifyError(f"truncated large box header at {offset}")
        size = struct.unpack(">Q", raw[8:16])[0]
        header_size = 16
    elif size == 0:
        size = file_size - offset
    if size < header_size or offset + size > file_size:
        raise VerifyError(f"invalid box size for {box_type!r} at {offset}")
    return box_type.decode("latin-1"), size, header_size


def read_top_boxes(path: str) -> tuple[list[Box], int]:
    file_size = os.path.getsize(path)
    boxes: list[Box] = []
    with open(path, "rb") as stream:
        offset = 0
        while offset < file_size:
            box_type, size, header_size = _header(stream, offset, file_size)
            boxes.append(Box(box_type, offset, size, header_size, offset + header_size))
            offset += size
    return boxes, file_size


def _parse_boxes(data: bytes, base: int) -> list[Box]:
    boxes: list[Box] = []
    offset = 0
    while offset < len(data):
        if len(data) - offset < 8:
            raise VerifyError("truncated child box")
        size, box_type = struct.unpack(">I4s", data[offset:offset + 8])
        header_size = 8
        if size == 1:
            if len(data) - offset < 16:
                raise VerifyError("truncated large child box")
            size = struct.unpack(">Q", data[offset + 8:offset + 16])[0]
            header_size = 16
        elif size == 0:
            size = len(data) - offset
        if size < header_size or offset + size > len(data):
            raise VerifyError("invalid child box size")
        node = Box(box_type.decode("latin-1"), base + offset, size, header_size,
                   base + offset + header_size)
        if node.type.encode("latin-1") in CONTAINERS:
            node.children = _parse_boxes(data[offset + header_size:offset + size],
                                         node.data_start)
        boxes.append(node)
        offset += size
    return boxes


def _children(node: Box, box_type: str) -> list[Box]:
    return [child for child in node.children if child.type == box_type]


def _child(node: Box, box_type: str) -> Box | None:
    found = _children(node, box_type)
    return found[0] if found else None


def _payload(data: bytes, base: int, node: Box | None) -> bytes:
    if node is None:
        return b""
    start = node.data_start - base
    end = node.end - base
    return data[start:end]


def _tkhd_id(data: bytes, base: int, node: Box | None) -> int:
    payload = _payload(data, base, node)
    if len(payload) < 16:
        raise VerifyError("truncated tkhd")
    return struct.unpack(">I", payload[12:16])[0]


def _mdhd(data: bytes, base: int, node: Box | None) -> tuple[int, int]:
    payload = _payload(data, base, node)
    if len(payload) < 24:
        raise VerifyError("truncated mdhd")
    timescale, duration = struct.unpack(">II", payload[12:20])
    return timescale, duration


def _handler(data: bytes, base: int, node: Box | None) -> str:
    payload = _payload(data, base, node)
    if len(payload) < 12:
        raise VerifyError("truncated hdlr")
    return payload[8:12].decode("latin-1")


def _stsd_type(data: bytes, base: int, node: Box | None) -> str:
    payload = _payload(data, base, node)
    if len(payload) < 16:
        raise VerifyError("truncated stsd")
    return payload[12:16].decode("latin-1")


def _table(data: bytes, base: int, node: Box | None, entry_size: int) -> tuple[int, bytes]:
    payload = _payload(data, base, node)
    if len(payload) < 8:
        raise VerifyError("truncated sample table")
    count = struct.unpack(">I", payload[4:8])[0]
    entries = payload[8:8 + count * entry_size]
    if len(entries) != count * entry_size:
        raise VerifyError("truncated sample table entries")
    return count, entries


def _stts_entries(data: bytes, base: int, node: Box | None) -> list[tuple[int, int]]:
    count, raw = _table(data, base, node, 8)
    return [struct.unpack(">II", raw[index:index + 8]) for index in range(0, count * 8, 8)]


def _ctts_entries(data: bytes, base: int, node: Box | None) -> list[tuple[int, int]]:
    payload = _payload(data, base, node)
    if not payload:
        return []
    version = payload[0]
    count, raw = _table(data, base, node, 8)
    fmt = ">Ii" if version else ">II"
    return [struct.unpack(fmt, raw[index:index + 8]) for index in range(0, count * 8, 8)]


def _stsz(data: bytes, base: int, node: Box | None) -> tuple[int, list[int]]:
    payload = _payload(data, base, node)
    if len(payload) < 12:
        raise VerifyError("truncated stsz")
    default, count = struct.unpack(">II", payload[4:12])
    if default:
        return count, [default] * count
    raw = payload[12:12 + count * 4]
    if len(raw) != count * 4:
        raise VerifyError("truncated stsz sizes")
    return count, list(struct.unpack(f">{count}I", raw))


def _stss(data: bytes, base: int, node: Box | None, sample_count: int) -> list[int]:
    if node is None:
        return list(range(1, sample_count + 1))
    count, raw = _table(data, base, node, 4)
    return list(struct.unpack(f">{count}I", raw))


def _ctts_range(data: bytes, base: int, node: Box | None,
                sample_count: int) -> tuple[int, int]:
    """Return the first and last composition offset without expanding arrays."""
    entries = _ctts_entries(data, base, node)
    if not entries:
        return 0, 0
    offset = 0
    index = 0
    first: int | None = None
    last = 0
    for run_count, value in entries:
        for _ in range(run_count):
            if index >= sample_count:
                break
            if first is None:
                first = value
            last = value
            index += 1
        if index >= sample_count:
            break
    return (first or 0), last


def _track_summary(data: bytes, base: int, trak: Box) -> dict[str, object]:
    tkhd = _child(trak, "tkhd")
    mdia = _child(trak, "mdia")
    if mdia is None:
        raise VerifyError("trak has no mdia")
    mdhd = _child(mdia, "mdhd")
    hdlr = _child(mdia, "hdlr")
    minf = _child(mdia, "minf")
    stbl = _child(minf, "stbl") if minf else None
    if stbl is None:
        raise VerifyError("trak has no stbl")
    timescale, _ = _mdhd(data, base, mdhd)
    stsd = _child(stbl, "stsd")
    codec = _stsd_type(data, base, stsd)
    stts = _stts_entries(data, base, _child(stbl, "stts"))
    stsz_node = _child(stbl, "stsz")
    sample_count, _sizes = _stsz(data, base, stsz_node)
    stts_count = sum(count for count, _ in stts)
    if stts_count != sample_count:
        raise VerifyError("stts/stsz sample count mismatch")
    ctts = _child(stbl, "ctts")
    first_offset, last_offset = _ctts_range(data, base, ctts, sample_count)
    if sample_count:
        first_dts = 0
        last_dts = sum(count * duration for count, duration in stts) - stts[-1][1]
        first_pts = first_dts + first_offset
        last_pts = last_dts + last_offset
    else:
        first_pts = last_pts = 0
    stss = _child(stbl, "stss")
    sync = _stss(data, base, stss, sample_count)
    return {
        "track_ID": _tkhd_id(data, base, tkhd),
        "codec": codec,
        "handler": _handler(data, base, hdlr),
        "timescale": timescale or 1,
        "samples": sample_count,
        "sample_count": sample_count,
        "first_pts": round(first_pts / (timescale or 1), 6),
        "last_pts": round(last_pts / (timescale or 1), 6),
        "keyframes": len(sync),
    }


def _chpl_count(data: bytes, base: int, moov: Box) -> int | None:
    udta = _child(moov, "udta")
    chpl = _child(udta, "chpl") if udta else None
    if chpl is None:
        return None
    payload = _payload(data, base, chpl)
    return payload[8] if len(payload) >= 9 else 0


def verify_file(mp4_path: str, manifest_path: str) -> dict[str, object]:
    try:
        manifest = json.loads(open(manifest_path, "rb").read())
    except (OSError, json.JSONDecodeError) as exc:
        raise VerifyError(f"cannot read manifest: {exc}") from exc
    try:
        top, file_size = read_top_boxes(mp4_path)
        moov = next((box for box in top if box.type == "moov"), None)
        mdat = next((box for box in top if box.type == "mdat"), None)
        if moov is None or mdat is None:
            raise VerifyError("MP4 must contain moov and mdat")
        with open(mp4_path, "rb") as stream:
            stream.seek(moov.data_start)
            moov_data = stream.read(moov.size - moov.header_size)
        moov.children = _parse_boxes(moov_data, moov.data_start)
    except (OSError, VerifyError, struct.error) as exc:
        if isinstance(exc, VerifyError):
            raise
        raise VerifyError(str(exc)) from exc
    moov_before_mdat = moov.start < mdat.start
    actual_tracks = [_track_summary(moov_data, moov.data_start, trak)
                     for trak in _children(moov, "trak")]
    expected_tracks = manifest.get("tracks", [])
    errors: list[str] = []
    if not moov_before_mdat:
        errors.append("moov is not before mdat")
    if len(actual_tracks) != len(expected_tracks):
        errors.append(f"track count {len(actual_tracks)} != {len(expected_tracks)}")
    if len(actual_tracks) != len(expected_tracks):
        match = False
    else:
        match = True
        for actual, expected in zip(actual_tracks, expected_tracks):
            for key in ("track_ID", "samples", "first_pts", "last_pts", "keyframes"):
                actual_key = "sample_count" if key == "samples" and key not in actual else key
                if actual.get(actual_key) != expected.get(key):
                    match = False
                    errors.append(f"track {actual.get('track_ID')} {key}: "
                                  f"{actual.get(actual_key)!r} != {expected.get(key)!r}")
    manifested_chapters = len(manifest.get("chapters", []))
    actual_chapters = _chpl_count(moov_data, moov.data_start, moov)
    if manifested_chapters and actual_chapters != manifested_chapters:
        errors.append(f"chapter count {actual_chapters} != {manifested_chapters}")
    return {
        "ok": moov_before_mdat and match and not errors,
        "moov_before_mdat": moov_before_mdat,
        "samples_match": match,
        "tracks_match": match,
        "samples": sum(track["samples"] for track in actual_tracks),
        "tracks": actual_tracks,
        "errors": errors,
    }


