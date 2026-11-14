from __future__ import annotations

import os
import struct
from dataclasses import dataclass, field
from typing import BinaryIO

from .errors import ContainerError, MediaError


WAVE_FORMAT_PCM = 0x0001
WAVE_FORMAT_IEEE_FLOAT = 0x0003
WAVE_FORMAT_EXTENSIBLE = 0xFFFE
CODEC_NAMES = {
    WAVE_FORMAT_PCM: "pcm",
    WAVE_FORMAT_IEEE_FLOAT: "pcm_float",
    0x0002: "adpcm",
    0x0011: "adpcm",
}


@dataclass
class WavInfo:
    path: str
    file_size: int
    format_tag: int
    codec: str
    channels: int
    sample_rate: int
    bits_per_sample: int
    block_align: int
    byte_rate: int
    data_size: int
    duration_ms: int
    data_offset: int
    fmt_offset: int
    chunks: list[dict] = field(default_factory=list)
    extensible_subformat: int | None = None
    valid_bits: int | None = None


def _read_exact(fh: BinaryIO, offset: int, count: int) -> bytes:
    fh.seek(offset)
    data = fh.read(count)
    if len(data) != count:
        raise MediaError(
            "FILE_TRUNCATED",
            f"expected {count} bytes at offset {offset}, got {len(data)}",
            offset=offset,
        )
    return data


def parse_wav(source: str | os.PathLike[str] | BinaryIO) -> WavInfo:
    owns_file = not hasattr(source, "read")
    if owns_file:
        try:
            fh = open(source, "rb")
        except PermissionError as exc:
            raise MediaError("PERMISSION_DENIED", str(exc), path=str(source), exit_code=2) from exc
        except OSError as exc:
            raise MediaError("IO_ERROR", str(exc), path=str(source), exit_code=2) from exc
    else:
        fh = source  # type: ignore[assignment]
    try:
        fh.seek(0, os.SEEK_END)
        file_size = fh.tell()
        if file_size < 12:
            raise MediaError("FILE_TRUNCATED", "WAV header is shorter than 12 bytes", offset=0)
        header = _read_exact(fh, 0, 12)
        if header[:4] != b"RIFF" or header[8:12] != b"WAVE":
            raise ContainerError("MAGIC_UNKNOWN", "RIFF/WAVE signature not found", offset=0)
        declared_end = 8 + struct.unpack_from("<I", header, 4)[0]
        if declared_end > file_size:
            raise MediaError(
                "FILE_TRUNCATED",
                f"RIFF declares end {declared_end}, file has {file_size}",
                offset=4,
            )
        fmt: dict | None = None
        data_size: int | None = None
        data_offset: int | None = None
        chunks: list[dict] = []
        position = 12
        limit = min(declared_end, file_size)
        while position + 8 <= limit:
            chunk_header = _read_exact(fh, position, 8)
            chunk_id = chunk_header[:4].decode("latin-1", "replace")
            chunk_size = struct.unpack_from("<I", chunk_header, 4)[0]
            payload = position + 8
            payload_end = payload + chunk_size
            padded_end = payload_end + (chunk_size & 1)
            if payload_end > file_size or padded_end > file_size:
                raise MediaError(
                    "FILE_TRUNCATED",
                    f"chunk {chunk_id} extends past file end",
                    offset=position,
                )
            chunks.append({"id": chunk_id, "offset": position, "size": chunk_size, "data_offset": payload})
            if chunk_id == "fmt ":
                if chunk_size < 16:
                    raise ContainerError("WAV_MALFORMED", "fmt chunk is shorter than 16 bytes", offset=position)
                raw = _read_exact(fh, payload, min(chunk_size, 40))
                format_tag, channels, sample_rate, byte_rate, block_align, bits = struct.unpack_from(
                    "<HHIIHH", raw, 0
                )
                extensible_subformat = None
                valid_bits = None
                if format_tag == WAVE_FORMAT_EXTENSIBLE:
                    if chunk_size < 40:
                        raise ContainerError(
                            "WAV_MALFORMED",
                            "extensible fmt chunk is shorter than 40 bytes",
                            offset=position,
                        )
                    cb_size = struct.unpack_from("<H", raw, 16)[0]
                    if cb_size < 22:
                        raise ContainerError("WAV_MALFORMED", "invalid extensible cbSize", offset=position)
                    valid_bits = struct.unpack_from("<H", raw, 18)[0]
                    extensible_subformat = struct.unpack_from("<H", raw, 24)[0]
                fmt = {
                    "format_tag": format_tag,
                    "channels": channels,
                    "sample_rate": sample_rate,
                    "byte_rate": byte_rate,
                    "block_align": block_align,
                    "bits_per_sample": bits,
                    "extensible_subformat": extensible_subformat,
                    "valid_bits": valid_bits,
                    "offset": position,
                }
            elif chunk_id == "data":
                if data_size is None:
                    data_size = chunk_size
                    data_offset = payload
            position = padded_end
        if fmt is None:
            raise ContainerError("WAV_MALFORMED", "required fmt chunk is missing", offset=12)
        if data_size is None or data_offset is None:
            raise ContainerError("WAV_MALFORMED", "required data chunk is missing", offset=12)
        effective_tag = fmt["extensible_subformat"] or fmt["format_tag"]
        codec = CODEC_NAMES.get(effective_tag, f"wave_{effective_tag:#x}")
        byte_rate = fmt["byte_rate"]
        if byte_rate > 0:
            duration_ms = int((data_size * 1000 + byte_rate // 2) // byte_rate)
        elif fmt["sample_rate"] and fmt["block_align"]:
            duration_ms = int((data_size // fmt["block_align"]) * 1000 / fmt["sample_rate"])
        else:
            duration_ms = 0
        return WavInfo(
            path=str(source) if owns_file else getattr(source, "name", "<stream>"),
            file_size=file_size,
            format_tag=fmt["format_tag"],
            codec=codec,
            channels=fmt["channels"],
            sample_rate=fmt["sample_rate"],
            bits_per_sample=fmt["bits_per_sample"],
            block_align=fmt["block_align"],
            byte_rate=byte_rate,
            data_size=data_size,
            duration_ms=duration_ms,
            data_offset=data_offset,
            fmt_offset=fmt["offset"],
            chunks=chunks,
            extensible_subformat=fmt["extensible_subformat"],
            valid_bits=fmt["valid_bits"],
        )
    finally:
        if owns_file:
            fh.close()
