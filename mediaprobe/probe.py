from __future__ import annotations

import hashlib
import os
import stat
from dataclasses import dataclass
from typing import BinaryIO

from .errors import EXIT_IO, MediaError
from .isobmff import IsoInfo, SampleRange, TrackInfo, parse_isobmff
from .riff import WavInfo, parse_wav


ISO_TOP_LEVEL_TYPES = {
    b"ftyp",
    b"styp",
    b"moov",
    b"moof",
    b"mdat",
    b"free",
    b"skip",
    b"wide",
    b"uuid",
}


@dataclass
class MediaAsset:
    path: str
    container: str
    format: str
    size_bytes: int
    sha256: str
    duration_ms: int
    fragmented: bool
    brand: str
    tracks: list[TrackInfo]
    brand_list: list[str] | None = None

    @property
    def track_count(self) -> int:
        return len(self.tracks)

    def to_dict(self) -> dict:
        return {
            "path": self.path,
            "container": self.container,
            "format": self.format,
            "size_bytes": self.size_bytes,
            "sha256": self.sha256,
            "duration_ms": self.duration_ms,
            "track_count": self.track_count,
            "fragmented": self.fragmented,
            "brand": self.brand,
            "brands": self.brand_list or [],
            "tracks": [track_to_dict(track) for track in self.tracks],
        }


def track_to_dict(track: TrackInfo) -> dict:
    return {
        "track_id": track.track_id,
        "handler": track.handler,
        "handler_type": track.handler_type,
        "codec": track.codec,
        "duration_ms": track.duration_ms,
        "timescale": track.timescale,
        "language": track.language,
        "profile": track.profile,
        "level": track.level,
        "sample_rate": track.sample_rate,
        "channels": track.channels,
        "width": track.width,
        "height": track.height,
        "sample_count": track.sample_count,
        "stsd_count": track.stsd_count,
        "stsd_actual": track.stsd_actual,
        "duration_source": track.duration_source,
        "fragment_error": track.fragment_error,
        "base_media_decode_time": track.base_media_decode_time,
    }


def file_sha256(path: str | os.PathLike[str], chunk_size: int = 1024 * 1024) -> str:
    digest = hashlib.sha256()
    try:
        with open(path, "rb") as fh:
            while True:
                chunk = fh.read(chunk_size)
                if not chunk:
                    break
                digest.update(chunk)
    except PermissionError as exc:
        raise MediaError("PERMISSION_DENIED", str(exc), path=str(path), exit_code=EXIT_IO) from exc
    except OSError as exc:
        raise MediaError("IO_ERROR", str(exc), path=str(path), exit_code=EXIT_IO) from exc
    return digest.hexdigest()


def _read_magic(fh: BinaryIO) -> bytes:
    fh.seek(0)
    return fh.read(12)


def _detect_container(magic: bytes) -> str:
    if len(magic) >= 12 and magic[:4] == b"RIFF" and magic[8:12] == b"WAVE":
        return "wav"
    if len(magic) >= 8 and magic[4:8] in ISO_TOP_LEVEL_TYPES:
        return "isobmff"
    raise MediaError("MAGIC_UNKNOWN", "unrecognized container magic", offset=0)


def _wav_asset(info: WavInfo, path: str, digest: str) -> MediaAsset:
    track = TrackInfo(
        track_id=1,
        handler_type="soun",
        handler="audio",
        codec=info.codec,
        duration_ms=info.duration_ms,
        timescale=info.sample_rate,
        sample_rate=info.sample_rate,
        channels=info.channels,
        sample_count=max(0, info.data_size // info.block_align) if info.block_align else 0,
        has_stbl_samples=True,
        duration_source="data",
        duration_box_path="RIFF/data",
        duration_box_offset=info.data_offset,
        codec_box_path="RIFF/fmt ",
        codec_box_offset=info.fmt_offset,
        chunk_box_path="RIFF/data",
        chunk_box_offset=info.data_offset,
        sample_ranges=[SampleRange(info.data_offset, info.data_size, info.duration_ms)],
    )
    return MediaAsset(
        path=path,
        container="wav",
        format="wav",
        size_bytes=info.file_size,
        sha256=digest,
        duration_ms=info.duration_ms,
        fragmented=False,
        brand="",
        tracks=[track],
    )


def _iso_asset(info: IsoInfo, path: str, digest: str) -> MediaAsset:
    return MediaAsset(
        path=path,
        container="isobmff",
        format=info.format,
        size_bytes=info.file_size,
        sha256=digest,
        duration_ms=info.duration_ms,
        fragmented=info.fragmented,
        brand=info.brand,
        brand_list=info.brands,
        tracks=info.tracks,
    )


def probe_file(path: str | os.PathLike[str]) -> MediaAsset:
    display_path = os.fspath(path)
    try:
        file_stat = os.stat(display_path)
    except PermissionError as exc:
        raise MediaError("PERMISSION_DENIED", str(exc), path=display_path, exit_code=EXIT_IO) from exc
    except FileNotFoundError as exc:
        raise MediaError("IO_ERROR", str(exc), path=display_path, exit_code=EXIT_IO) from exc
    except OSError as exc:
        raise MediaError("IO_ERROR", str(exc), path=display_path, exit_code=EXIT_IO) from exc
    if not stat.S_ISREG(file_stat.st_mode):
        raise MediaError("IO_ERROR", "not a regular file", path=display_path, exit_code=EXIT_IO)
    if file_stat.st_size == 0:
        raise MediaError("FILE_EMPTY", "file has zero bytes", path=display_path, offset=0)
    try:
        with open(display_path, "rb") as fh:
            magic = _read_magic(fh)
    except PermissionError as exc:
        raise MediaError("PERMISSION_DENIED", str(exc), path=display_path, exit_code=EXIT_IO) from exc
    except OSError as exc:
        raise MediaError("IO_ERROR", str(exc), path=display_path, exit_code=EXIT_IO) from exc
    if len(magic) < 8:
        raise MediaError("FILE_TRUNCATED", "file is shorter than a container header", path=display_path, offset=0)
    container = _detect_container(magic)
    digest = file_sha256(display_path)
    if container == "wav":
        return _wav_asset(parse_wav(display_path), display_path, digest)
    return _iso_asset(parse_isobmff(display_path), display_path, digest)
