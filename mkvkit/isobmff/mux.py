"""Remux Matroska frames into MP4.

Two-pass strategy: demux builds the frame model, timing tables are
derived, then ftyp + moov (with placeholder chunk offsets) are written
before mdat is streamed sample-by-sample from the input.  Chunk offsets
are backfilled into the stco/co64 tables afterwards, and the result is
re-verified before reporting success.
"""
from __future__ import annotations

import os
import struct

from ..mkv.demux import demux
from ..timing.rewrite import build_timing
from . import boxes
from .verify import verify_mp4

MOVIE_TIMESCALE = 1000


class SelfCheckError(Exception):
    """The written MP4 failed its read-back verification (exit code 5)."""


def _track_timescale(track) -> int:
    if track.kind == "video":
        return 90000
    if track.kind == "audio":
        return track.sample_rate or 48000
    return 1000


def _stsd(track) -> bytes:
    if track.kind == "video":
        return boxes.stsd_avc1(track.width, track.height, track.codec_private)
    if track.kind == "audio":
        return boxes.stsd_mp4a(track.channels, track.sample_rate,
                               track.codec_private)
    return boxes.stsd_tx3g()


def _handler(track):
    if track.kind == "video":
        return boxes.vmhd(), boxes.hdlr(b"vide", "VideoHandler")
    if track.kind == "audio":
        return boxes.smhd(), boxes.hdlr(b"soun", "SoundHandler")
    return boxes.nmhd(), boxes.hdlr(b"text", "TextHandler")


def _group_chunks(frames):
    """Group frame indices into chunks, one per source cluster."""
    chunks = []
    for idx, frame in enumerate(frames):
        if chunks and frames[chunks[-1][-1]].cluster_index == frame.cluster_index:
            chunks[-1].append(idx)
        else:
            chunks.append([idx])
    return chunks


def _stsc_entries(chunks) -> list:
    entries = []
    for i, chunk in enumerate(chunks):
        if not entries or entries[-1][1] != len(chunk):
            entries.append((i + 1, len(chunk), 1))
    return entries


def _sample_size(track, frame) -> int:
    # Matroska S_TEXT/UTF8 blocks contain raw text.  A tx3g sample is a
    # 16-bit big-endian byte count followed by the UTF-8 text.
    return frame.size + 2 if track.kind == "subtitle" else frame.size


def _write_sample(out, src, track, frame):
    src.seek(frame.offset)
    data = src.read(frame.size)
    if len(data) != frame.size:
        raise SelfCheckError(f"short read on sample at {frame.offset}")
    if track.kind == "subtitle":
        if len(data) > 0xFFFF:
            raise SelfCheckError("subtitle sample exceeds tx3g limit")
        data = struct.pack(">H", len(data)) + data
    out.write(data)


def _build_trak(track, frames, timing, chunks, co64, empty_edit_ms):
    stbl_prefix = b"".join(
        [
            _stsd(track),
            boxes.stts(timing.stts),
            boxes.ctts(timing.ctts_table) if timing.ctts_table else b"",
            boxes.stss([i + 1 for i, f in enumerate(frames) if f.keyframe])
            if not all(f.keyframe for f in frames)
            else b"",
            boxes.stsc(_stsc_entries(chunks)),
            boxes.stsz([_sample_size(track, f) for f in frames]),
        ]
    )
    stco_box = boxes.stco([0] * len(chunks), co64)
    stbl = boxes.box(b"stbl", stbl_prefix + stco_box)
    media_header, handler = _handler(track)
    minf = boxes.box(b"minf", media_header + boxes.dinf() + stbl)
    mdia = boxes.box(
        b"mdia",
        boxes.mdhd(timing.timescale, timing.duration_ticks, track.language)
        + handler
        + minf,
    )
    segment_duration = timing.duration_ticks * MOVIE_TIMESCALE // timing.timescale
    edts = boxes.edts(timing.elst_media_time, segment_duration, empty_edit_ms)
    volume = 0x0100 if track.kind == "audio" else 0
    track_duration = empty_edit_ms + segment_duration
    tkhd = boxes.tkhd(track.number, track_duration, track.width,
                      track.height, volume)
    trak_payload = tkhd + edts + mdia
    # offset of the stco/co64 entry area within the trak box
    stco_rel = (
        8 + len(tkhd) + len(edts)
        + 8 + len(boxes.mdhd(timing.timescale, timing.duration_ticks, track.language)) + len(handler)
        + 8 + len(media_header) + len(boxes.dinf())
        + 8 + len(stbl_prefix) + 16
    )
    return boxes.box(b"trak", trak_payload), stco_rel


def remux(input_path, output_path) -> dict:
    parent = os.path.dirname(os.fspath(output_path))
    if parent:
        os.makedirs(parent, exist_ok=True)
    result = demux(input_path)
    issues = list(result.issues)
    repair_count = 0
    prepared = []
    for track in result.tracks:
        frames = result.frames.get(track.number, [])
        if not frames:
            continue
        pts_ns = [f.pts_ns for f in frames]
        timescale = _track_timescale(track)
        timing = build_timing(pts_ns, timescale)
        if timing.repairs:
            repair_count += timing.repairs
            issues.append(
                f"track {track.number}: {timing.repairs} timestamp repairs"
            )
        chunks = _group_chunks(frames)
        prepared.append((track, frames, timing, chunks))
    if not prepared:
        raise SelfCheckError("no frames to mux")

    start_ns = {
        track.number: timing.first_pts_ticks * 1_000_000_000 // timing.timescale
        for track, _, timing, _ in prepared
    }
    origin_ns = min(start_ns.values())
    empty_edits = {
        track.number: max(0, (start_ns[track.number] - origin_ns) // 1_000_000)
        for track, _, _, _ in prepared
    }
    movie_duration = max(
        empty_edits[track.number]
        + t.duration_ticks * MOVIE_TIMESCALE // t.timescale
        for track, _, t, _ in prepared
    )
    next_track_id = max(track.number for track, _, _, _ in prepared) + 1
    total_samples = sum(len(frames) for _, frames, _, _ in prepared)
    total_bytes = sum(
        _sample_size(track, f)
        for track, frames, _, _ in prepared
        for f in frames
    )

    co64 = False
    for _ in range(2):  # moov size depends on stco width; iterate once
        ftyp = boxes.ftyp()
        trak_blobs = []
        stco_patches = []
        cursor = len(ftyp) + 8 + len(
            boxes.mvhd(MOVIE_TIMESCALE, movie_duration, next_track_id)
        )
        for track, frames, timing, chunks in prepared:
            trak, rel = _build_trak(track, frames, timing, chunks, co64,
                                    empty_edits[track.number])
            stco_patches.append(cursor + rel)
            cursor += len(trak)
            trak_blobs.append(trak)
        moov_size = 8 + sum(
            [len(boxes.mvhd(MOVIE_TIMESCALE, movie_duration, next_track_id))]
            + [len(t) for t in trak_blobs]
        )
        mdat_header_len = 8 if total_bytes + 8 < 0x100000000 else 16
        max_offset = len(ftyp) + moov_size + mdat_header_len + total_bytes
        need_co64 = max_offset >= 0xFFFFFFFF
        if need_co64 == co64:
            break
        co64 = need_co64

    mvhd = boxes.mvhd(MOVIE_TIMESCALE, movie_duration, next_track_id)
    moov = boxes.box(b"moov", mvhd + b"".join(trak_blobs))

    chunk_offsets: list[list[int]] = [[] for _ in prepared]
    with open(input_path, "rb") as src, open(output_path, "wb") as out:
        out.write(ftyp)
        out.write(moov)
        out.write(boxes.mdat_header(total_bytes))
        cluster_ids = sorted(
            {f.cluster_index for _, frames, _, _ in prepared for f in frames}
        )
        for cluster_index in cluster_ids:
            for t_idx, (track, frames, timing, chunks) in enumerate(prepared):
                for chunk in chunks:
                    if frames[chunk[0]].cluster_index != cluster_index:
                        continue
                    chunk_offsets[t_idx].append(out.tell())
                    for frame_idx in chunk:
                        _write_sample(out, src, track, frames[frame_idx])
        for patch_pos, offsets in zip(stco_patches, chunk_offsets):
            out.seek(patch_pos)
            if co64:
                out.write(b"".join(struct.pack(">Q", o) for o in offsets))
            else:
                out.write(b"".join(struct.pack(">I", o) for o in offsets))

    try:
        check = verify_mp4(output_path)
    except Exception as exc:
        raise SelfCheckError(f"self-check failed: {exc}") from exc
    if not check["ok"]:
        raise SelfCheckError(f"self-check failed: {check}")

    track_reports = []
    for (track, frames, timing, chunks) in prepared:
        track_reports.append(
            {
                "track": track.number,
                "codec": track.mp4_codec,
                "timescale": timing.timescale,
                "frames": len(frames),
                "first_pts_ns": timing.first_pts_ticks * 1_000_000_000
                // timing.timescale,
                "last_pts_ns": timing.last_pts_ticks * 1_000_000_000
                // timing.timescale,
            }
        )
    av_delta = 0.0
    video = next((t for t in prepared if t[0].mp4_codec == "avc1"), None)
    audio = next((t for t in prepared if t[0].mp4_codec == "mp4a"), None)
    if video is not None and audio is not None:
        av_delta = abs(
            float(empty_edits[video[0].number] - empty_edits[audio[0].number])
        )
    return {
        "input": str(input_path),
        "output": str(output_path),
        "tracks": track_reports,
        "av_start_delta_ms": round(av_delta, 3),
        "co64_used": co64,
        "issues": len(result.issues) + repair_count,
        "issue_details": issues,
        "samples_written": total_samples,
    }
