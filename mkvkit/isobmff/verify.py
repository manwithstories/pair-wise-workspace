"""Read-only MP4 verification.

Parses the box hierarchy and checks: required box structure, strictly
monotonic DTS per track, negative ctts entries, edit-list media time,
stco/co64 chunk counts and mdat bounds, and inter-track first-frame
presentation delta.
"""
from __future__ import annotations

import os
import struct

import numpy as np

from ..ebml.reader import EbmlError

_CONTAINER_BOXES = {b"moov", b"trak", b"mdia", b"minf", b"stbl", b"edts", b"dinf"}


def _iter_boxes(data, start, end):
    pos = start
    while pos + 8 <= end:
        size, typ = struct.unpack_from(">I4s", data, pos)
        header = 8
        if size == 1:
            size = struct.unpack_from(">Q", data, pos + 8)[0]
            header = 16
        elif size == 0:
            size = end - pos
        if size < header or pos + size > end:
            raise EbmlError(f"invalid box {typ!r} at {pos}")
        yield typ, pos, size, pos + header
        pos += size
    if pos != end:
        raise EbmlError("box hierarchy does not align to file size")


def _find(children, typ):
    for child in children:
        if child[0] == typ:
            return child
    return None


def _box_end(box_tuple):
    _, offset, size, _ = box_tuple
    return offset + size


def _read_fullbox_table(data, box_tuple, entry_fmt, entry_size, skip=0):
    _, _, _, data_offset = box_tuple
    count = struct.unpack_from(">I", data, data_offset + 4)[0]
    out = []
    pos = data_offset + 8 + skip
    for _ in range(count):
        out.append(struct.unpack_from(entry_fmt, data, pos))
        pos += entry_size
    return out


def _trak_tables(data, trak):
    mdia = _find(list(_iter_boxes(data, trak[3], _box_end(trak))), b"mdia")
    if mdia is None:
        raise EbmlError("trak without mdia")
    mdia_children = list(_iter_boxes(data, mdia[3], _box_end(mdia)))
    mdhd = _find(mdia_children, b"mdhd")
    hdlr = _find(mdia_children, b"hdlr")
    minf = _find(mdia_children, b"minf")
    if mdhd is None or minf is None:
        raise EbmlError("mdia missing mdhd or minf")
    timescale = struct.unpack_from(">I", data, mdhd[3] + 12)[0]
    handler = data[hdlr[3] + 8:hdlr[3] + 12] if hdlr is not None else b""
    minf_children = list(_iter_boxes(data, minf[3], _box_end(minf)))
    stbl = _find(minf_children, b"stbl")
    if stbl is None:
        raise EbmlError("minf without stbl")
    stbl_children = list(_iter_boxes(data, stbl[3], _box_end(stbl)))
    tables = {"timescale": timescale, "handler": handler}
    for typ, off, size, doff in stbl_children:
        tables[typ.decode("latin1")] = (typ, off, size, doff)
    # edts is optional and lives next to mdia
    trak_children = list(_iter_boxes(data, trak[3], _box_end(trak)))
    edts = _find(trak_children, b"edts")
    if edts is not None:
        elst = _find(list(_iter_boxes(data, edts[3], _box_end(edts))), b"elst")
        if elst is not None:
            count = struct.unpack_from(">I", data, elst[3] + 4)[0]
            tables["elst"] = [
                struct.unpack_from(">Ii", data, elst[3] + 8 + 12 * i)
                for i in range(count)
            ]
    return tables


def _edit_timing(tables, movie_timescale):
    empty_duration = 0
    media_time = 0
    for segment_duration, entry_media_time in tables.get("elst", []):
        if entry_media_time == -1:
            empty_duration += segment_duration
        else:
            media_time = entry_media_time
            break
    empty_ms = empty_duration * 1000 // movie_timescale if movie_timescale else 0
    return empty_ms, media_time


def _verify_mp4(path) -> dict:
    file_size = os.path.getsize(path)
    with open(path, "rb") as fh:
        data = fh.read()
    top = list(_iter_boxes(data, 0, file_size))
    ftyp = _find(top, b"ftyp")
    if ftyp is None:
        raise EbmlError("missing ftyp box")
    moov = _find(top, b"moov")
    mdat = _find(top, b"mdat")
    if moov is None or mdat is None:
        raise EbmlError("missing moov or mdat box")
    mdat_start, mdat_end = mdat[3], mdat[1] + mdat[2]
    moov_children = list(_iter_boxes(data, moov[3], _box_end(moov)))
    traks = [c for c in moov_children if c[0] == b"trak"]
    mvhd = _find(moov_children, b"mvhd")
    movie_timescale = (
        struct.unpack_from(">I", data, mvhd[3] + 12)[0] if mvhd is not None else 0
    )
    box_tree_ok = (
        mvhd is not None and movie_timescale > 0 and bool(traks)
        and ftyp[1] < moov[1] < mdat[1]
    )

    tracks = []
    dts_monotonic = True
    tables_ok = True
    ctts_negative_count = 0
    stco_entries = 0
    mdat_bounds_ok = True
    first_pts = []
    handlers = []
    for trak in traks:
        tables = _trak_tables(data, trak)
        timescale = tables["timescale"]
        required = ("stsd", "stts", "stsc", "stsz")
        box_tree_ok = box_tree_ok and all(k in tables for k in required)
        has_offsets = "stco" in tables or "co64" in tables
        box_tree_ok = box_tree_ok and has_offsets

        stts = _read_fullbox_table(data, tables["stts"], ">II", 8)
        deltas = np.concatenate(
            [np.full(count, delta, dtype=np.int64) for count, delta in stts]
        )
        stts_samples = sum(count for count, _ in stts)
        dts = np.concatenate([[0], np.cumsum(deltas)[:-1]])
        if deltas.size and (deltas <= 0).any():
            dts_monotonic = False
        if dts.size > 1 and (np.diff(dts) <= 0).any():
            dts_monotonic = False

        ctts_vals = np.zeros(dts.size, dtype=np.int64)
        if "ctts" in tables:
            version = data[tables["ctts"][3]]
            fmt, signed = (">ii", True) if version == 1 else (">II", False)
            entries = _read_fullbox_table(data, tables["ctts"], fmt, 8)
            ctts_vals = np.concatenate(
                [np.full(c, v, dtype=np.int64) for c, v in entries]
            )
            if ctts_vals.size != int(stts_samples):
                tables_ok = False
            if signed:
                ctts_negative_count += int((ctts_vals < 0).sum())

        empty_edit_ms, elst_media_time = _edit_timing(tables, movie_timescale)

        stsz_box = tables["stsz"]
        sample_size, sample_count = struct.unpack_from(
            ">II", data, stsz_box[3] + 4
        )
        if sample_size:
            sizes = [sample_size] * sample_count
        else:
            sizes = list(
                struct.unpack_from(f">{sample_count}I", data, stsz_box[3] + 12)
            )
        if int(stts_samples) != sample_count or int(ctts_vals.size) != sample_count:
            tables_ok = False
        if "stco" in tables:
            offsets = [e[0] for e in _read_fullbox_table(data, tables["stco"], ">I", 4)]
        else:
            offsets = [e[0] for e in _read_fullbox_table(data, tables["co64"], ">Q", 8)]
        stco_entries = max(stco_entries, len(offsets))

        stsc = _read_fullbox_table(data, tables["stsc"], ">III", 12)
        sample_idx = 0
        for chunk_idx, offset in enumerate(offsets):
            run = max(r for r in stsc if r[0] <= chunk_idx + 1)
            per_chunk = run[1]
            chunk_bytes = sum(sizes[sample_idx:sample_idx + per_chunk])
            sample_idx += per_chunk
            if offset < mdat_start or offset + chunk_bytes > mdat_end:
                mdat_bounds_ok = False
        if sample_idx != sample_count:
            mdat_bounds_ok = False
        first_pts.append(
            empty_edit_ms / 1000.0
            + (dts[0] + ctts_vals[0] - elst_media_time) / timescale
        )
        handlers.append(tables["handler"])
        tracks.append(
            {
                "timescale": timescale,
                "samples": int(dts.size),
                "chunks": len(offsets),
                "elst_media_time": elst_media_time,
                "empty_edit_ms": empty_edit_ms,
            }
        )

    av_delta_ms = 0.0
    video_pts = next((p for p, h in zip(first_pts, handlers) if h == b"vide"), None)
    audio_pts = next((p for p, h in zip(first_pts, handlers) if h == b"soun"), None)
    if video_pts is not None and audio_pts is not None:
        av_delta_ms = round(abs(video_pts - audio_pts) * 1000.0, 3)
    elst_media_time = tracks[0]["elst_media_time"] if tracks else 0
    ok = (
        box_tree_ok and tables_ok and dts_monotonic
        and ctts_negative_count == 0 and mdat_bounds_ok
    )
    return {
        "ok": ok,
        "box_tree_ok": box_tree_ok,
        "dts_monotonic": dts_monotonic,
        "tables_consistent": tables_ok,
        "ctts_negative_count": ctts_negative_count,
        "elst_media_time": elst_media_time,
        "stco_entries": stco_entries,
        "mdat_bounds_ok": mdat_bounds_ok,
        "av_first_frame_delta_ms": av_delta_ms,
        "tracks": tracks,
    }


def verify_mp4(path) -> dict:
    """Verify an MP4 without modifying it, normalizing parse failures."""
    try:
        return _verify_mp4(path)
    except EbmlError:
        raise
    except (struct.error, IndexError, ValueError, ZeroDivisionError) as exc:
        raise EbmlError(f"invalid MP4 structure: {exc}") from exc
