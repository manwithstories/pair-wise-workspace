"""Fragmented MP4 export split at keyframes with moof/mdat pairs."""

from __future__ import annotations

import os
import struct
import tempfile
from dataclasses import dataclass

from mkvtomp4.mp4.boxes import box, full_box, u32, u64
from mkvtomp4.mp4.export_plain import ExportError, _ftyp
from mkvtomp4.mp4.index import tfdt_box
from mkvtomp4.mp4.moov import build_moov
from mkvtomp4.timing.normalize import NormalizedMedia, NormalizedTrack


@dataclass
class FragmentChunk:
    track: NormalizedTrack
    start: int
    end: int

    @property
    def first_dts(self) -> int:
        return int(self.track.dts[self.start])


def _chunks(track: NormalizedTrack, target_units: int) -> list[FragmentChunk]:
    if track.sample_count == 0:
        return []
    key_positions = set(int(index) for index in track.keyframes.nonzero()[0])
    starts = [0]
    last_start = 0
    for index in range(1, track.sample_count):
        elapsed = int(track.dts[index] - track.dts[last_start])
        can_split = index in key_positions if key_positions else True
        if can_split and elapsed >= target_units:
            starts.append(index)
            last_start = index
    chunks = []
    for position, start in enumerate(starts):
        end = starts[position + 1] if position + 1 < len(starts) else track.sample_count
        chunks.append(FragmentChunk(track, start, end))
    return chunks


def _sample_flags(keyframe: bool) -> int:
    return 0x02000000 if keyframe else 0x01010000


def _traf(chunk: FragmentChunk, data_offset: int) -> bytes:
    track = chunk.track
    tfhd = full_box("tfhd", 0, 0x020000, u32(track.track.mp4_track_id))
    trun_payload = u32(chunk.end - chunk.start) + int(data_offset).to_bytes(4, "big", signed=True)
    for index in range(chunk.start, chunk.end):
        duration = int(track.durations[index])
        size = int(track.sizes[index])
        flags = _sample_flags(bool(track.keyframes[index]))
        trun_payload += u32(duration) + u32(size) + u32(flags)
    trun = full_box("trun", 0, 0x701, trun_payload)
    return box("traf", tfhd + tfdt_box(chunk.first_dts) + trun)


def _moof(sequence: int, chunk: FragmentChunk, data_offset: int) -> bytes:
    mfhd = full_box("mfhd", 0, 0, u32(sequence))
    return box("moof", mfhd + _traf(chunk, data_offset))


def _write_sample(source, output, offset: int, size: int) -> None:
    source.seek(offset)
    remaining = size
    while remaining:
        chunk = source.read(min(remaining, 1 << 20))
        if not chunk:
            raise ExportError(f"source ended while reading sample at {offset}")
        output.write(chunk)
        remaining -= len(chunk)


def export_fragmented(
    media: NormalizedMedia,
    out_path: str,
    *,
    frag_duration: float = 2.0,
) -> dict[str, object]:
    if frag_duration <= 0:
        raise ExportError("fragment duration must be positive")
    os.makedirs(os.path.dirname(os.path.abspath(out_path)), exist_ok=True)
    ftyp = _ftyp()
    empty_offsets = {track.track.mkv_track: [] for track in media.tracks}
    moov = build_moov(media, empty_offsets, fragmented=True)
    chunks: list[FragmentChunk] = []
    for track in media.tracks:
        target = max(1, int(round(track.track.timescale * frag_duration)))
        chunks.extend(_chunks(track, target))
    chunks.sort(key=lambda chunk: (chunk.first_dts, chunk.track.track.mp4_track_id))
    descriptor, temp_name = tempfile.mkstemp(
        prefix=".mkvtomp4-", suffix=".mp4", dir=os.path.dirname(os.path.abspath(out_path))
    )
    total_size = len(ftyp) + len(moov)
    sequence = 0
    try:
        with os.fdopen(descriptor, "wb") as output, open(media.source.path, "rb") as source:
            output.write(ftyp)
            output.write(moov)
            for chunk in chunks:
                payload_size = sum(
                    int(size) for size in chunk.track.sizes[chunk.start : chunk.end]
                )
                moof_stub = _moof(sequence, chunk, 0)
                data_offset = len(moof_stub) + 8
                moof = _moof(sequence, chunk, data_offset)
                if len(moof) != len(moof_stub):
                    raise ExportError("moof size changed while patching data offset")
                output.write(moof)
                output.write(struct.pack(">I4s", 8 + payload_size, b"mdat"))
                for index in range(chunk.start, chunk.end):
                    _write_sample(
                        source,
                        output,
                        int(chunk.track.source_offsets[index]),
                        int(chunk.track.sizes[index]),
                    )
                total_size += len(moof) + 8 + payload_size
                sequence += 1
            output.flush()
            os.fsync(output.fileno())
        os.replace(temp_name, out_path)
    except BaseException:
        try:
            os.unlink(temp_name)
        except FileNotFoundError:
            pass
        raise
    return {
        "path": out_path,
        "layout": "fragmented",
        "fragment_count": len(chunks),
        "frag_duration": frag_duration,
        "size": total_size,
    }
