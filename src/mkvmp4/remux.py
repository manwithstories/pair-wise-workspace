"""Matroska to faststart MP4 remuxer (metadata-first, streaming payloads)."""

from __future__ import annotations

import json
import os
import statistics
import tempfile
from dataclasses import dataclass, field
from pathlib import Path
from typing import BinaryIO

from mkvmp4.ebml.reader import EBMLReader
from mkvmp4.mkv.parser import ParsedBlock, ParsedMedia, parse_matroska
from mkvmp4.mkv.timeline import TimelineError, expand_laces, ns_to_units
from mkvmp4.mp4.boxes import (
    BoxBuffer, audio_sample_entry, box, full_box, i16, language_code, matrix_identity,
    text_sample_entry, u16, u24, u32, u64, u8, visual_sample_entry, write_mdat_header,
)
from mkvmp4.mp4.stbl import SampleMeta, build_stbl


class RemuxError(RuntimeError):
    pass


class UnsupportedCodecError(RemuxError):
    pass


class RepairLimitError(RemuxError):
    pass


CODEC_MAP = {
    "V_MPEG4/ISO/AVC": ("avc1", "vide"),
    "A_AAC": ("mp4a", "soun"),
    "S_TEXT/UTF8": ("tx3g", "sbtl"),
}

OUTPUT_TIMESCALE = 1_000_000


@dataclass(slots=True)
class RawSample:
    track_number: int
    cluster_index: int
    source_index: int
    block_base_ns: int
    pts_ns: int
    duration_ns: int
    keyframe: bool
    reordered: bool
    temp_offset: int
    size: int
    lace_index: int = 0
    lace_count: int = 1
    explicit_total_ns: int | None = None


@dataclass(slots=True)
class TrackPlan:
    source_track: object
    track_id: int
    entry_type: str
    handler: str
    samples: list[RawSample] = field(default_factory=list)
    ordered_samples: list[RawSample] = field(default_factory=list)
    sample_meta: list[SampleMeta] = field(default_factory=list)
    duration_units: int = 0
    first_pts_units: int = 0
    last_pts_units: int = 0
    edit_media_time: int = 0


@dataclass(slots=True)
class RepairResult:
    duplicate: int = 0
    backward: int = 0
    gap: int = 0
    repaired: int = 0
    total_samples: int = 0
    mode: str = "clamp"

    @property
    def ratio(self) -> float:
        return self.repaired / self.total_samples if self.total_samples else 0.0

    def as_dict(self) -> dict[str, object]:
        return {
            "duplicate": self.duplicate,
            "backward": self.backward,
            "gap": self.gap,
            "repaired": self.repaired,
            "total_samples": self.total_samples,
            "ratio": round(self.ratio, 8),
            "mode": self.mode,
        }


def _track_codec(track: object) -> tuple[str, str]:
    codec_id = str(getattr(track, "codec_id", ""))
    entry_type, handler = CODEC_MAP.get(codec_id, (None, None))
    if entry_type is None:
        for prefix, mapped in CODEC_MAP.items():
            if codec_id.startswith(prefix + "/"):
                entry_type, handler = mapped
                break
    if entry_type is None:
        raise UnsupportedCodecError(
            f"unsupported codec {codec_id!r} on track {getattr(track, 'number', '?')}")
    return entry_type, handler


def map_tracks(media: ParsedMedia) -> dict[int, TrackPlan]:
    plans: dict[int, TrackPlan] = {}
    for track in media.tracks:
        entry_type, handler = _track_codec(track)
        if track.number in plans:
            raise RemuxError(f"duplicate Matroska track number {track.number}")
        plans[track.number] = TrackPlan(track, track.number, entry_type, handler)
    if not plans:
        raise RemuxError("input has no tracks")
    return plans


class _SampleExtractor:
    def __init__(self, reader: EBMLReader, media: ParsedMedia, temp: BinaryIO,
                 plans: dict[int, TrackPlan]):
        self.reader = reader
        self.media = media
        self.temp = temp
        self.plans = plans
        self.written = 0

    def extract(self) -> None:
        scale = self.media.info.timestamp_scale_ns
        clusters = self.media.clusters
        block_seq = {number: 0 for number in self.plans}
        for parsed in self.media.blocks:
            if parsed.track_number not in self.plans:
                raise RemuxError(f"block references unknown track {parsed.track_number}")
            cluster = clusters[parsed.cluster_index]
            base_ns = (cluster.timestamp_ticks + parsed.relative_ticks) * scale
            self.reader.seek(parsed.payload_offset)
            block_data = self.reader.read(parsed.payload_size)
            try:
                header, frames = expand_laces(block_data)
            except TimelineError as exc:
                raise RemuxError(f"invalid block: {exc}") from exc
            if header.track_number != parsed.track_number:
                raise RemuxError("block track number changed while parsing")
            count = len(frames)
            explicit_ns = parsed.duration_ticks * scale if parsed.duration_ticks else None
            plan = self.plans[parsed.track_number]
            for lace_index, frame in enumerate(frames):
                if plan.entry_type == "tx3g":
                    text = bytes(frame).rstrip(b"\x00")
                    sample_data = u16(min(len(text), 0xFFFF)) + text[:0xFFFF]
                else:
                    sample_data = bytes(frame)
                size = len(sample_data)
                temp_offset = self.written
                self.temp.write(sample_data)
                self.written += size
                plan.samples.append(RawSample(
                    track_number=parsed.track_number,
                    cluster_index=parsed.cluster_index,
                    source_index=block_seq[parsed.track_number],
                    block_base_ns=base_ns,
                    pts_ns=base_ns,
                    duration_ns=0,
                    keyframe=parsed.keyframe and not parsed.reference_blocks,
                    reordered=bool(parsed.reference_blocks),
                    temp_offset=temp_offset,
                    size=size,
                    lace_index=lace_index,
                    lace_count=count,
                    explicit_total_ns=explicit_ns,
                ))
            block_seq[parsed.track_number] += 1

    def resolve_timing(self) -> None:
        for plan in self.plans.values():
            default_ns = getattr(plan.source_track, "default_duration_ns", None)
            samples = plan.samples
            index = 0
            previous_duration = 1_000_000
            while index < len(samples):
                first = samples[index]
                count = max(1, first.lace_count)
                group = samples[index:index + count]
                if first.explicit_total_ns:
                    total = max(1, first.explicit_total_ns)
                    starts = [first.block_base_ns + (i * total + count // 2) // count
                              for i in range(count)]
                    boundaries = starts[1:] + [first.block_base_ns + total]
                elif default_ns:
                    frame_duration = int(default_ns)
                    starts = [first.block_base_ns + i * frame_duration for i in range(count)]
                    boundaries = [start + frame_duration for start in starts]
                elif index + count < len(samples):
                    span = max(1, samples[index + count].block_base_ns - first.block_base_ns)
                    frame_duration = max(1, span // count)
                    starts = [first.block_base_ns + i * frame_duration for i in range(count)]
                    boundaries = [start + frame_duration for start in starts]
                else:
                    frame_duration = previous_duration
                    starts = [first.block_base_ns + i * frame_duration for i in range(count)]
                    boundaries = [start + frame_duration for start in starts]
                for sample, start, end in zip(group, starts, boundaries):
                    sample.pts_ns = start
                    sample.duration_ns = max(1, end - start)
                if group:
                    previous_duration = group[-1].duration_ns
                index += count


def _repair_track(plan: TrackPlan, mode: str, result: RepairResult) -> None:
    samples = plan.samples
    if not samples:
        return
    first = samples[0].pts_ns
    out = [first]
    for index in range(1, len(samples)):
        sample = samples[index]
        delta = sample.pts_ns - samples[index - 1].pts_ns
        threshold = max(1, max(sample.duration_ns, samples[index - 1].duration_ns) * 3 // 2)
        issue = None
        next_reordered = index + 1 < len(samples) and samples[index + 1].reordered
        if sample.reordered or samples[index - 1].reordered or next_reordered:
            issue = None
        elif delta == 0:
            issue = "duplicate"
        elif delta < 0:
            issue = "backward"
        elif delta > threshold:
            issue = "gap"
        if issue:
            setattr(result, issue, getattr(result, issue) + 1)
            result.repaired += 1
        if sample.reordered and mode == "clamp":
            out.append(sample.pts_ns)
        elif mode == "resample" or issue:
            step = max(1, sample.duration_ns)
            out.append(out[-1] + step)
        else:
            step = max(1, delta)
            out.append(out[-1] + step)
    if mode == "resample":
        step = max(1, int(statistics.median(s.duration_ns for s in samples)))
        out = [first + index * step for index in range(len(samples))]
    for sample, pts in zip(samples, out):
        sample.pts_ns = pts - first
    if samples[0].duration_ns <= 0:
        samples[0].duration_ns = max(1, out[1] - out[0] if len(out) > 1 else 1)
    plan.duration_units = sum(ns_to_units(s.duration_ns, OUTPUT_TIMESCALE) for s in samples)


def rebuild_timeline(plans: dict[int, TrackPlan], mode: str,
                     max_repair_ratio: float) -> RepairResult:
    if mode not in {"clamp", "resample", "fail"}:
        raise RemuxError(f"unknown repair mode {mode}")
    result = RepairResult(mode=mode)
    for plan in plans.values():
        _repair_track(plan, mode, result)
        result.total_samples += len(plan.samples)
    if mode == "fail" and result.repaired:
        raise RepairLimitError(
            f"timeline anomalies detected ({result.repaired}); --repair=fail")
    if result.ratio > max_repair_ratio:
        raise RepairLimitError(
            f"repair ratio {result.ratio:.4f} exceeds {max_repair_ratio:.4f}")
    return result


def _sample_meta(plan: TrackPlan, samples: list[RawSample]) -> list[SampleMeta]:
    metas: list[SampleMeta] = []
    cumulative_ns = 0
    previous_dts = 0
    for sample in samples:
        dts = previous_dts
        cumulative_ns += max(1, sample.duration_ns)
        next_dts = ns_to_units(cumulative_ns, OUTPUT_TIMESCALE)
        duration = max(1, next_dts - previous_dts)
        cts = ns_to_units(sample.pts_ns, OUTPUT_TIMESCALE)
        metas.append(SampleMeta(0, sample.size, dts, cts, duration,
                                sample.keyframe, sample.cluster_index))
        previous_dts = next_dts
    return metas


def _layout_samples(plans: dict[int, TrackPlan], cluster_count: int) -> None:
    for plan in plans.values():
        ordered = sorted(plan.samples, key=lambda sample: (sample.cluster_index, sample.source_index))
        plan.ordered_samples = ordered
        plan.sample_meta = _sample_meta(plan, ordered)
        plan.first_pts_units = plan.sample_meta[0].cts if plan.sample_meta else 0
        plan.last_pts_units = plan.sample_meta[-1].cts if plan.sample_meta else 0
        if plan.sample_meta:
            final = plan.sample_meta[-1]
            plan.duration_units = final.dts + final.duration
        min_offset = min((meta.cts_offset for meta in plan.sample_meta), default=0)
        plan.edit_media_time = -min_offset if min_offset < 0 else 0


def _ftyp() -> bytes:
    return box("ftyp", b"isom" + u32(0x200) + b"isomiso2avc1mp41")


def _mvhd(duration: int, next_track_id: int) -> bytes:
    payload = (u32(0) + u32(0) + u32(OUTPUT_TIMESCALE) + u32(min(duration, 0xFFFFFFFF))
               + u32(0x00010000) + u16(0x0100) + b"\x00" * 10 + matrix_identity()
               + b"\x00" * 24 + u32(next_track_id))
    return full_box("mvhd", 0, 0, payload)


def _tkhd(plan: TrackPlan, duration: int) -> bytes:
    track = plan.source_track
    width = getattr(track, "pixel_width", 0)
    height = getattr(track, "pixel_height", 0)
    flags = 1 | (1 if getattr(track, "default", True) else 0)
    payload = (u32(0) + u32(0) + u32(plan.track_id) + u32(0)
               + u32(min(duration, 0xFFFFFFFF)) + b"\x00" * 8 + i16(0) + i16(0)
               + i16(0x0100 if plan.handler == "soun" else 0) + u16(0) + matrix_identity()
               + u32(width << 16) + u32(height << 16))
    return full_box("tkhd", 0, flags, payload)


def _mdhd(duration: int, language: str) -> bytes:
    payload = (u32(0) + u32(0) + u32(OUTPUT_TIMESCALE)
               + u32(min(duration, 0xFFFFFFFF)) + u16(language_code(language)) + u16(0))
    return full_box("mdhd", 0, 0, payload)


def _hdlr(handler: str, name: str) -> bytes:
    return full_box("hdlr", 0, 0, u32(0) + handler.encode("ascii") + b"\x00" * 12
                    + name.encode("utf-8", "replace") + b"\x00")


def _dinf() -> bytes:
    return box("dinf", full_box("dref", 0, 0, u32(1) + full_box("url ", 0, 1)))


def _edts(plan: TrackPlan) -> bytes | None:
    if plan.edit_media_time <= 0:
        return None
    entry = u32(min(plan.edit_media_time, 0xFFFFFFFF)) + u32(
        min(plan.duration_units, 0xFFFFFFFF)) + u32(0x00010000)
    return box("edts", full_box("elst", 0, 0, u32(1) + entry))


def _track_udta(plan: TrackPlan) -> bytes:
    track = plan.source_track
    parts = [box("name", str(getattr(track, "name", "")).encode("utf-8")),
             box("lang", str(getattr(track, "language", "und")).encode("ascii")),
             box("forc", u8(1 if getattr(track, "forced", False) else 0))]
    return box("udta", b"".join(parts))


def _chpl(chapters: list[object]) -> bytes:
    payload = bytearray(u32(0) + u8(min(len(chapters), 255)))
    for chapter in chapters[:255]:
        title = str(getattr(chapter, "title", ""))
        title_bytes = title.encode("utf-8")[:255]
        start = max(0, int(getattr(chapter, "start_ns", 0) / 100))
        payload += u64(start) + u8(len(title_bytes)) + title_bytes
    return full_box("chpl", 0, 0, bytes(payload))


def _entry_for(plan: TrackPlan) -> bytes:
    track = plan.source_track
    private = bytes(getattr(track, "codec_private", b""))
    if plan.entry_type == "avc1":
        return visual_sample_entry(int(getattr(track, "pixel_width", 0)),
                                   int(getattr(track, "pixel_height", 0)), private)
    if plan.entry_type == "mp4a":
        rate = int(round(float(getattr(track, "sampling_frequency", 0) or 48000)))
        return audio_sample_entry(rate, int(getattr(track, "channels", 0) or 2), private)
    return text_sample_entry()


def _stbl(plan: TrackPlan, cluster_chunks: dict[int, int]) -> bytes:
    return build_stbl(plan.sample_meta, _entry_for(plan), cluster_chunks)


def _minf(plan: TrackPlan, cluster_chunks: dict[int, int]) -> bytes:
    if plan.handler == "vide":
        media_header = full_box("vmhd", 0, 1, u16(0) + u16(0) + u16(0) + u16(0))
    elif plan.handler == "soun":
        media_header = full_box("smhd", 0, 0, i16(0) + u16(0))
    else:
        media_header = full_box("nmhd", 0, 0)
    parts = media_header + _dinf() + _stbl(plan, cluster_chunks)
    return box("minf", parts)


def _mdia(plan: TrackPlan, cluster_chunks: dict[int, int]) -> bytes:
    track = plan.source_track
    parts = (_mdhd(plan.duration_units, str(getattr(track, "language", "und")))
             + _hdlr(plan.handler, str(getattr(track, "name", "")))
             + _minf(plan, cluster_chunks))
    return box("mdia", parts)


def _trak(plan: TrackPlan, cluster_chunks: dict[int, int]) -> bytes:
    parts = _tkhd(plan, plan.duration_units)
    edit = _edts(plan)
    if edit is not None:
        parts += edit
    parts += _mdia(plan, cluster_chunks) + _track_udta(plan)
    return box("trak", parts)


def build_moov(plans: dict[int, TrackPlan], chapters: list[object],
               cluster_chunks: dict[int, int]) -> bytes:
    ordered = sorted(plans.values(), key=lambda plan: plan.track_id)
    duration = max((plan.duration_units for plan in ordered), default=0)
    parts = [_mvhd(duration, max(plan.track_id for plan in ordered) + 1)]
    parts.extend(_trak(plan, cluster_chunks) for plan in ordered)
    if chapters:
        parts.append(box("udta", _chpl(chapters)))
    return box("moov", b"".join(parts))


def _assign_offsets(plans: dict[int, TrackPlan], cluster_count: int,
                    mdat_data_start: int) -> tuple[int, list[tuple[TrackPlan, RawSample, SampleMeta]]]:
    order: list[tuple[TrackPlan, RawSample, SampleMeta]] = []
    payload_size = 0
    for cluster_index in range(cluster_count):
        for track_number in sorted(plans):
            plan = plans[track_number]
            pairs = [(sample, meta) for sample, meta in zip(plan.ordered_samples, plan.sample_meta)
                     if sample.cluster_index == cluster_index]
            for sample, meta in pairs:
                meta.offset = mdat_data_start + payload_size
                payload_size += sample.size
                order.append((plan, sample, meta))
    return payload_size, order


def _manifest(media: ParsedMedia, plans: dict[int, TrackPlan], repairs: RepairResult,
              mdat_bytes: int, output_path: str, source_size: int) -> dict[str, object]:
    tracks = []
    for plan in sorted(plans.values(), key=lambda item: item.track_id):
        track = plan.source_track
        tracks.append({
            "track": plan.track_id,
            "track_ID": plan.track_id,
            "codec": plan.entry_type,
            "handler": plan.handler,
            "name": str(getattr(track, "name", "")),
            "language": str(getattr(track, "language", "und")),
            "default": bool(getattr(track, "default", True)),
            "forced": bool(getattr(track, "forced", False)),
            "samples": len(plan.sample_meta),
            "sample_count": len(plan.sample_meta),
            "first_pts": round(plan.first_pts_units / OUTPUT_TIMESCALE, 6),
            "last_pts": round(plan.last_pts_units / OUTPUT_TIMESCALE, 6),
            "duration": round(plan.duration_units / OUTPUT_TIMESCALE, 6),
            "keyframes": sum(1 for meta in plan.sample_meta if meta.keyframe),
        })
    return {
        "ok": True,
        "version": 1,
        "output": os.path.basename(output_path),
        "input_bytes": source_size,
        "tracks": tracks,
        "repairs": repairs.as_dict(),
        "cues_rebuilt": len(media.clusters),
        "mdat_bytes": mdat_bytes,
        "moov_before_mdat": True,
        "digest": media.digest,
        "elements": media.stats.elements,
        "unknown_size": media.stats.unknown_size,
        "clusters": len(media.clusters),
        "chapters": [
            {"uid": chapter.uid, "start": round(chapter.start_ns / 1e9, 6),
             "end": round(chapter.end_ns / 1e9, 6) if chapter.end_ns is not None else None,
             "title": chapter.title, "language": chapter.language}
            for chapter in media.chapters
        ],
    }


def _cluster_chunks(plans: dict[int, TrackPlan]) -> dict[int, int]:
    present = sorted({meta.cluster_index for plan in plans.values() for meta in plan.sample_meta})
    return {cluster: ordinal for ordinal, cluster in enumerate(present)}


def remux_file(input_path: str, output_path: str, *, manifest_path: str | None = None,
               repair: str = "clamp", max_repair_ratio: float = 0.05) -> dict[str, object]:
    Path(output_path).parent.mkdir(parents=True, exist_ok=True)
    if manifest_path:
        Path(manifest_path).parent.mkdir(parents=True, exist_ok=True)
    with EBMLReader(input_path) as reader:
        source_size = reader.size
        media = parse_matroska(reader)
        plans = map_tracks(media)
        with tempfile.TemporaryFile() as temp:
            extractor = _SampleExtractor(reader, media, temp, plans)
            extractor.extract()
            media.blocks.clear()
            extractor.resolve_timing()
            repairs = rebuild_timeline(plans, repair, max_repair_ratio)
            _layout_samples(plans, len(media.clusters))
            cluster_chunks = _cluster_chunks(plans)
            ftyp = _ftyp()
            moov = build_moov(plans, media.chapters, cluster_chunks)
            payload_size = sum(sample.size for plan in plans.values() for sample in plan.samples)
            header_size = 8 if payload_size + 8 <= 0xFFFFFFFF else 16
            mdat_data_start = len(ftyp) + len(moov) + header_size
            _, order = _assign_offsets(plans, len(media.clusters), mdat_data_start)
            actual_moov = build_moov(plans, media.chapters, cluster_chunks)
            if len(actual_moov) != len(moov):
                mdat_data_start = len(ftyp) + len(actual_moov) + header_size
                _, order = _assign_offsets(plans, len(media.clusters), mdat_data_start)
                actual_moov = build_moov(plans, media.chapters, cluster_chunks)
            with open(output_path, "wb") as dst:
                dst.write(ftyp)
                dst.write(actual_moov)
                header_size = write_mdat_header(dst, payload_size)
                assert dst.tell() == mdat_data_start
                for _, sample, _ in order:
                    temp.seek(sample.temp_offset)
                    dst.write(temp.read(sample.size))
    manifest = _manifest(media, plans, repairs, payload_size, output_path, source_size)
    if manifest_path:
        Path(manifest_path).write_text(json.dumps(manifest, ensure_ascii=False, indent=2),
                                       encoding="utf-8")
    return manifest


