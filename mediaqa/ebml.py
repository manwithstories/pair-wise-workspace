"""EBML parser for WebM/MKV: Segment/Info/Tracks, unknown-length elements."""

import struct

from .io_bounds import MediaError

ID_EBML = 0x1A45DFA3
ID_DOCTYPE = 0x4282
ID_SEGMENT = 0x18538067
ID_INFO = 0x1549A966
ID_TIMECODE_SCALE = 0x2AD7B1
ID_DURATION = 0x4489
ID_TRACKS = 0x1654AE6B
ID_TRACK_ENTRY = 0xAE
ID_TRACK_NUMBER = 0xD7
ID_TRACK_TYPE = 0x83
ID_CODEC_ID = 0x86
ID_DEFAULT_DURATION = 0x23E383
ID_LANGUAGE = 0x22B59C
ID_VIDEO = 0xE0
ID_PIXEL_WIDTH = 0xB0
ID_PIXEL_HEIGHT = 0xBA
ID_AUDIO = 0xE1
ID_SAMPLING_FREQ = 0xB5
ID_CHANNELS = 0x9F
ID_BIT_DEPTH = 0x6264
ID_CLUSTER = 0x1F43B675
ID_TIMECODE = 0xE7

_TRACK_KINDS = {1: "video", 2: "audio", 8: "subtitle"}


def _uint(reader, size):
    value = 0
    for _ in range(size):
        value = (value << 8) | reader.u8()
    return value


def _float(reader, size):
    if size == 4:
        return struct.unpack(">f", reader.read(4))[0]
    if size == 8:
        return struct.unpack(">d", reader.read(8))[0]
    raise MediaError("bad EBML float size %d" % size,
                     reader.base + reader.tell(), reader.path)


def _iter_elements(reader, end):
    """Yield (id, payload_start, payload_size); unknown size runs to `end`."""
    while reader.tell() < end:
        off = reader.tell()
        try:
            eid = reader.ebml_id()
            size = reader.ebml_size()
        except MediaError:
            return
        payload_end = end if size is None else reader.tell() + size
        if payload_end > end:
            raise MediaError("element 0x%x exceeds parent bounds" % eid,
                             reader.base + off, reader.path)
        yield eid, reader.tell(), payload_end
        reader.seek(payload_end)


def _parse_track_entry(reader, end):
    info = {"language": "und", "sample_rate": 0, "channels": 0,
            "bits_per_sample": 0, "width": 0, "height": 0,
            "codec": "", "kind": "other", "sample_duration_us": 0}
    for eid, off, eend in _iter_elements(reader, end):
        reader.seek(off)
        size = eend - off
        if eid == ID_TRACK_TYPE:
            info["kind"] = _TRACK_KINDS.get(_uint(reader, size), "other")
        elif eid == ID_CODEC_ID:
            info["codec"] = reader.read(size).decode("utf-8", "replace")
        elif eid == ID_DEFAULT_DURATION:
            info["sample_duration_us"] = _uint(reader, size) // 1000
        elif eid == ID_LANGUAGE:
            info["language"] = reader.read(size).decode("ascii", "replace")
        elif eid == ID_VIDEO:
            for vid, voff, vend in _iter_elements(reader, eend):
                reader.seek(voff)
                if vid == ID_PIXEL_WIDTH:
                    info["width"] = _uint(reader, vend - voff)
                elif vid == ID_PIXEL_HEIGHT:
                    info["height"] = _uint(reader, vend - voff)
        elif eid == ID_AUDIO:
            for aid, aoff, aend in _iter_elements(reader, eend):
                reader.seek(aoff)
                if aid == ID_SAMPLING_FREQ:
                    info["sample_rate"] = int(_float(reader, aend - aoff))
                elif aid == ID_CHANNELS:
                    info["channels"] = _uint(reader, aend - aoff)
                elif aid == ID_BIT_DEPTH:
                    info["bits_per_sample"] = _uint(reader, aend - aoff)
    return info


def _parse_segment(reader, end):
    scale, duration_raw = 1_000_000, None
    tracks, last_timecode = [], 0
    for eid, off, eend in _iter_elements(reader, end):
        reader.seek(off)
        if eid == ID_INFO:
            for iid, ioff, iend in _iter_elements(reader, eend):
                reader.seek(ioff)
                if iid == ID_TIMECODE_SCALE:
                    scale = _uint(reader, iend - ioff)
                elif iid == ID_DURATION:
                    duration_raw = _float(reader, iend - ioff)
        elif eid == ID_TRACKS:
            for tid, toff, tend in _iter_elements(reader, eend):
                if tid == ID_TRACK_ENTRY:
                    reader.seek(toff)
                    tracks.append(_parse_track_entry(reader, tend))
        elif eid == ID_CLUSTER:
            for cid, coff, cend in _iter_elements(reader, eend):
                if cid == ID_TIMECODE:
                    reader.seek(coff)
                    last_timecode = max(last_timecode,
                                        _uint(reader, cend - coff))
    if duration_raw is not None:
        duration_us, source = int(duration_raw * scale / 1000), "container"
    else:
        duration_us, source = last_timecode * scale // 1000, "unknown"
    out = []
    for t in tracks:
        t.update({
            "timescale": 1_000_000_000 // max(scale, 1),
            "duration_us": duration_us,
            "media_duration_us": duration_us,
            "start_us": 0,
            "sample_count": 0,
            "duration_source": source,
            "extra": {},
        })
        t["display_width"], t["display_height"] = t["width"], t["height"]
        t["rotation"] = 0
        out.append(t)
    return out, source


def parse(path, reader):
    """Parse a WebM/MKV file; returns (container, tracks, warnings)."""
    if reader.ebml_id() != ID_EBML:
        raise MediaError("missing EBML header", 0, path)
    size = reader.ebml_size()
    if size is None or reader.tell() + size > reader.size:
        raise MediaError("bad EBML header size", 0, path)
    header = reader.sub(size)
    doctype = "matroska"
    for eid, off, eend in _iter_elements(header, header.size):
        if eid == ID_DOCTYPE:
            header.seek(off)
            doctype = header.read(eend - off).decode("ascii", "replace")
    while not reader.eof():
        off = reader.tell()
        eid = reader.ebml_id()
        size = reader.ebml_size()
        if eid == ID_SEGMENT:
            end = reader.size if size is None else reader.tell() + size
            tracks, _source = _parse_segment(reader, end)
            if not tracks:
                raise MediaError("Segment without Tracks", reader.base + off, path)
            container = "webm" if doctype == "webm" else "mkv"
            return container, tracks, []
        if size is None:
            raise MediaError("unknown-length element outside Segment",
                             reader.base + off, path)
        reader.skip(size)
    raise MediaError("missing Segment element", 0, path)
