"""Sample-table construction and two-layout MP4 export.

Only AVC (V_MPEG4/ISO/AVC) and AAC (A_AAC) tracks are remuxed; any
other codec raises UnsupportedCodecError before a single output byte
is written, so no track is ever silently dropped.

Layouts:
  * ``moov-last`` - ftyp, mdat, moov (single pass, mdat size backfilled)
  * ``faststart`` - ftyp, moov, mdat (moov built twice: a dry run fixes
    its length, then chunk offsets are computed and it is rebuilt)

Both layouts share byte-identical sample tables (stsd/stts/ctts/stss/
stsc/stsz); only box order and stco absolute offsets differ.
"""

import hashlib
import struct

from ..errors import EbmlError, UnsupportedCodecError
from ..mkv.timing import build_timing, rle
from . import boxes

CODEC_MAP = {"V_MPEG4/ISO/AVC": "avc1", "A_AAC": "mp4a"}
MOVIE_TIMESCALE = 1000
MDAT_HEADER_SIZE = 16  # 4-byte size=1 + "mdat" + 8-byte largesize


class TrackPlan:
    def __init__(self, track, track_id, samples, timing):
        self.track = track
        self.track_id = track_id
        self.samples = samples
        self.timing = timing
        self.sizes = [s.size for s in samples]
        self.keyframes = [i + 1 for i, s in enumerate(samples) if s.keyframe]
        self.chunks = []  # [(relative_offset, [sizes])] in mdat order
        self.table_boxes = {}  # name -> bytes (stco excluded from hashing)


def _check_codecs(doc):
    for t in doc.tracks:
        if t.codec_id not in CODEC_MAP:
            raise UnsupportedCodecError(
                "track %d codec %r is not supported "
                "(AVC/AAC passthrough only)" % (t.number, t.codec_id))


def _build_plans(doc, timescale):
    plans = []
    for track_id, track in enumerate(doc.tracks, 1):
        samples = [s for s in doc.samples if s.track_number == track.number]
        timing = build_timing([s.pts_ns for s in samples], timescale,
                              track.default_duration_ns)
        plans.append(TrackPlan(track, track_id, samples, timing))
    # Chunk plan: consecutive same-track runs in source (mdat) order.
    current = {}
    pos = 0
    by_number = {p.track.number: p for p in plans}
    for s in doc.samples:
        plan = by_number[s.track_number]
        if current.get(s.track_number) is None:
            current[s.track_number] = [pos, []]
            plan.chunks.append(current[s.track_number])
        current[s.track_number][1].append(s.size)
        pos += s.size
        for other in plans:
            if other.track.number != s.track_number:
                current[other.track.number] = None
    payload_size = pos
    return plans, payload_size


def _sample_entry(plan):
    t = plan.track
    if t.codec_id == "V_MPEG4/ISO/AVC":
        return boxes.avc1(t.width, t.height, t.codec_private)
    return boxes.mp4a(t.channels, t.sample_rate,
                      boxes.esds(t.codec_private))


def _build_tables(plan, stco_base):
    timing = plan.timing
    n = timing.sample_count
    tables = {}
    tables["stsd"] = boxes.stsd(_sample_entry(plan))
    tables["stts"] = boxes.stts([(n, timing.delta)])
    if timing.has_b_frames:
        tables["ctts"] = boxes.ctts(rle(timing.ctts))
    if 0 < len(plan.keyframes) < n:
        tables["stss"] = boxes.stss(plan.keyframes)
    sc_entries = []
    offsets = []
    previous_count = None
    for index, (rel_offset, sizes) in enumerate(plan.chunks, 1):
        offsets.append(stco_base + rel_offset)
        if len(sizes) != previous_count:
            sc_entries.append((index, len(sizes), 1))
            previous_count = len(sizes)
    tables["stsc"] = boxes.stsc(sc_entries)
    tables["stsz"] = boxes.stsz(plan.sizes)
    tables["stco"] = boxes.stco(offsets)
    plan.table_boxes = tables


def _build_trak(plan):
    timing = plan.timing
    t = plan.track
    is_video = t.type == 1
    duration_ms = (timing.duration_ticks * MOVIE_TIMESCALE
                   + timing.timescale - 1) // timing.timescale
    tables = plan.table_boxes
    stbl = (tables["stsd"] + tables["stts"]
            + tables.get("ctts", b"") + tables.get("stss", b"")
            + tables["stsc"] + tables["stsz"] + tables["stco"])
    if is_video:
        media_header = boxes.vmhd()
        handler = boxes.hdlr(b"vide", "VideoHandler")
    else:
        media_header = boxes.smhd()
        handler = boxes.hdlr(b"soun", "SoundHandler")
    minf = boxes.box(b"minf", media_header + boxes.dinf()
                     + boxes.box(b"stbl", stbl))
    mdia = boxes.box(b"mdia",
                     boxes.mdhd(timing.timescale, timing.duration_ticks,
                                t.language)
                     + handler + minf)
    edts = b""
    if timing.edit_media_time > 0:
        edts = boxes.box(b"edts", boxes.elst(
            [(duration_ms, timing.edit_media_time)]))
    return boxes.box(b"trak",
                     boxes.tkhd(plan.track_id, duration_ms,
                                t.width if is_video else 0,
                                t.height if is_video else 0,
                                not is_video)
                     + edts + mdia)


def sample_table_digest(plans):
    """SHA-256 over layout-independent sample tables (stco excluded)."""
    digest = hashlib.sha256()
    for plan in plans:
        digest.update(struct.pack(">I", plan.track_id))
        for name in ("stsd", "stts", "ctts", "stss", "stsc", "stsz"):
            digest.update(plan.table_boxes.get(name, b""))
    return digest.hexdigest()


def _write_mdat(out, doc):
    """Stream sample payloads into mdat; backfill the 64-bit largesize."""
    header_pos = out.tell()
    out.write(struct.pack(">I4sQ", 1, b"mdat", 0))  # placeholder
    data_start = out.tell()
    written = 0
    with open(doc.path, "rb") as src:
        for s in doc.samples:
            src.seek(s.offset)
            remaining = s.size
            while remaining > 0:
                buf = src.read(min(1 << 20, remaining))
                if not buf:
                    raise EbmlError("truncated sample data", s.offset)
                out.write(buf)
                remaining -= len(buf)
                written += len(buf)
    end = out.tell()
    out.seek(header_pos)
    out.write(struct.pack(">I4sQ", 1, b"mdat", MDAT_HEADER_SIZE + written))
    out.seek(end)
    return data_start, written


class MuxResult:
    def __init__(self, plans, layout, timescale, mdat_offset, mdat_size,
                 moov_offset, moov_size, output_path=""):
        self.plans = plans
        self.layout = layout
        self.timescale = timescale
        self.mdat_offset = mdat_offset
        self.mdat_size = mdat_size
        self.moov_offset = moov_offset
        self.moov_size = moov_size
        self.output_path = output_path
        self.sample_table_sha256 = sample_table_digest(plans)


def remux(doc, out_path, layout="faststart", timescale=90000):
    """Remux a parsed MkvDocument into an MP4 file."""
    if layout not in ("faststart", "moov-last"):
        raise ValueError("unknown layout %r" % layout)
    _check_codecs(doc)
    plans, payload_size = _build_plans(doc, timescale)
    ftyp = boxes.ftyp()

    def build_moov(stco_base):
        for plan in plans:
            _build_tables(plan, stco_base)
        duration_ms = max(
            (p.timing.duration_ticks * MOVIE_TIMESCALE
             + p.timing.timescale - 1) // p.timing.timescale
            for p in plans)
        moov_payload = boxes.mvhd(MOVIE_TIMESCALE, duration_ms,
                                  len(plans) + 1)
        moov_payload += b"".join(_build_trak(p) for p in plans)
        return boxes.box(b"moov", moov_payload)

    moov_dry = build_moov(0)
    if layout == "faststart":
        stco_base = len(ftyp) + len(moov_dry) + MDAT_HEADER_SIZE
        moov = build_moov(stco_base)
        assert len(moov) == len(moov_dry), "moov length changed"
        sequence = [ftyp, moov, None]  # None marks the mdat position
    else:
        stco_base = len(ftyp) + MDAT_HEADER_SIZE
        moov = build_moov(stco_base)
        sequence = [ftyp, None, moov]

    mdat_offset = mdat_size = moov_offset = 0
    with open(out_path, "wb") as out:
        for part in sequence:
            if part is None:
                mdat_offset, mdat_size = _write_mdat(out, doc)
            else:
                if part is moov:
                    moov_offset = out.tell()
                out.write(part)
    if mdat_size != payload_size:
        raise EbmlError("mdat payload size mismatch: wrote %d, planned %d"
                        % (mdat_size, payload_size))
    return MuxResult(plans, layout, timescale, mdat_offset, mdat_size,
                     moov_offset, len(moov), str(out_path))


# ---------------------------------------------------------------------
# Re-parse produced MP4 files for verification.
# ---------------------------------------------------------------------

_CONTAINER_BOXES = {b"moov", b"trak", b"mdia", b"minf", b"stbl", b"edts"}


def _iter_boxes(buf, start=0, end=None):
    """Yield (type, payload_offset, size, header_size) over a buffer."""
    end = len(buf) if end is None else end
    pos = start
    while pos + 8 <= end:
        size, typ = struct.unpack_from(">I4s", buf, pos)
        header = 8
        if size == 1:
            size = struct.unpack_from(">Q", buf, pos + 8)[0]
            header = 16
        elif size == 0:
            size = end - pos
        if size < header or pos + size > end:
            raise EbmlError("invalid MP4 box at 0x%x" % pos)
        yield typ, pos + header, size, header
        pos += size


def _find_child(buf, start, end, wanted):
    for typ, off, size, header in _iter_boxes(buf, start, end):
        if typ == wanted:
            return off, size - header
    return None


def _read_u32_table(buf, off, size):
    count = struct.unpack_from(">I", buf, off + 4)[0]
    values = []
    pos = off + 8
    for _ in range(count):
        values.append(struct.unpack_from(">I", buf, pos)[0])
        pos += 4
    return values


def _read_pair_table(buf, off, size):
    count = struct.unpack_from(">I", buf, off + 4)[0]
    pairs = []
    pos = off + 8
    for _ in range(count):
        pairs.append(struct.unpack_from(">II", buf, pos))
        pos += 8
    return pairs


def inspect(path):
    """Re-parse a produced MP4; returns a verification summary dict."""
    import os
    size_total = os.path.getsize(path)
    top = {}
    with open(path, "rb") as fh:
        pos = 0
        while pos + 8 <= size_total:
            fh.seek(pos)
            head = fh.read(16)
            if len(head) < 8:
                break
            size, typ = struct.unpack_from(">I4s", head, 0)
            header = 8
            if size == 1:
                size = struct.unpack_from(">Q", head, 8)[0]
                header = 16
            elif size == 0:
                size = size_total - pos
            if size < header or pos + size > size_total:
                raise EbmlError("invalid MP4 box at 0x%x" % pos)
            top[typ] = (pos + header, size - header, pos, size)
            pos += size
    if b"moov" not in top or b"mdat" not in top:
        raise EbmlError("MP4 is missing moov or mdat")
    mdat_off, mdat_len, mdat_box_off, mdat_box_size = top[b"mdat"]
    moov_off, moov_len, moov_box_off, moov_box_size = top[b"moov"]
    with open(path, "rb") as fh:
        fh.seek(moov_box_off)
        data = fh.read(moov_box_size)
    moov_off = moov_box_off + (moov_box_size - moov_len)
    tracks = []
    moov_payload_off = moov_off - moov_box_off
    for typ, off, size, header in _iter_boxes(
            data, moov_payload_off, moov_box_size):
        if typ != b"trak":
            continue
        tracks.append(_inspect_trak(data, off, size - header))
    return {
        "tracks": tracks,
        "mdat": {"offset": mdat_off, "size": mdat_len},
        "moov": {"offset": moov_box_off, "size": moov_box_size},
        "sample_table_sha256": _digest_tracks(tracks),
    }


def _inspect_trak(data, off, size):
    end = off + size
    track = {"track_ID": 0, "timescale": 0, "elst_media_time": 0,
             "stts": [], "ctts": [], "stsz": [], "stco": [],
             "tables": {}}
    mdia = _find_child(data, off, end, b"mdia")
    edts = _find_child(data, off, end, b"edts")
    tkhd = _find_child(data, off, end, b"tkhd")
    track["track_ID"] = struct.unpack_from(">I", data, tkhd[0] + 12)[0]
    if edts:
        elst = _find_child(data, edts[0], edts[0] + edts[1], b"elst")
        if elst and struct.unpack_from(">I", data, elst[0] + 4)[0] > 0:
            track["elst_media_time"] = struct.unpack_from(
                ">i", data, elst[0] + 12)[0]
    moff, msize = mdia
    mdhd = _find_child(data, moff, moff + msize, b"mdhd")
    track["timescale"] = struct.unpack_from(">I", data, mdhd[0] + 12)[0]
    minf = _find_child(data, moff, moff + msize, b"minf")
    stbl = _find_child(data, minf[0], minf[0] + minf[1], b"stbl")
    for typ, boff, bsize, bheader in _iter_boxes(data, stbl[0],
                                                 stbl[0] + stbl[1]):
        name = typ.decode("ascii")
        payload = data[boff - bheader:boff - bheader + bsize]
        if name in ("stsd", "stts", "ctts", "stss", "stsc", "stsz"):
            track["tables"][name] = payload
        if name == "stts":
            track["stts"] = _read_pair_table(data, boff, bsize)
        elif name == "ctts":
            track["ctts"] = _read_pair_table(data, boff, bsize)
        elif name == "stsz":
            count = struct.unpack_from(">I", data, boff + 8)[0]
            track["stsz"] = [
                struct.unpack_from(">I", data, boff + 12 + 4 * i)[0]
                for i in range(count)]
        elif name == "stco":
            track["stco"] = _read_u32_table(data, boff, bsize)
    _derive_pts(track)
    return track


def _derive_pts(track):
    deltas = []
    for count, delta in track["stts"]:
        deltas.extend([delta] * count)
    offsets = []
    for count, offset in track["ctts"]:
        offsets.extend([offset] * count)
    if not offsets:
        offsets = [0] * len(deltas)
    dts = 0
    presented = []
    for i, delta in enumerate(deltas):
        presented.append(dts + offsets[i] - track["elst_media_time"])
        dts += delta
    track["sample_count"] = len(deltas)
    scale = track["timescale"] or 1
    track["first_pts_ms"] = (min(presented) * 1000.0 / scale
                             if presented else 0.0)
    track["last_pts_ms"] = (max(presented) * 1000.0 / scale
                            if presented else 0.0)


def _digest_tracks(tracks):
    digest = hashlib.sha256()
    for track in sorted(tracks, key=lambda t: t["track_ID"]):
        digest.update(struct.pack(">I", track["track_ID"]))
        for name in ("stsd", "stts", "ctts", "stss", "stsc", "stsz"):
            digest.update(track["tables"].get(name, b""))
    return digest.hexdigest()


def verify(expect, actual):
    """Compare an inspect() summary against a remux report; -> mismatches."""
    mismatches = []
    expected_tracks = {t["track_ID"]: t for t in expect.get("tracks", [])}
    for track in actual["tracks"]:
        want = expected_tracks.get(track["track_ID"])
        if want is None:
            mismatches.append("unexpected track_ID %d" % track["track_ID"])
            continue
        tid = track["track_ID"]
        if track["sample_count"] != want["sample_count"]:
            mismatches.append("track %d sample_count %d != %d" % (
                tid, track["sample_count"], want["sample_count"]))
        if abs(track["first_pts_ms"] - want["first_pts_ms"]) > 0.5:
            mismatches.append("track %d first_pts_ms %.3f != %.3f" % (
                tid, track["first_pts_ms"], want["first_pts_ms"]))
        if abs(track["last_pts_ms"] - want["last_pts_ms"]) > 1.0:
            mismatches.append("track %d last_pts_ms %.3f != %.3f" % (
                tid, track["last_pts_ms"], want["last_pts_ms"]))
    if len(actual["tracks"]) != len(expected_tracks):
        mismatches.append("track count %d != %d" % (
            len(actual["tracks"]), len(expected_tracks)))
    if actual["sample_table_sha256"] != expect.get("sample_table_sha256"):
        mismatches.append("sample_table_sha256 differs")
    exp_mdat = expect.get("mdat", {})
    if actual["mdat"]["offset"] != exp_mdat.get("offset"):
        mismatches.append("mdat offset %d != %d" % (
            actual["mdat"]["offset"], exp_mdat.get("offset")))
    if actual["mdat"]["size"] != exp_mdat.get("size"):
        mismatches.append("mdat size %d != %d" % (
            actual["mdat"]["size"], exp_mdat.get("size")))
    total_samples = sum(len(t["stsz"]) for t in actual["tracks"])
    total_bytes = sum(sum(t["stsz"]) for t in actual["tracks"])
    if total_bytes != actual["mdat"]["size"]:
        mismatches.append("stsz total %d != mdat payload %d" % (
            total_bytes, actual["mdat"]["size"]))
    for track in actual["tracks"]:
        if track["stco"]:
            first = min(track["stco"])
            last_end = actual["mdat"]["offset"] + actual["mdat"]["size"]
            if first < actual["mdat"]["offset"] or \
                    max(track["stco"]) >= last_end:
                mismatches.append("track %d stco outside mdat"
                                  % track["track_ID"])
    if total_samples != sum(t["sample_count"] for t in actual["tracks"]):
        mismatches.append("stsz/stts sample count mismatch")
    return mismatches
