"""RIFF/WAVE parser: fmt/fact/data chunks, EXTENSIBLE subformat GUID, padding."""

import struct

from .io_bounds import MediaError

_FORMAT_NAMES = {
    0x0001: "pcm", 0x0003: "ieee_float", 0x0006: "alaw", 0x0007: "mulaw",
    0x0050: "mpeg", 0x0055: "mp3", 0x00FF: "aac", 0x2000: "ac3",
}
_EXTENSIBLE = 0xFFFE
_GUID_TAIL = bytes.fromhex("000000001000800000aa00389b71")


def _parse_fmt(buf, base, path):
    if len(buf) < 16:
        raise MediaError("truncated fmt chunk", base, path)
    tag, channels, rate, _byte_rate, align, bits = struct.unpack_from(
        "<HHIIHH", buf, 0)
    if tag == _EXTENSIBLE:
        if len(buf) < 40:
            raise MediaError("truncated WAVE_FORMAT_EXTENSIBLE fmt", base, path)
        guid = buf[24:40]
        if guid[2:] != _GUID_TAIL:
            return {"format_tag": 0xFFFF, "channels": channels, "sample_rate": rate,
                    "block_align": align, "bits": bits, "codec": "unknown-guid"}
        tag = struct.unpack_from("<H", guid, 0)[0]
    return {"format_tag": tag, "channels": channels, "sample_rate": rate,
            "block_align": align, "bits": bits,
            "codec": _FORMAT_NAMES.get(tag, "tag-0x%04x" % tag)}


def parse(path, reader):
    """Parse a RIFF/WAVE file; returns (container, tracks, warnings)."""
    if reader.read(4) != b"RIFF":
        raise MediaError("missing RIFF magic", 0, path)
    riff_size = reader.u32le()
    if reader.read(4) != b"WAVE":
        raise MediaError("RIFF form type is not WAVE", 8, path)
    if riff_size + 8 > reader.size:
        raise MediaError("RIFF size %d exceeds file" % riff_size, 4, path)
    end = 8 + riff_size
    fmt = fact = None
    data_size = data_off = 0
    pos = 12
    while pos + 8 <= end:
        reader.seek(pos)
        cid = reader.read(4)
        csize = reader.u32le()
        payload_off = pos + 8
        if payload_off + csize > reader.size:
            raise MediaError(
                "chunk %r size %d exceeds file" % (cid.decode("latin1"), csize),
                payload_off, path)
        if cid == b"fmt ":
            fmt = _parse_fmt(reader.read(csize), payload_off, path)
        elif cid == b"fact":
            if csize >= 4:
                fact = reader.u32le()
        elif cid == b"data":
            data_size, data_off = csize, payload_off
        pos = payload_off + csize + (csize & 1)
    if fmt is None:
        raise MediaError("missing fmt chunk", 12, path)
    if data_off == 0:
        raise MediaError("missing data chunk", 12, path)
    rate = fmt["sample_rate"] or 1
    if fmt["format_tag"] in (0x0001, 0x0003) and fmt["block_align"]:
        frames = data_size // fmt["block_align"]
        source = "data"
    elif fact is not None:
        frames = fact
        source = "fact"
    else:
        frames, source = 0, "unknown"
    track = {
        "kind": "audio",
        "codec": fmt["codec"],
        "timescale": rate,
        "duration_us": frames * 1_000_000 // rate,
        "media_duration_us": frames * 1_000_000 // rate,
        "start_us": 0,
        "sample_count": frames,
        "sample_duration_us": 1_000_000 // rate,
        "sample_rate": fmt["sample_rate"],
        "channels": fmt["channels"],
        "bits_per_sample": fmt["bits"],
        "language": None,
        "duration_source": source,
        "extra": {"data_offset": data_off, "data_size": data_size,
                  "block_align": fmt["block_align"],
                  "format_tag": fmt["format_tag"]},
    }
    return "wav", [track], []
