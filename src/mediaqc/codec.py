"""Codec configuration parsing: avcC, hvcC, esds and SPS bitstreams."""

from __future__ import annotations

import struct
from dataclasses import dataclass, field
from typing import Any

from .model import Finding

AAC_RATES = [
    96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050,
    16000, 12000, 11025, 8000, 7350,
]


class BitError(ValueError):
    pass


class BitReader:
    def __init__(self, data: bytes) -> None:
        self.data = data
        self.bit = 0

    def read(self, count: int) -> int:
        if count < 0 or self.bit + count > len(self.data) * 8:
            raise BitError("bitstream exhausted")
        value = 0
        for _ in range(count):
            byte = self.data[self.bit >> 3]
            value = (value << 1) | ((byte >> (7 - (self.bit & 7))) & 1)
            self.bit += 1
        return value

    def ue(self) -> int:
        zeros = 0
        while self.read(1) == 0:
            zeros += 1
            if zeros > 31:
                raise BitError("Exp-Golomb code too long")
        return (1 << zeros) - 1 + (self.read(zeros) if zeros else 0)

    def se(self) -> int:
        value = self.ue()
        return (value + 1) // 2 if value & 1 else -(value // 2)


def unescape_rbsp(data: bytes) -> bytes:
    out = bytearray()
    zero_run = 0
    for byte in data:
        if zero_run >= 2 and byte == 3:
            zero_run = 0
            continue
        out.append(byte)
        zero_run = zero_run + 1 if byte == 0 else 0
    return bytes(out)


@dataclass(slots=True)
class CodecInfo:
    entry_type: str
    codec: str
    width: int | None = None
    height: int | None = None
    sample_rate: int | None = None
    channels: int | None = None
    details: dict[str, Any] = field(default_factory=dict)
    findings: list[Finding] = field(default_factory=list)


def parse_avc_sps(nal: bytes) -> dict[str, Any]:
    data = unescape_rbsp(nal[1:] if nal and (nal[0] & 0x1F) == 7 else nal)
    bits = BitReader(data)
    profile_idc = bits.read(8)
    constraints = bits.read(8)
    level_idc = bits.read(8)
    sps_id = bits.ue()
    chroma_format_idc = 1
    separate_colour_plane = 0
    bit_depth_luma = 8
    bit_depth_chroma = 8
    if profile_idc in {100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135}:
        chroma_format_idc = bits.ue()
        if chroma_format_idc == 3:
            separate_colour_plane = bits.read(1)
        bit_depth_luma = bits.ue() + 8
        bit_depth_chroma = bits.ue() + 8
        bits.read(1)
        if bits.read(1):
            count = 8 if chroma_format_idc != 3 else 12
            for index in range(count):
                if bits.read(1):
                    _skip_scaling_list(bits, 16 if index < 6 else 64)
    log2_max_frame_num = bits.ue() + 4
    pic_order_cnt_type = bits.ue()
    if pic_order_cnt_type == 0:
        bits.ue()
    elif pic_order_cnt_type == 1:
        bits.read(1)
        bits.se()
        bits.se()
        for _ in range(bits.ue()):
            bits.se()
    bits.ue()
    bits.read(1)
    width_mbs = bits.ue() + 1
    height_units = bits.ue() + 1
    frame_mbs_only = bits.read(1)
    if not frame_mbs_only:
        bits.read(1)
    bits.read(1)
    crop = [0, 0, 0, 0]
    if bits.read(1):
        crop = [bits.ue(), bits.ue(), bits.ue(), bits.ue()]
    crop_x = 1 if chroma_format_idc in {0, 3} else 2
    crop_y = (2 - frame_mbs_only) * (1 if chroma_format_idc in {0, 3} else 2)
    width = width_mbs * 16 - crop_x * (crop[0] + crop[1])
    height = (2 - frame_mbs_only) * height_units * 16 - crop_y * (crop[2] + crop[3])
    return {
        "sps_id": sps_id,
        "profile_idc": profile_idc,
        "constraints_hex": f"{constraints:02x}",
        "level_idc": level_idc,
        "chroma_format_idc": chroma_format_idc,
        "separate_colour_plane": separate_colour_plane,
        "bit_depth_luma": bit_depth_luma,
        "bit_depth_chroma": bit_depth_chroma,
        "log2_max_frame_num": log2_max_frame_num,
        "pic_order_cnt_type": pic_order_cnt_type,
        "frame_mbs_only": frame_mbs_only,
        "width": width,
        "height": height,
    }


def _skip_scaling_list(bits: BitReader, size: int) -> None:
    last = 8
    for _ in range(size):
        if last != 0:
            last = (last + bits.se() + 256) % 256


def parse_avcc(data: bytes, entry_offset: int = 0) -> tuple[dict[str, Any], list[Finding]]:
    findings: list[Finding] = []
    if len(data) < 7:
        return {}, [Finding("CODEC_PARSE_ERROR", "warning", "truncated avcC",
                            f"{len(data)} bytes", offset=entry_offset)]
    details: dict[str, Any] = {
        "version": data[0],
        "profile_idc": data[1],
        "constraints_hex": f"{data[2]:02x}",
        "level_idc": data[3],
        "nal_length_size": (data[4] & 3) + 1,
        "sps": [],
        "pps_count": 0,
    }
    position = 6
    sps_count = data[5] & 0x1F
    for _ in range(sps_count):
        if position + 2 > len(data):
            findings.append(Finding("CODEC_PARSE_ERROR", "warning", "truncated SPS list",
                                    "avcC SPS length missing", offset=entry_offset + position))
            break
        length = struct.unpack(">H", data[position:position + 2])[0]
        position += 2
        nal = data[position:position + length]
        position += length
        if len(nal) != length:
            findings.append(Finding("CODEC_PARSE_ERROR", "warning", "truncated SPS NAL",
                                    f"declared={length} actual={len(nal)}",
                                    offset=entry_offset + position - len(nal)))
            break
        try:
            info = parse_avc_sps(nal)
            info["nal_length"] = length
            details["sps"].append(info)
        except (BitError, IndexError, ValueError) as exc:
            findings.append(Finding("CODEC_PARSE_ERROR", "warning", "SPS parse failed",
                                    str(exc), offset=entry_offset + position - length))
    if position < len(data):
        pps_count = data[position]
        details["pps_count"] = pps_count
        position += 1
        for _ in range(pps_count):
            if position + 2 > len(data):
                break
            length = struct.unpack(">H", data[position:position + 2])[0]
            position += 2 + length
    if details["sps"]:
        first = details["sps"][0]
        details["width"] = first.get("width")
        details["height"] = first.get("height")
        details["codec_string"] = (
            f"avc1.{first['profile_idc']:02X}{first['constraints_hex']}{first['level_idc']:02X}"
        )
    return details, findings


def parse_esds(data: bytes, entry_offset: int = 0) -> tuple[dict[str, Any], list[Finding]]:
    details: dict[str, Any] = {"descriptors": [], "aac": None}
    findings: list[Finding] = []
    start = 4 if len(data) >= 4 else 0
    _walk_descriptors(data, start, len(data), details, entry_offset, findings, depth=0)
    return details, findings


def _read_desc_length(data: bytes, position: int) -> tuple[int, int]:
    length = 0
    for _ in range(4):
        if position >= len(data):
            raise BitError("truncated descriptor length")
        byte = data[position]
        position += 1
        length = (length << 7) | (byte & 0x7F)
        if not byte & 0x80:
            return length, position
    raise BitError("descriptor length exceeds 4 bytes")


def _walk_descriptors(data, start, end, details, base_offset, findings, depth) -> None:
    if depth > 8:
        return
    position = start
    while position + 2 <= end:
        tag = data[position]
        try:
            length, body = _read_desc_length(data, position + 1)
        except BitError as exc:
            findings.append(Finding("CODEC_PARSE_ERROR", "warning", "bad esds descriptor",
                                    str(exc), offset=base_offset + position))
            return
        stop = min(body + length, end)
        details["descriptors"].append({"tag": tag, "offset": body, "length": length})
        if tag == 0x04 and stop - body >= 13:
            details["object_type_indication"] = data[body]
            details["stream_type"] = data[body + 1] >> 2
            _walk_descriptors(data, body + 13, stop, details, base_offset, findings, depth + 1)
        elif tag == 0x05:
            try:
                details["aac"] = parse_audio_specific_config(data[body:stop])
                details["aac"]["offset"] = body
            except (BitError, IndexError, ValueError) as exc:
                findings.append(Finding("CODEC_PARSE_ERROR", "warning",
                                        "AAC AudioSpecificConfig parse failed", str(exc),
                                        offset=base_offset + body))
        elif tag == 0x03 and stop - body >= 3:
            _walk_descriptors(data, body + 3, stop, details, base_offset, findings, depth + 1)
        elif tag == 0x06:
            _walk_descriptors(data, body, stop, details, base_offset, findings, depth + 1)
        if stop <= body:
            break
        position = stop


def parse_audio_specific_config(data: bytes) -> dict[str, Any]:
    bits = BitReader(data)
    object_type = bits.read(5)
    if object_type == 31:
        object_type = 32 + bits.read(6)
    rate_index = bits.read(4)
    sample_rate = bits.read(24) if rate_index == 15 else (
        AAC_RATES[rate_index] if rate_index < len(AAC_RATES) else None
    )
    channels = bits.read(4)
    return {
        "object_type": object_type,
        "sample_rate_index": rate_index,
        "sample_rate": sample_rate,
        "channels": channels,
    }


def parse_hevc_sps(nal: bytes) -> dict[str, Any]:
    data = unescape_rbsp(nal[2:] if len(nal) > 2 else b"")
    bits = BitReader(data)
    vps_id = bits.read(4)
    max_sub_layers = bits.read(3)
    bits.read(1)
    profile_space = bits.read(2)
    tier = bits.read(1)
    profile_idc = bits.read(5)
    bits.read(32)
    bits.read(48)
    level_idc = bits.read(8)
    profile_present: list[int] = []
    level_present: list[int] = []
    for _ in range(max_sub_layers):
        profile_present.append(bits.read(1))
        level_present.append(bits.read(1))
    if max_sub_layers > 0:
        for _ in range(max_sub_layers, 8):
            bits.read(2)
    for index in range(max_sub_layers):
        if profile_present[index]:
            bits.read(88)
        if level_present[index]:
            bits.read(8)
    sps_id = bits.ue()
    chroma_format_idc = bits.ue()
    if chroma_format_idc == 3:
        bits.read(1)
    width = bits.ue()
    height = bits.ue()
    left = right = top = bottom = 0
    if bits.read(1):
        left, right, top, bottom = bits.ue(), bits.ue(), bits.ue(), bits.ue()
    sub_width = 2 if chroma_format_idc in {1, 2} else 1
    sub_height = 2 if chroma_format_idc == 1 else 1
    return {
        "sps_id": sps_id,
        "vps_id": vps_id,
        "profile_space": profile_space,
        "tier": tier,
        "profile_idc": profile_idc,
        "level_idc": level_idc,
        "chroma_format_idc": chroma_format_idc,
        "width": width - sub_width * (left + right),
        "height": height - sub_height * (top + bottom),
    }


def parse_hvcc(data: bytes, entry_offset: int = 0) -> tuple[dict[str, Any], list[Finding]]:
    findings: list[Finding] = []
    if len(data) < 23:
        return {}, [Finding("CODEC_PARSE_ERROR", "warning", "truncated hvcC",
                            f"{len(data)} bytes", offset=entry_offset)]
    profile_space = data[1] >> 6
    tier = (data[1] >> 5) & 1
    profile_idc = data[1] & 0x1F
    level_idc = data[12]
    details: dict[str, Any] = {
        "version": data[0], "profile_space": profile_space, "tier": tier,
        "profile_idc": profile_idc, "level_idc": level_idc,
        "chroma_format_idc": data[16] & 3,
        "bit_depth_luma": (data[17] & 7) + 8,
        "bit_depth_chroma": (data[18] & 7) + 8,
        "sps": [], "vps_count": 0, "pps_count": 0,
    }
    position = 23
    array_count = data[22]
    for _ in range(array_count):
        if position + 3 > len(data):
            break
        header = data[position]
        nal_type = header & 0x3F
        count = struct.unpack(">H", data[position + 1:position + 3])[0]
        position += 3
        if nal_type == 32:
            details["vps_count"] += count
        elif nal_type == 34:
            details["pps_count"] += count
        for _ in range(count):
            if position + 2 > len(data):
                break
            length = struct.unpack(">H", data[position:position + 2])[0]
            position += 2
            nal = data[position:position + length]
            position += length
            if len(nal) != length:
                findings.append(Finding("CODEC_PARSE_ERROR", "warning", "truncated HEVC NAL",
                                        f"declared={length} actual={len(nal)}",
                                        offset=entry_offset + position - len(nal)))
                break
            if nal_type == 33:
                try:
                    info = parse_hevc_sps(nal)
                    info["nal_length"] = length
                    details["sps"].append(info)
                except (BitError, IndexError, ValueError) as exc:
                    findings.append(Finding("CODEC_PARSE_ERROR", "warning", "HEVC SPS parse failed",
                                            str(exc), offset=entry_offset + position - length))
    if details["sps"]:
        details["width"] = details["sps"][0].get("width")
        details["height"] = details["sps"][0].get("height")
        details["codec_string"] = f"hvc1.{profile_space}.{profile_idc}.L{level_idc}"
    return details, findings


def _nested_boxes(data: bytes, start: int, base_offset: int):
    position = start
    while position + 8 <= len(data):
        size, kind = struct.unpack(">I4s", data[position:position + 8])
        header = 8
        if size == 1:
            if position + 16 > len(data):
                break
            size = struct.unpack(">Q", data[position + 8:position + 16])[0]
            header = 16
        elif size == 0:
            size = len(data) - position
        if size < header or position + size > len(data):
            break
        yield kind, data[position + header:position + size], base_offset + position + header
        position += size


def parse_sample_entry(handle, box, track_index: int) -> CodecInfo:
    handle.seek(box.payload_offset)
    data = handle.read(box.payload_size)
    entry = box.type.decode("latin-1", "replace")
    info = CodecInfo(entry_type=entry, codec=entry)
    children: dict[bytes, tuple[bytes, int]] = {}
    if entry in {"avc1", "avc3", "hvc1", "hev1", "encv"} and len(data) >= 78:
        info.width = struct.unpack(">H", data[24:26])[0]
        info.height = struct.unpack(">H", data[26:28])[0]
        child_start = 78
    elif entry in {"mp4a", "enca", "ac-3", "ec-3"} and len(data) >= 28:
        version = struct.unpack(">H", data[8:10])[0]
        info.channels = struct.unpack(">H", data[16:18])[0]
        info.sample_rate = struct.unpack(">I", data[24:28])[0] >> 16
        child_start = 28 + (16 if version == 1 else 36 if version == 2 else 0)
    else:
        child_start = len(data)
    for kind, payload, absolute in _nested_boxes(data, child_start, box.payload_offset):
        children[kind] = (payload, absolute)
    if b"wave" in children:
        wave_payload, wave_offset = children[b"wave"]
        for kind, nested, absolute in _nested_boxes(wave_payload, 0, wave_offset):
            children.setdefault(kind, (nested, absolute))
    if entry in {"encv", "enca"}:
        info.codec = "encrypted"
        info.findings.append(Finding(
            "UNSUPPORTED_ENCRYPTION", "error", "encrypted sample entry is unsupported",
            f"sample entry {entry}", track_index, box.offset,
        ))
    if entry in {"avc1", "avc3"}:
        if b"avcC" not in children:
            info.findings.append(Finding("CODEC_PARAM_MISSING", "error", "AVC parameter sets missing",
                                         "avcC not present", track_index, box.offset))
        else:
            details, findings = parse_avcc(children[b"avcC"][0], children[b"avcC"][1])
            info.details = details
            info.findings.extend(_with_track(findings, track_index))
            info.width = details.get("width", info.width)
            info.height = details.get("height", info.height)
            if not details.get("sps"):
                info.findings.append(Finding("CODEC_PARAM_MISSING", "error", "AVC SPS missing",
                                             "avcC contains no SPS", track_index,
                                             children[b"avcC"][1]))
            info.codec = details.get("codec_string", entry)
    elif entry in {"hvc1", "hev1"}:
        if b"hvcC" not in children:
            info.findings.append(Finding("CODEC_PARAM_MISSING", "error", "HEVC parameter sets missing",
                                         "hvcC not present", track_index, box.offset))
        else:
            details, findings = parse_hvcc(children[b"hvcC"][0], children[b"hvcC"][1])
            info.details = details
            info.findings.extend(_with_track(findings, track_index))
            info.width = details.get("width", info.width)
            info.height = details.get("height", info.height)
            if not details.get("sps"):
                info.findings.append(Finding("CODEC_PARAM_MISSING", "error", "HEVC SPS missing",
                                             "hvcC contains no SPS", track_index,
                                             children[b"hvcC"][1]))
            info.codec = details.get("codec_string", entry)
    elif entry in {"mp4a", "enca"}:
        if b"esds" not in children:
            info.findings.append(Finding("CODEC_PARAM_MISSING", "error", "AAC configuration missing",
                                         "esds not present", track_index, box.offset))
        else:
            details, findings = parse_esds(children[b"esds"][0], children[b"esds"][1])
            info.details = details
            info.findings.extend(_with_track(findings, track_index))
            aac = details.get("aac") or {}
            info.sample_rate = aac.get("sample_rate") or info.sample_rate
            info.channels = aac.get("channels") or info.channels
            if aac:
                info.codec = f"mp4a.40.{aac.get('object_type', 2)}"
            else:
                info.findings.append(Finding("CODEC_PARAM_MISSING", "error", "AAC ASC missing",
                                             "esds has no DecoderSpecificInfo", track_index,
                                             children[b"esds"][1]))
    return info


def _with_track(findings: list[Finding], track_index: int) -> list[Finding]:
    for finding in findings:
        finding.track_index = track_index
    return findings
