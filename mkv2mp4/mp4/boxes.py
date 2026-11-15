"""ISO BMFF box serialisation.

Supports 32-bit sizes, 64-bit largesize, and fullbox version/flags.
All movie/track media times use version-0 (32-bit) fields; files are
far below any overflow threshold.
"""

import struct


def box(typ, payload):
    """Serialise one box; falls back to 64-bit largesize when needed."""
    size = len(payload) + 8
    if size > 0xFFFFFFFF:
        return struct.pack(">I4sQ", 1, typ, size + 8)
    return struct.pack(">I4s", size, typ) + payload


def fullbox(typ, version, flags, payload):
    return box(typ, struct.pack(">I", (version << 24) | flags) + payload)


MATRIX_UNITY = struct.pack(
    ">9I", 0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000)


def ftyp():
    return box(b"ftyp", b"isom" + struct.pack(">I", 0x200)
               + b"isomiso2avc1mp41")


def mvhd(timescale, duration, next_track_id):
    payload = struct.pack(">IIII", 0, 0, timescale, duration)
    payload += struct.pack(">I", 0x00010000)          # rate 1.0
    payload += struct.pack(">H", 0x0100) + b"\0" * 10  # volume 1.0
    payload += MATRIX_UNITY + b"\0" * 24
    payload += struct.pack(">I", next_track_id)
    return fullbox(b"mvhd", 0, 0, payload)


def tkhd(track_id, duration, width, height, is_audio):
    payload = struct.pack(">II", 0, 0)
    payload += struct.pack(">I", track_id) + b"\0" * 4
    payload += struct.pack(">I", duration) + b"\0" * 8
    payload += struct.pack(">HHH", 0, 0, 0x0100 if is_audio else 0)
    payload += b"\0" * 2 + MATRIX_UNITY
    payload += struct.pack(">II", int(width) << 16, int(height) << 16)
    return fullbox(b"tkhd", 0, 0x000007, payload)


def mdhd(timescale, duration, language="und"):
    packed = 0
    for ch in language[:3].ljust(3):
        packed = (packed << 5) | (ord(ch) - 0x60)
    payload = struct.pack(">IIII", 0, 0, timescale, duration)
    payload += struct.pack(">HH", packed, 0)
    return fullbox(b"mdhd", 0, 0, payload)


def hdlr(handler, name):
    payload = b"\0" * 4 + handler + b"\0" * 12 + name.encode() + b"\0"
    return fullbox(b"hdlr", 0, 0, payload)


def vmhd():
    return fullbox(b"vmhd", 0, 1, struct.pack(">HHHH", 0, 0, 0, 0))


def smhd():
    return fullbox(b"smhd", 0, 0, struct.pack(">HH", 0, 0))


def dinf():
    url = fullbox(b"url ", 0, 1, b"")  # self-contained
    return box(b"dinf", fullbox(b"dref", 0, 0, struct.pack(">I", 1) + url))


def stsd(entry):
    return fullbox(b"stsd", 0, 0, struct.pack(">I", 1) + entry)


def avc1(width, height, avcc_record):
    """VisualSampleEntry; avcC payload is the MKV CodecPrivate record."""
    entry = b"\0" * 6 + struct.pack(">H", 1) + b"\0" * 16
    entry += struct.pack(">HH", width, height)
    entry += struct.pack(">II", 0x00480000, 0x00480000) + b"\0" * 4
    entry += struct.pack(">H", 1) + b"\0" * 32        # compressorname
    entry += struct.pack(">Hh", 0x0018, -1)
    entry += box(b"avcC", avcc_record)
    return box(b"avc1", entry)


def _descriptor(tag, payload):
    """MPEG-4 expandable-size descriptor (7 bits per length byte)."""
    groups = []
    n = len(payload)
    while True:
        groups.append(n & 0x7F)
        n >>= 7
        if not n:
            break
    out = bytes([tag])
    for i, group in enumerate(reversed(groups)):
        out += bytes([group | (0x80 if i < len(groups) - 1 else 0)])
    return out + payload


def esds(audio_specific_config, max_bitrate=0, avg_bitrate=0):
    """ES_Descriptor wrapping an AAC AudioSpecificConfig."""
    dec_specific = _descriptor(0x05, audio_specific_config)
    dec_config = _descriptor(
        0x04,
        bytes([0x40, 0x15]) + b"\0" * 3                 # AAC, audio stream
        + struct.pack(">II", max_bitrate, avg_bitrate)
        + dec_specific)
    sl_config = _descriptor(0x06, b"\x02")
    es = _descriptor(0x03, struct.pack(">HB", 1, 0) + dec_config + sl_config)
    return fullbox(b"esds", 0, 0, es)


def mp4a(channels, sample_rate, esds_box):
    entry = b"\0" * 6 + struct.pack(">H", 1) + b"\0" * 8
    entry += struct.pack(">HHH", channels, 16, 0) + b"\0" * 2
    entry += struct.pack(">I", int(sample_rate) << 16)
    entry += esds_box
    return box(b"mp4a", entry)


def _table(typ, entries, fmt):
    payload = struct.pack(">I", len(entries))
    for entry in entries:
        payload += struct.pack(fmt, *entry)
    return fullbox(typ, 0, 0, payload)


def stts(entries):
    return _table(b"stts", entries, ">II")


def ctts(entries):
    return _table(b"ctts", entries, ">II")


def stsc(entries):
    return _table(b"stsc", entries, ">III")


def stss(indices):
    return _table(b"stss", [(i,) for i in indices], ">I")


def stco(offsets):
    return _table(b"stco", [(o,) for o in offsets], ">I")


def stsz(sizes):
    payload = struct.pack(">II", 0, len(sizes))
    payload += b"".join(struct.pack(">I", s) for s in sizes)
    return fullbox(b"stsz", 0, 0, payload)


def elst(entries):
    """Edit list: entries are (segment_duration, media_time) pairs."""
    payload = struct.pack(">I", len(entries))
    for duration, media_time in entries:
        payload += struct.pack(">Iihh", duration, media_time, 1, 0)
    return fullbox(b"elst", 0, 0, payload)
