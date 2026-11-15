"""Container sniffing without decoding media payloads."""

from __future__ import annotations

import struct
from dataclasses import dataclass
from pathlib import Path

SUPPORTED = {"mp4", "mov", "avi", "wav"}
_BMFF_BRANDS = {
    b"isom": "mp4", b"iso2": "mp4", b"mp41": "mp4", b"mp42": "mp4",
    b"iso4": "mp4", b"iso5": "mp4", b"iso6": "mp4", b"avc1": "mp4",
    b"M4V ": "mp4", b"M4A ": "mp4", b"M4B ": "mp4", b"M4P ": "mp4",
    b"dash": "mp4", b"mmp4": "mp4", b"MSNV": "mp4", b"NDAS": "mp4",
    b"F4V ": "mp4", b"qt  ": "mov", b"moov": "mov",
}


class SniffError(ValueError):
    def __init__(self, message: str, code: str = "unsupported_container") -> None:
        super().__init__(message)
        self.code = code


@dataclass(slots=True)
class SniffResult:
    container: str
    offset: int = 0
    brand: str | None = None
    iods: bool = False
    reason: str = ""


def sniff_file(path: str | Path) -> SniffResult:
    file_path = Path(path)
    try:
        size = file_path.stat().st_size
        with file_path.open("rb") as handle:
            head = handle.read(16)
            if _is_riff(head):
                handle.seek(8)
                form = handle.read(4)
                if form == b"AVI ":
                    return SniffResult("avi", reason="RIFF AVI form")
                if form == b"WAVE":
                    return SniffResult("wav", reason="RIFF WAVE form")
                raise SniffError(f"unsupported RIFF form {form!r}")
            return _sniff_bmff(handle, size)
    except SniffError:
        raise
    except OSError as exc:
        raise SniffError(str(exc), code="io_error") from exc


def _is_riff(head: bytes) -> bool:
    return len(head) >= 12 and head[:4] == b"RIFF" and head[8:12] in {b"AVI ", b"WAVE"}


def _sniff_bmff(handle, file_size: int) -> SniffResult:
    offset = 0
    scanned = 0
    brand: str | None = None
    while offset + 8 <= file_size and scanned < 64:
        handle.seek(offset)
        header = handle.read(8)
        if len(header) < 8:
            break
        size32, box_type = struct.unpack(">I4s", header)
        header_size = 8
        if size32 == 1:
            ext = handle.read(8)
            if len(ext) < 8:
                break
            box_size = struct.unpack(">Q", ext)[0]
            header_size = 16
        elif size32 == 0:
            box_size = file_size - offset
        else:
            box_size = size32
        if box_size < header_size or offset + box_size > file_size:
            break
        if box_type == b"ftyp":
            payload = handle.read(min(box_size - header_size, 64))
            if len(payload) >= 8:
                major = payload[:4]
                brand = major.decode("latin-1", "replace")
                if major in _BMFF_BRANDS:
                    return SniffResult(_BMFF_BRANDS[major], offset, brand, reason="ftyp brand")
                compatible = [payload[i:i + 4] for i in range(8, len(payload), 4)]
                for item in compatible:
                    if item in _BMFF_BRANDS:
                        return SniffResult(_BMFF_BRANDS[item], offset, brand, reason="ftyp compatible brand")
                raise SniffError(f"unsupported_container: ftyp brand {brand}")
        if box_type == b"moov":
            kind = "mov" if brand and _BMFF_BRANDS.get(brand.encode("latin-1")) == "mov" else "mp4"
            return SniffResult(kind, offset=0, brand=brand, reason="top-level moov")
        if box_type not in {b"free", b"skip", b"wide", b"uuid"}:
            break
        offset += box_size
        scanned += 1
    raise SniffError("unsupported_container")
