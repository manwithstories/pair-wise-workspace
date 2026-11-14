"""ISO BMFF (MP4/MOV/M4A) parser: box tree, moov/trak/mdia tables, elst, tkhd."""

import struct

from .io_bounds import MediaError

KNOWN_BRANDS = {
    "isom", "iso2", "iso3", "iso4", "iso5", "iso6", "iso8",
    "mp41", "mp42", "avc1", "M4A ", "M4V ", "M4P ", "qt  ",
    "dash", "3gp4", "3gp5", "3gp6", "cmfc", "cmfs",
}
CONTAINER_BY_BRAND = {"qt  ": "mov", "M4A ": "m4a", "M4V ": "m4v", "M4P ": "m4p"}
HANDLER_KINDS = {
    "vide": "video", "soun": "audio",
    "text": "subtitle", "sbtl": "subtitle", "subt": "subtitle",
}
_ROTATIONS = (
    ((0, 0x10000, -0x10000, 0), 90),
    ((-0x10000, 0, 0, -0x10000), 180),
    ((0, -0x10000, 0x10000, 0), 270),
)


class _Cur:
    """Cursor over a box payload buffer; error offsets are absolute."""

    def __init__(self, buf, base, path):
        self.b, self.o, self.base, self.path = buf, 0, base, path

    def left(self):
        return len(self.b) - self.o

    def take(self, n):
        if n < 0 or self.o + n > len(self.b):
            raise MediaError("truncated box payload", self.base + self.o, self.path)
        d = self.b[self.o:self.o + n]
        self.o += n
        return d

    def skip(self, n):
        self.take(n)

    def u8(self):
        return self.take(1)[0]

    def u16(self):
        return struct.unpack(">H", self.take(2))[0]

    def u32(self):
        return struct.unpack(">I", self.take(4))[0]

    def s32(self):
        return struct.unpack(">i", self.take(4))[0]

    def u64(self):
        return struct.unpack(">Q", self.take(8))[0]

    def s64(self):
        return struct.unpack(">q", self.take(8))[0]

    def boxes(self):
        """Yield (type, payload_offset, payload_len) for each child box."""
        while self.left() >= 8:
            start = self.o
            size = self.u32()
            typ = self.take(4).decode("latin1")
            header = 8
            if size == 1:
                size = self.u64()
                header = 16
            elif size == 0:
                size = len(self.b) - start
            if typ == "uuid":
                self.skip(16)
                header += 16
            if size < header or start + size > len(self.b):
                raise MediaError(
                    "box %r size %d out of bounds" % (typ, size),
                    self.base + start, self.path)
            yield typ, self.o, size - header
            self.o = start + size
        if self.left():
            raise MediaError("trailing garbage in box", self.base + self.o, self.path)


def _iter_file_boxes(reader):
    end = reader.size
    while reader.tell() < end:
        start = reader.tell()
        if end - start < 8:
            raise MediaError("truncated box header", reader.base + start, reader.path)
        size = reader.u32be()
        typ = reader.read(4).decode("latin1")
        header = 8
        if size == 1:
            size = reader.u64be()
            header = 16
        elif size == 0:
            size = end - start
        if typ == "uuid":
            reader.skip(16)
            header += 16
        if size < header or start + size > end:
            raise MediaError(
                "box %r size %d exceeds file bounds" % (typ, size),
                reader.base + start, reader.path)
        yield typ, reader.tell(), size - header
        reader.seek(start + size)


def _parse_mvhd(cur):
    version = cur.u8()
    cur.skip(3)
    if version == 1:
        cur.skip(16)
        return cur.u32(), cur.u64()
    cur.skip(8)
    return cur.u32(), cur.u32()


def _parse_tkhd(cur):
    version = cur.u8()
    cur.skip(3)
    cur.skip(16 if version == 1 else 8)
    track_id = cur.u32()
    cur.skip(4)
    cur.u64() if version == 1 else cur.u32()
    cur.skip(16)
    matrix = [cur.s32() for _ in range(9)]
    width, height = cur.u32() >> 16, cur.u32() >> 16
    rot = 0
    for keys, deg in _ROTATIONS:
        if (matrix[0], matrix[1], matrix[3], matrix[4]) == keys:
            rot = deg
    return {"track_id": track_id, "matrix": matrix, "width": width,
            "height": height, "rotation": rot}


def _parse_mdhd(cur):
    version = cur.u8()
    cur.skip(3)
    cur.skip(16 if version == 1 else 8)
    timescale = cur.u32()
    duration = cur.u64() if version == 1 else cur.u32()
    lang = cur.u16()
    language = "".join(chr(((lang >> s) & 0x1F) + 0x60) for s in (10, 5, 0))
    return {"timescale": timescale, "duration": duration, "language": language}


def _parse_elst(cur):
    version = cur.u8()
    cur.skip(3)
    entries = []
    for _ in range(cur.u32()):
        if version == 1:
            seg, mt = cur.u64(), cur.s64()
        else:
            seg, mt = cur.u32(), cur.s32()
        cur.skip(4)
        entries.append((seg, mt))
    return entries


def _parse_stbl(cur, out):
    for typ, off, ln in cur.boxes():
        sub = _Cur(cur.b[off:off + ln], cur.base + off, cur.path)
        if typ == "stsd":
            sub.skip(4)
            if sub.u32():
                sub.u32()
                out["codec"] = sub.take(4).decode("latin1")
                if sub.left() >= 28:
                    sub.skip(16)
                    channels, bits = sub.u16(), sub.u16()
                    sub.skip(4)
                    out["stsd_audio"] = (channels, bits, sub.u32() >> 16)
        elif typ == "stts":
            sub.skip(4)
            out["stts"] = [(sub.u32(), sub.u32()) for _ in range(sub.u32())]
        elif typ == "stsz":
            sub.skip(4)
            uniform, count = sub.u32(), sub.u32()
            out["stsz"] = ([uniform] * count if uniform
                           else [sub.u32() for _ in range(count)])
        elif typ in ("stco", "co64"):
            sub.skip(4)
            read = sub.u64 if typ == "co64" else sub.u32
            out["stco"] = [read() for _ in range(sub.u32())]


def _walk(cur, out, depth=0):
    for typ, off, ln in cur.boxes():
        sub = _Cur(cur.b[off:off + ln], cur.base + off, cur.path)
        if typ == "tkhd":
            out["tkhd"] = _parse_tkhd(sub)
        elif typ == "mdhd":
            out["mdhd"] = _parse_mdhd(sub)
        elif typ == "hdlr":
            sub.skip(8)
            out["handler"] = sub.take(4).decode("latin1")
        elif typ == "elst":
            out["elst"] = _parse_elst(sub)
        elif typ == "stbl":
            _parse_stbl(sub, out)
        elif typ in ("edts", "mdia", "minf") and depth < 4:
            _walk(sub, out, depth + 1)


def _build_track(raw, mv_timescale):
    mdhd, tkhd = raw["mdhd"], raw.get("tkhd", {})
    timescale = mdhd["timescale"] or 1
    media_us = mdhd["duration"] * 1_000_000 // timescale
    duration_us, start_us, source = media_us, 0, "mdhd"
    elst = raw.get("elst") or []
    presented = [mt for _, mt in elst if mt >= 0]
    if elst and presented:
        duration_us = sum(s for s, _ in elst) * 1_000_000 // (mv_timescale or 1)
        start_us = presented[0] * 1_000_000 // timescale
        source = "elst"
    stts = raw.get("stts") or []
    stsz = raw.get("stsz") or []
    rotation = tkhd.get("rotation", 0)
    width, height = tkhd.get("width", 0), tkhd.get("height", 0)
    swapped = rotation in (90, 270)
    kind = HANDLER_KINDS.get(raw.get("handler", ""), "other")
    audio = raw.get("stsd_audio") if kind == "audio" else None
    return {
        "kind": kind,
        "codec": raw.get("codec", ""),
        "timescale": timescale,
        "duration_us": duration_us,
        "media_duration_us": media_us,
        "start_us": start_us,
        "sample_count": len(stsz),
        "sample_duration_us": stts[0][1] * 1_000_000 // timescale if stts else 0,
        "width": width, "height": height, "rotation": rotation,
        "display_width": height if swapped else width,
        "display_height": width if swapped else height,
        "sample_rate": audio[2] if audio else 0,
        "channels": audio[0] if audio else 0,
        "bits_per_sample": audio[1] if audio else 0,
        "language": mdhd["language"],
        "duration_source": source,
        "extra": {"stts": stts, "stsz": stsz, "stco": raw.get("stco", []),
                  "handler": raw.get("handler", ""),
                  "matrix": tkhd.get("matrix"), "track_id": tkhd.get("track_id", 0)},
    }


def parse(path, reader):
    """Parse an ISO BMFF file; returns (container, brands, tracks, warnings)."""
    ftyp_buf = moov_buf = moov_off = None
    for typ, off, ln in _iter_file_boxes(reader):
        if typ == "ftyp":
            reader.seek(off)
            ftyp_buf = reader.read(ln)
        elif typ == "moov":
            reader.seek(off)
            moov_buf, moov_off = reader.read(ln), off
    if ftyp_buf is None:
        raise MediaError("missing ftyp box", 0, path)
    if moov_buf is None:
        raise MediaError("missing moov box", 0, path)
    if len(ftyp_buf) < 8:
        raise MediaError("truncated ftyp box", 0, path)
    major = ftyp_buf[0:4].decode("latin1")
    brands = [major] + [ftyp_buf[i:i + 4].decode("latin1")
                        for i in range(8, len(ftyp_buf) - 3, 4)]
    warnings = []
    if major not in KNOWN_BRANDS:
        warnings.append({"rule_id": "UNKNOWN_BRAND", "severity": "warning",
                         "expected": "known ftyp brand", "actual": major,
                         "message": "unrecognized major brand %r" % major})
    cur = _Cur(moov_buf, moov_off, path)
    mvhd = (1000, 0)
    trak_bufs = []
    for typ, off, ln in cur.boxes():
        if typ == "mvhd":
            mvhd = _parse_mvhd(_Cur(moov_buf[off:off + ln], moov_off + off, path))
        elif typ == "trak":
            trak_bufs.append((off, ln))
    tracks = []
    for off, ln in trak_bufs:
        raw = {}
        _walk(_Cur(moov_buf[off:off + ln], moov_off + off, path), raw)
        if "mdhd" not in raw:
            raise MediaError("trak without mdhd", moov_off + off, path)
        tracks.append(_build_track(raw, mvhd[0]))
    return CONTAINER_BY_BRAND.get(major, "mp4"), brands, tracks, warnings
