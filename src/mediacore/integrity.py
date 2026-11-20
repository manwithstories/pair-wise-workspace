"""Independent structural integrity check and file hashing.

Deliberately does **not** use libavformat: a truncated file that libavformat
refuses to open still has readable top-level box headers, and "does the last
box fit inside the file" is the cleanest truncation signal we have.
"""

from __future__ import annotations

import hashlib
import struct
from pathlib import Path
from typing import Any

def structural_integrity(path: str | Path) -> dict[str, Any]:
    """Walk ISO-BMFF boxes without libavformat and report basic integrity.

    This is deliberately a second, independent opinion: if the file is not
    MP4/MOV (e.g. WAV) it returns ``{"iso_bmff": False}`` rather than failing.
    For MP4 it checks that every top-level box fits inside the file and that
    ``moov`` exists and precedes-or-follows ``mdat`` consistently, which
    catches the truncation cases libavformat refuses to open at all.
    """
    path = str(path)
    data_size = Path(path).stat().st_size
    boxes = _walk_top_level(path, data_size)
    if not boxes:
        return {"iso_bmff": False, "file_size": data_size, "boxes": []}
    types = [b["type"] for b in boxes]
    if "ftyp" not in types:
        return {"iso_bmff": False, "file_size": data_size, "boxes": boxes}
    complete = all(b["complete"] for b in boxes)
    return {
        "iso_bmff": True,
        "file_size": data_size,
        "boxes": boxes,
        "complete": complete,
        "has_moov": "moov" in types,
        "has_mdat": "mdat" in types,
        "declared_size": sum(b["size"] for b in boxes),
    }


def _walk_top_level(path: str, file_size: int) -> list[dict[str, Any]]:
    import struct

    out: list[dict[str, Any]] = []
    with open(path, "rb") as fh:
        offset = 0
        while offset + 8 <= file_size:
            header = fh.read(16)
            if len(header) < 8:
                break
            size = struct.unpack(">I", header[:4])[0]
            ftype = header[4:8].decode("latin1", "replace")
            header_len = 8
            if size == 1:
                if len(header) < 16:
                    break
                size = struct.unpack(">Q", header[8:16])[0]
                header_len = 16
            elif size == 0:
                size = file_size - offset
            if size < header_len:
                break
            complete = offset + size <= file_size
            out.append(
                {
                    "type": ftype,
                    "offset": offset,
                    "size": size,
                    "complete": complete,
                }
            )
            if not complete:
                break
            offset += size
            fh.seek(offset)
    return out


def file_sha256(path: str | Path, *, chunk: int = 1 << 20) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as fh:
        while block := fh.read(chunk):
            digest.update(block)
    return digest.hexdigest()

