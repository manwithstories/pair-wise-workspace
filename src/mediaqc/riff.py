"""RIFF AVI/WAV traversal with odd-byte padding and truncation checks."""

from __future__ import annotations

import struct
from dataclasses import dataclass
from pathlib import Path

from .model import Finding, ProbeResult, Track


@dataclass(slots=True)
class Chunk:
    kind: bytes
    offset: int
    size: int
    data_offset: int
    end: int
    path: tuple[bytes, ...] = ()
    list_form: bytes = b""


@dataclass(slots=True)
class RiffScan:
    chunks: list[Chunk]
    findings: list[Finding]
    declared_end: int
    file_size: int
    form: bytes


def scan_riff(path: str | Path) -> RiffScan:
    file_path = Path(path)
    file_size = file_path.stat().st_size
    findings: list[Finding] = []
    chunks: list[Chunk] = []
    with file_path.open("rb") as handle:
        header = handle.read(12)
        if len(header) < 12:
            return RiffScan([], [Finding("TRUNCATED_RIFF", "error", "truncated RIFF header",
                                         f"got {len(header)} bytes", offset=0)], 0, file_size, b"")
        _riff, declared, form = struct.unpack("<4sI4s", header)
        declared_end = 8 + declared
        if declared_end > file_size:
            findings.append(Finding("TRUNCATED_RIFF", "error", "RIFF size exceeds file size",
                                    f"declared_end={declared_end} actual={file_size}", offset=4))
        elif file_size > declared_end:
            findings.append(Finding("TAIL_GARBAGE", "warning", "bytes after RIFF payload",
                                    f"{file_size - declared_end} trailing byte(s)", offset=declared_end))
        scan_end = min(declared_end, file_size)
        try:
            _walk(handle, 12, scan_end, 0, (), chunks, findings)
        except (struct.error, ValueError) as exc:
            findings.append(Finding("TRUNCATED_RIFF", "error", "RIFF chunk traversal failed",
                                    str(exc), offset=min(12, file_size)))
    return RiffScan(chunks, findings, declared_end, file_size, form)


def _walk(handle, start: int, end: int, depth: int, path: tuple[bytes, ...],
          chunks: list[Chunk], findings: list[Finding]) -> None:
    if depth > 8:
        raise ValueError("RIFF nesting exceeds 8 levels")
    position = start
    while position < end:
        if end - position < 8:
            findings.append(Finding("RIFF_PADDING_ERROR", "warning", "trailing RIFF bytes",
                                    f"{end - position} byte(s)", offset=position))
            return
        handle.seek(position)
        kind, size = struct.unpack("<4sI", handle.read(8))
        data_offset = position + 8
        data_end = data_offset + size
        padded_end = data_end + (size & 1)
        if data_end > end:
            findings.append(Finding("TRUNCATED_CHUNK", "error", "RIFF chunk exceeds container",
                                    f"{kind!r} declares {size} bytes", offset=position))
            return
        chunk = Chunk(kind, position, size, data_offset, data_end, path)
        chunks.append(chunk)
        if kind == b"LIST" and size >= 4:
            handle.seek(data_offset)
            form = handle.read(4)
            chunk.list_form = form
            _walk(handle, data_offset + 4, data_end, depth + 1, path + (form,), chunks, findings)
        position = padded_end


def _record(handle, chunk: Chunk, limit: int = 256) -> bytes:
    handle.seek(chunk.data_offset)
    return handle.read(min(chunk.size, limit))


def probe_riff(path: str | Path, container: str) -> ProbeResult:
    file_path = Path(path)
    result = ProbeResult(str(file_path), container, file_path.stat().st_size)
    scan = scan_riff(file_path)
    result.findings.extend(scan.findings)
    with file_path.open("rb") as handle:
        if container == "wav":
            _parse_wav(handle, scan, result)
        else:
            _parse_avi(handle, scan, result)
    return result


def _parse_wav(handle, scan: RiffScan, result: ProbeResult) -> None:
    fmt_chunk = next((item for item in scan.chunks if item.kind == b"fmt "), None)
    data_chunk = next((item for item in scan.chunks if item.kind == b"data"), None)
    if fmt_chunk is None:
        result.findings.append(Finding("FMT_MISSING", "error", "WAV fmt chunk missing",
                                       "no fmt chunk", offset=12))
        return
    data = _record(handle, fmt_chunk)
    if len(data) < 16:
        result.findings.append(Finding("TRUNCATED_CHUNK", "error", "WAV fmt chunk truncated",
                                       f"{len(data)} bytes", offset=fmt_chunk.offset))
        return
    tag, channels, rate, byte_rate, block_align, bits = struct.unpack("<HHIIHH", data[:16])
    codec = _pcm_codec(tag, bits)
    duration = data_chunk.size / byte_rate * 1000 if data_chunk and byte_rate else 0.0
    samples = data_chunk.size // block_align if data_chunk and block_align else 0
    track = Track(
        index=0, track_id=0, kind="audio", codec=codec, timescale=rate,
        duration=samples, duration_ms=duration, presentation_duration_ms=duration,
        sample_count=samples, sample_rate=rate, channels=channels,
        sample_entry=f"fmt=0x{tag:04x}",
        table_facts={"format_tag": tag, "bits_per_sample": bits,
                     "byte_rate": byte_rate, "block_align": block_align},
    )
    result.tracks.append(track)
    result.metadata.update({"format": codec, "bits_per_sample": bits,
                            "byte_rate": byte_rate, "data_bytes": data_chunk.size if data_chunk else 0})
    if data_chunk is None:
        result.findings.append(Finding("DATA_CHUNK_MISSING", "error", "WAV data chunk missing",
                                       "no data chunk", offset=fmt_chunk.offset))
    elif data_chunk.size & 1:
        result.findings.append(Finding("ODD_DATA_CHUNK", "info", "odd-sized data chunk padded",
                                       f"data_size={data_chunk.size}", 0, data_chunk.offset))
    if channels == 0 or rate == 0 or byte_rate == 0:
        result.findings.append(Finding("AUDIO_PARAM_ZERO", "error", "invalid WAV audio parameters",
                                       f"channels={channels} rate={rate} byte_rate={byte_rate}",
                                       0, fmt_chunk.offset))
    if len(data) >= 18:
        extra = struct.unpack("<H", data[16:18])[0]
        result.metadata["fmt_extension_bytes"] = extra


def _parse_avi(handle, scan: RiffScan, result: ProbeResult) -> None:
    avih = next((item for item in scan.chunks if item.kind == b"avih"), None)
    avih_record = _record(handle, avih, 56) if avih else b""
    if len(avih_record) < 40:
        result.findings.append(Finding("AVIH_MISSING", "error", "AVI avih chunk missing",
                                       "cannot read main header", offset=12))
        return
    micros, total_frames, _streams = struct.unpack("<III", avih_record[:12])
    width, height = struct.unpack("<II", avih_record[32:40])
    result.metadata.update({"microseconds_per_frame": micros, "total_frames": total_frames,
                            "width": width, "height": height})
    strl_chunks = [item for item in scan.chunks if item.list_form == b"strl"]
    declared_streams = struct.unpack("<I", avih_record[24:28])[0]
    if declared_streams != len(strl_chunks):
        result.findings.append(Finding("STREAM_COUNT_MISMATCH", "warning",
                                       "avih stream count differs from strl count",
                                       f"declared={declared_streams} parsed={len(strl_chunks)}",
                                       offset=avih.data_offset + 24))
    for index, strl in enumerate(strl_chunks):
        children = [item for item in scan.chunks
                    if item.offset > strl.data_offset and item.end <= strl.end]
        strh = next((item for item in children if item.kind == b"strh"), None)
        strf = next((item for item in children if item.kind == b"strf"), None)
        if strh is None or strf is None:
            result.findings.append(Finding("TRACK_PARSE_ERROR", "error", "AVI stream missing strh/strf",
                                           f"index={index}", index, strl.offset))
            continue
        header = _record(handle, strh, 56)
        if len(header) < 48:
            result.findings.append(Finding("TRUNCATED_CHUNK", "error", "AVI strh truncated",
                                           f"{len(header)} bytes", index, strh.offset))
            continue
        fcc_type, fcc_handler = header[:4], header[4:8]
        scale, rate, _start, length = struct.unpack("<IIII", header[20:36])
        sample_size = struct.unpack("<I", header[44:48])[0]
        kind = "video" if fcc_type == b"vids" else "audio" if fcc_type == b"auds" else "unknown"
        duration_ticks = length * scale if scale else 0
        duration_ms = duration_ticks / rate * 1000 if rate else 0.0
        track = Track(index=index, track_id=index, kind=kind,
                      codec=fcc_handler.decode("latin-1", "replace").strip() or "unknown",
                      timescale=rate, duration=duration_ticks, duration_ms=duration_ms,
                      presentation_duration_ms=duration_ms, sample_count=length,
                      sample_entry=fcc_handler.decode("latin-1", "replace"))
        track.table_facts = {"scale": scale, "rate": rate, "sample_size": sample_size,
                             "strh_offset": strh.offset}
        format_data = _record(handle, strf, 64)
        if kind == "video" and len(format_data) >= 20:
            track.width, track.height = struct.unpack("<ii", format_data[4:12])
            bits = struct.unpack("<H", format_data[14:16])[0]
            compression = format_data[16:20].decode("latin-1", "replace").strip("\x00 ")
            track.codec = compression or track.codec
            track.table_facts["bits_per_pixel"] = bits
            track.codec_details = {"compression": track.codec, "bits_per_pixel": bits}
        elif kind == "audio" and len(format_data) >= 16:
            tag, channels, rate_hz, byte_rate, _align, bits = struct.unpack(
                "<HHIIHH", format_data[:16]
            )
            track.sample_rate, track.channels = rate_hz, channels
            track.codec = _pcm_codec(tag, bits)
            track.codec_details = {"format_tag": tag, "bits_per_sample": bits,
                                   "byte_rate": byte_rate}
        if kind == "audio" and scale and rate:
            track.duration_ms = length * scale / rate * 1000
            track.presentation_duration_ms = track.duration_ms
        if rate == 0 or scale == 0:
            result.findings.append(Finding("TIMESCALE_ZERO", "error", "AVI stream timing is zero",
                                           f"scale={scale} rate={rate}", index,
                                           strh.data_offset + 20))
        result.tracks.append(track)
    if micros == 0 and any(track.kind == "video" for track in result.tracks):
        result.findings.append(Finding("TIMESCALE_ZERO", "error", "AVI frame duration is zero",
                                       "avih.dwMicroSecPerFrame=0", offset=avih.data_offset))


def _pcm_codec(tag: int, bits: int) -> str:
    if tag == 1:
        return f"pcm_s{bits or 16}le"
    if tag == 3:
        return f"pcm_f{bits or 32}le"
    if tag == 0xFFFE:
        return f"pcm_extensible_{bits}le"
    return f"wav_format_0x{tag:04x}"
