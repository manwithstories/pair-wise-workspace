"""Demux packet timestamps and frame-type flags."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

import av

from .container import MediaOpenError, open_video


@dataclass(frozen=True)
class PacketRecord:
    index: int
    dts: int | None
    pts: int | None
    keyframe: bool
    pict_type: str | None = None
    size: int = 0
    duration: int | None = None
    discard: bool = False

    @property
    def is_keyframe(self) -> bool:
        return self.keyframe


class PacketList(list[PacketRecord]):
    """A list with demux status attached for partial/truncated reads."""

    def __init__(self) -> None:
        super().__init__()
        self.truncated = False
        self.error: str | None = None


class _BitReader:
    def __init__(self, data: bytes) -> None:
        self.data = data
        self.bit = 0

    def read_bit(self) -> int:
        if self.bit >= len(self.data) * 8:
            raise EOFError("slice header ended early")
        byte = self.data[self.bit // 8]
        value = (byte >> (7 - self.bit % 8)) & 1
        self.bit += 1
        return value

    def read_ue(self) -> int:
        zeros = 0
        while self.read_bit() == 0:
            zeros += 1
            if zeros > 31:
                raise ValueError("invalid Exp-Golomb value")
        value = 1
        for _ in range(zeros):
            value = (value << 1) | self.read_bit()
        return value - 1


def _remove_emulation_bytes(data: bytes) -> bytes:
    output = bytearray()
    zeroes = 0
    for value in data:
        if zeroes >= 2 and value == 3:
            zeroes = 0
            continue
        output.append(value)
        zeroes = zeroes + 1 if value == 0 else 0
    return bytes(output)


def _annexb_nals(data: bytes) -> list[bytes] | None:
    starts: list[tuple[int, int]] = []
    pos = 0
    while True:
        found = data.find(b"\x00\x00\x01", pos)
        if found < 0:
            break
        prefix = 4 if found > 0 and data[found - 1] == 0 else 3
        starts.append((found, found + prefix))
        pos = found + prefix
    if not starts:
        return None
    units = []
    for i, (_, payload) in enumerate(starts):
        end = starts[i + 1][0] if i + 1 < len(starts) else len(data)
        if end > payload:
            units.append(data[payload:end])
    return units


def _length_prefixed_nals(data: bytes) -> list[bytes] | None:
    for width in (4, 3):
        units: list[bytes] = []
        pos = 0
        while pos + width <= len(data):
            size = int.from_bytes(data[pos : pos + width], "big")
            pos += width
            if size <= 0 or pos + size > len(data):
                units = []
                break
            units.append(data[pos : pos + size])
            pos += size
        if units and pos == len(data):
            return units
    return None


def _h264_pict_type(packet: av.Packet) -> str | None:
    data = bytes(packet)
    units = _annexb_nals(data) or _length_prefixed_nals(data) or []
    for unit in units:
        nal_type = unit[0] & 0x1F
        if nal_type == 5:
            return "I"
        if nal_type not in (1, 2):
            continue
        try:
            reader = _BitReader(_remove_emulation_bytes(unit[1:]))
            reader.read_ue()
            slice_type = reader.read_ue() % 5
        except (EOFError, ValueError):
            continue
        if slice_type in (2, 4):
            return "I"
        if slice_type in (0, 3):
            return "P"
        if slice_type == 1:
            return "B"
    return None


def _h265_pict_type(packet: av.Packet) -> str | None:
    data = bytes(packet)
    units = _annexb_nals(data) or _length_prefixed_nals(data) or []
    for unit in units:
        if len(unit) < 2:
            continue
        nal_type = (unit[0] >> 1) & 0x3F
        if 16 <= nal_type <= 21:
            return "I"
    return None


def detect_pict_type(packet: av.Packet, codec_name: str) -> str | None:
    if codec_name in {"h264", "avc1"}:
        return _h264_pict_type(packet)
    if codec_name in {"hevc", "h265", "hev1"}:
        return _h265_pict_type(packet)
    return "I" if packet.is_keyframe else None


def extract_packets(path: str | Path, stream_index: int | None = None) -> PacketList:
    """Extract packet metadata in decode order, retaining partial results on EOF errors."""

    records = PacketList()
    try:
        with open_video(path, stream_index) as (container, stream, _):
            codec_name = stream.codec_context.name or "unknown"
            for packet in container.demux(stream):
                if packet.size == 0:
                    continue
                frame_index = len(records)
                records.append(
                    PacketRecord(
                        index=frame_index,
                        dts=packet.dts,
                        pts=packet.pts,
                        keyframe=bool(packet.is_keyframe),
                        pict_type=detect_pict_type(packet, codec_name),
                        size=packet.size,
                        duration=packet.duration,
                        discard=bool(packet.is_discard),
                    )
                )
    except MediaOpenError:
        raise
    except av.error.FFmpegError as exc:
        records.truncated = bool(records)
        records.error = str(exc)
    return records
