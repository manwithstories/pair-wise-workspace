from __future__ import annotations

import struct
from dataclasses import dataclass


AVC_PROFILES = {
    66: "Baseline",
    77: "Main",
    88: "Extended",
    100: "High",
    110: "High 10",
    122: "High 4:2:2",
    244: "High 4:4:4",
}
HEVC_PROFILES = {
    1: "Main",
    2: "Main 10",
    3: "Main Still Picture",
    4: "Range Extensions",
}
SAMPLE_RATES = [
    96000,
    88200,
    64000,
    48000,
    44100,
    32000,
    24000,
    22050,
    16000,
    12000,
    11025,
    8000,
    7350,
]
AAC_CHANNELS = {1: 1, 2: 2, 3: 3, 4: 4, 5: 5, 6: 6, 7: 8}


@dataclass
class CodecInfo:
    codec: str | None = None
    profile: str | None = None
    level: str | None = None
    sample_rate: int | None = None
    channels: int | None = None
    object_type: int | None = None
    codec_box_path: str = ""
    box_offset: int = 0


def _level_decimal(value: int, divisor: int = 10) -> str:
    whole, decimal = divmod(value, divisor)
    return f"{whole}.{decimal}" if decimal else f"{whole}.0"


def avc_level(level_idc: int) -> str:
    return _level_decimal(level_idc)


def hevc_level(level_idc: int) -> str:
    return _level_decimal(level_idc, 30)


def parse_avcc(data: bytes) -> CodecInfo:
    if len(data) < 4:
        return CodecInfo(codec="h264")
    return CodecInfo(
        codec="h264",
        profile=AVC_PROFILES.get(data[1], f"Profile {data[1]}"),
        level=avc_level(data[3]),
    )


def parse_hvcc(data: bytes) -> CodecInfo:
    if len(data) < 13:
        return CodecInfo(codec="hevc")
    profile_idc = data[1] & 0x1F
    return CodecInfo(
        codec="hevc",
        profile=HEVC_PROFILES.get(profile_idc, f"Profile {profile_idc}"),
        level=hevc_level(data[12]),
    )


class _Bits:
    def __init__(self, data: bytes) -> None:
        self.data = data
        self.position = 0

    def take(self, count: int) -> int:
        if self.position + count > len(self.data) * 8:
            raise ValueError("audio specific config is truncated")
        value = 0
        for _ in range(count):
            byte_index = self.position // 8
            bit_index = 7 - self.position % 8
            value = (value << 1) | ((self.data[byte_index] >> bit_index) & 1)
            self.position += 1
        return value


def parse_audio_specific_config(data: bytes) -> tuple[int | None, int | None, int | None]:
    """Return (audio object type, sample rate, channel count)."""

    if not data:
        return None, None, None
    bits = _Bits(data)
    try:
        object_type = bits.take(5)
        if object_type == 31:
            object_type = 32 + bits.take(6)
        frequency_index = bits.take(4)
        if frequency_index == 15:
            sample_rate = bits.take(24)
        else:
            sample_rate = SAMPLE_RATES[frequency_index] if frequency_index < len(SAMPLE_RATES) else None
        channels = bits.take(4)
        if object_type in (5, 29):
            extension_index = bits.take(4)
            if extension_index < len(SAMPLE_RATES):
                sample_rate = SAMPLE_RATES[extension_index]
            bits.take(5)  # base audio object type
        return object_type, sample_rate, AAC_CHANNELS.get(channels)
    except (IndexError, ValueError):
        return None, None, None


def _descriptor_length(data: bytes, position: int, end: int) -> tuple[int, int]:
    length = 0
    for _ in range(4):
        if position >= end:
            raise ValueError("truncated descriptor length")
        byte = data[position]
        position += 1
        length = (length << 7) | (byte & 0x7F)
        if not byte & 0x80:
            return length, position
    raise ValueError("descriptor length exceeds four bytes")


def _walk_descriptors(data: bytes, start: int, end: int):
    position = start
    while position + 2 <= end:
        tag = data[position]
        length, payload = _descriptor_length(data, position + 1, end)
        payload_end = payload + length
        if payload_end > end:
            payload_end = end
        yield tag, payload, payload_end
        if tag == 0x03 and payload + 3 <= payload_end:
            flags = data[payload + 2]
            nested = payload + 3
            if flags & 0x80:
                nested += 2
            if flags & 0x40 and nested < payload_end:
                nested += 1 + data[nested]
            if flags & 0x20:
                nested += 2
            yield from _walk_descriptors(data, nested, payload_end)
        elif tag == 0x04 and payload + 13 <= payload_end:
            yield from _walk_descriptors(data, payload + 13, payload_end)
        position = payload_end


def parse_esds(data: bytes) -> CodecInfo:
    info = CodecInfo(codec="aac")
    if len(data) < 4:
        return info
    # esds is a full box; descriptors follow version/flags.
    for tag, payload, payload_end in _walk_descriptors(data, 4, len(data)):
        if tag == 0x04 and payload + 13 <= payload_end:
            info.object_type = data[payload]
        elif tag == 0x05:
            object_type, sample_rate, channels = parse_audio_specific_config(data[payload:payload_end])
            info.object_type = info.object_type or object_type
            info.sample_rate = sample_rate
            info.channels = channels
    return info


def parse_mp4a_entry(data: bytes, entry_payload_start: int = 0) -> CodecInfo:
    """Parse channel/sample-rate fields and optionally a nested esds payload."""

    info = CodecInfo(codec="aac")
    if len(data) >= 28:
        version = struct.unpack_from(">H", data, 8)[0]
        info.channels = struct.unpack_from(">H", data, 16)[0] or None
        rate_fixed = struct.unpack_from(">I", data, 24)[0]
        info.sample_rate = rate_fixed >> 16 or None
        child_offset = 28
        if version == 1:
            child_offset += 16
        elif version == 2:
            child_offset += 36
    return info
