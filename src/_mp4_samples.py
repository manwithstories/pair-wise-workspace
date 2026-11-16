"""Sample entries and codec configuration boxes.

Each entry describes one track's coded media in a form an MP4 reader can
consume, without touching the sample bytes: avc1/hvc1 carry the source
CodecPrivate (avcC/hvcC payload) verbatim, mp4a carries an esds built from the
AAC AudioSpecificConfig, Opus carries dOps, and tx3g is a fixed stub.
"""

from __future__ import annotations

from dataclasses import dataclass

from ._mp4_core import Box, Mp4Error, box, full_box


@dataclass
class VisualEntry:
    width: int
    height: int
    codec_private: bytes
    compressor: bytes = b""


@dataclass
class AudioEntry:
    channel_count: int
    sample_rate: int
    sample_size: int = 16
    codec_private: bytes = b""


def _pad(value: bytes, align: int = 4) -> bytes:
    remainder = len(value) % align
    return value if not remainder else value + b"\x00" * (align - remainder)


def avc1_entry(entry: VisualEntry) -> Box:
    """Visual sample entry ('avc1') with an avcC from CodecPrivate.

    The AVCDecoderConfigurationRecord is exactly the Matroska CodecPrivate
    payload, so no parsing is needed to pass it through — but an empty
    CodecPrivate is rejected rather than written as a broken avcC.
    """
    if not entry.codec_private:
        raise Mp4Error("missing_codec_private", "avc1 requires an AVC decoder configuration")
    if entry.width <= 0 or entry.height <= 0:
        raise Mp4Error("invalid_dimensions", f"avc1 needs positive dimensions, got {entry.width}x{entry.height}")
    payload = (
        b"\x00" * 6
        + (1).to_bytes(2, "big")  # data_reference_index
        + b"\x00" * 16  # pre_defined/reserved
        + entry.width.to_bytes(2, "big")
        + entry.height.to_bytes(2, "big")
        + (0x00480000).to_bytes(4, "big")  # 72 dpi
        + (0x00480000).to_bytes(4, "big")
        + b"\x00" * 4  # reserved
        + (1).to_bytes(2, "big")  # frame_count
        + _pad(entry.compressor)[:32].ljust(32, b"\x00")
        + (0x0018).to_bytes(2, "big")  # depth
        + b"\xff\xff"  # pre_defined = -1
    )
    children = [box(b"avcC", entry.codec_private)]
    return Box(type=b"avc1", payload=payload, children=children)


def hvc1_entry(entry: VisualEntry) -> Box:
    """Visual sample entry ('hvc1') with an hvcC from CodecPrivate."""
    if not entry.codec_private:
        raise Mp4Error("missing_codec_private", "hvc1 requires an HEVC decoder configuration")
    base = avc1_entry(VisualEntry(entry.width, entry.height, b"\x00", entry.compressor))
    base.type = b"hvc1"
    base.children = [box(b"hvcC", entry.codec_private)]
    return base


def _descriptor(tag: int, payload: bytes) -> bytes:
    """MPEG-4 descriptor with a 1..4 byte expandable length."""
    length = len(payload)
    encoded = bytearray()
    # Build the length from the most significant 7-bit group first.
    groups = [(length >> shift) & 0x7F for shift in range(21, -1, -7)]
    while len(groups) > 1 and groups[0] == 0:
        groups.pop(0)
    for index, value in enumerate(groups):
        last = index == len(groups) - 1
        encoded.append(value if last else value | 0x80)
    return bytes([tag]) + bytes(encoded) + payload


def esds_box(audio: AudioEntry, *, object_type: int = 0x40, stream_type: int = 0x05) -> Box:
    """ES_Descriptor for AAC (MPEG-4 Audio, object type 0x40)."""
    asc = audio.codec_private
    if asc:
        decoder_specific = _descriptor(0x05, asc)
        avg_bitrate = 0
        max_bitrate = 0
        buffer_size = 0
    else:
        # Fall back to a 2-byte AudioSpecificConfig derived from the sample
        # rate and channel count when the source omitted CodecPrivate.
        freq_index = _aac_frequency_index(audio.sample_rate)
        cfg = (0x02 << 11) | (freq_index << 7) | (audio.channel_count << 3)
        decoder_specific = _descriptor(0x05, cfg.to_bytes(2, "big"))
        avg_bitrate = max_bitrate = 128_000
        buffer_size = 6144
    decoder_config = (
        bytes([object_type])
        + b"\x15"  # stream type audio (0x05) << 2 | upstream=0 | reserved=1
        + (buffer_size).to_bytes(3, "big")
        + max_bitrate.to_bytes(4, "big")
        + avg_bitrate.to_bytes(4, "big")
        + decoder_specific
    )
    payload = (
        _descriptor(0x03, (1).to_bytes(2, "big") + b"\x00" + _descriptor(0x04, decoder_config))
        + _descriptor(0x06, b"\x02")
    )
    return full_box(b"esds", 0, 0, payload)


def _aac_frequency_index(sample_rate: int) -> int:
    table = [
        96_000, 88_200, 64_000, 48_000, 44_100, 32_000, 24_000,
        22_050, 16_000, 12_000, 11_025, 8_000, 7_350,
    ]
    if sample_rate in table:
        return table.index(sample_rate)
    return 0x0F  # explicit frequency, unsupported here; callers validate first


def mp4a_entry(audio: AudioEntry) -> Box:
    """Audio sample entry ('mp4a') with an esds."""
    if audio.channel_count <= 0:
        raise Mp4Error("invalid_channels", f"channel count {audio.channel_count} is invalid")
    if audio.sample_rate <= 0:
        raise Mp4Error("invalid_sample_rate", f"sample rate {audio.sample_rate} is invalid")
    if not audio.codec_private and audio.sample_rate not in (
        96_000, 88_200, 64_000, 48_000, 44_100, 32_000, 24_000,
        22_050, 16_000, 12_000, 11_025, 8_000, 7_350,
    ):
        raise Mp4Error(
            "invalid_sample_rate",
            f"AAC without CodecPrivate needs a standard sample rate, got {audio.sample_rate}",
        )
    payload = (
        b"\x00" * 6
        + (1).to_bytes(2, "big")
        + b"\x00" * 8
        + audio.channel_count.to_bytes(2, "big")
        + audio.sample_size.to_bytes(2, "big")
        + b"\x00\x00"  # pre_defined
        + b"\x00\x00"  # reserved
        + (audio.sample_rate << 16).to_bytes(4, "big")
    )
    return Box(type=b"mp4a", payload=payload, children=[esds_box(audio)])


def opus_entry(audio: AudioEntry) -> Box:
    """Audio sample entry ('Opus')."""
    if audio.channel_count <= 0 or audio.sample_rate <= 0:
        raise Mp4Error("invalid_audio", "Opus needs positive channels and sample rate")
    payload = (
        b"\x00" * 6
        + (1).to_bytes(2, "big")
        + b"\x00" * 8
        + audio.channel_count.to_bytes(2, "big")
        + audio.sample_size.to_bytes(2, "big")
        + b"\x00\x00"  # pre_defined
        + b"\x00\x00"  # reserved
        + (audio.sample_rate << 16).to_bytes(4, "big")
    )
    children = []
    if audio.codec_private:
        children.append(box(b"dOps", audio.codec_private))
    return Box(type=b"Opus", payload=payload, children=children)


def tx3g_entry() -> Box:
    """Minimal timed-text sample entry ('tx3g')."""
    payload = (
        b"\x00" * 6
        + (1).to_bytes(2, "big")
        + b"\x00" * 24
        + (0).to_bytes(2, "big")  # displayFlags
        + (1).to_bytes(2, "big")  # horizontal justification
        + b"\xff\xff\xff\xff"  # background colour rgba
        + b"\x00" * 12  # box record: top/left/bottom/right
        + b"\x00" * 4  # default style: startChar, endChar
        + (1).to_bytes(2, "big")  # font-ID
        + (0x12).to_bytes(2, "big")  # face-style-flags
        + (12).to_bytes(2, "big")  # font-size
        + b"\xff\xff\xff\xff"  # text colour
    )
    return Box(type=b"tx3g", payload=payload)


def stsd_box(entries: list[Box]) -> Box:
    """Sample description box.

    The entry count is the only fixed payload field; the entries themselves are
    kept as children so the offset walk and the parser see the same tree.
    """
    return full_box(b"stsd", 0, 0, (len(entries)).to_bytes(4, "big"), children=list(entries))


__all__ = [
    "AudioEntry",
    "VisualEntry",
    "avc1_entry",
    "esds_box",
    "hvc1_entry",
    "mp4a_entry",
    "opus_entry",
    "stsd_box",
    "tx3g_entry",
]
