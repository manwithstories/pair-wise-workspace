"""Consistency rules over parsed container facts."""

from __future__ import annotations

from .model import Finding, ProbeResult

DRIFT_WARNING_MS = 100.0
INTERLEAVE_GAP_BYTES = 1024 * 1024
INTERLEAVE_GAP_MS = 2000.0

RULE_CATALOG = {
    "UNSUPPORTED_CONTAINER": "error",
    "UNSUPPORTED_LAYOUT": "error",
    "UNSUPPORTED_ENCRYPTION": "error",
    "IO_ERROR": "error",
    "FILE_PARSE_ERROR": "error",
    "BOX_BOUNDARY_INVALID": "error",
    "TRACK_PARSE_ERROR": "error",
    "MOOV_MISSING": "error",
    "AUDIO_VIDEO_DRIFT": "warning",
    "STSZ_STTS_COUNT_MISMATCH": "error",
    "CHUNK_OFFSET_OUT_OF_RANGE": "error",
    "CHUNK_OVERLAP": "error",
    "INTERLEAVE_GAP": "warning",
    "MULTIPLE_SPS": "info",
    "EMPTY_EDIT": "warning",
    "ZERO_EDIT_DURATION": "warning",
    "EDIT_DURATION_MISMATCH": "info",
    "NO_SYNC_SAMPLE": "warning",
    "NO_MDAT": "error",
    "CHUNK_TABLE_MISSING": "error",
    "STSC_INVALID": "error",
    "CODEC_UNKNOWN": "warning",
    "DURATION_ZERO": "warning",
    "NEGATIVE_CTTS": "warning",
    "TIMESCALE_ZERO": "error",
    "CODEC_PARAM_MISSING": "error",
    "CODEC_PARSE_ERROR": "warning",
    "SAMPLE_COUNT_HIGH": "info",
    "TAIL_GARBAGE": "warning",
    "TRUNCATED_RIFF": "error",
    "TRUNCATED_CHUNK": "error",
    "RIFF_PADDING_ERROR": "warning",
    "FMT_MISSING": "error",
    "DATA_CHUNK_MISSING": "error",
    "ODD_DATA_CHUNK": "info",
    "AUDIO_PARAM_ZERO": "error",
    "AVIH_MISSING": "error",
    "STREAM_COUNT_MISMATCH": "warning",
}


def _inside_mdat(start: int, end: int, ranges: list[tuple[int, int]]) -> bool:
    return any(start >= low and end <= high for low, high in ranges)


def evaluate_rules(result: ProbeResult) -> list[Finding]:
    found: list[Finding] = list(result.findings)
    if result.tracks and not result.mdat_ranges and result.container in {"mp4", "mov"}:
        found.append(Finding("NO_MDAT", "error", "media data box is missing",
                             "no top-level mdat", offset=0))
    intervals: list[tuple[int, int, int, int]] = []
    for track in result.tracks:
        facts = track.table_facts
        if (result.container in {"mp4", "mov"} and track.sample_count > 0
                and facts.get("stts_sample_count", 0) > 0
                and not facts.get("table_truncated")
                and facts["stts_sample_count"] != track.sample_count):
            found.append(Finding(
                "STSZ_STTS_COUNT_MISMATCH", "error", "sample counts in stsz and stts differ",
                f"stsz={track.sample_count} stts={facts['stts_sample_count']}",
                track.index, facts.get("stts_offset", 0),
            ))
        if (result.container in {"mp4", "mov"} and track.sample_count > 0
                and (facts.get("chunk_offset_count", 0) == 0
                     or facts.get("stsc_entry_count", 0) == 0)):
            found.append(Finding(
                "CHUNK_TABLE_MISSING", "error", "sample count exists without chunk mapping",
                f"sample_count={track.sample_count}", track.index, facts.get("stco_offset", 0),
            ))
        if track.duration == 0 and track.sample_count > 0:
            found.append(Finding("DURATION_ZERO", "warning", "track duration is zero",
                                 f"sample_count={track.sample_count}", track.index,
                                 facts.get("mdhd_offset", 0)))
        if not track.codec or track.codec == "unknown":
            found.append(Finding("CODEC_UNKNOWN", "warning", "codec is not recognized",
                                 f"sample_entry={track.sample_entry or 'none'}", track.index,
                                 facts.get("mdhd_offset", 0)))
        sps = track.codec_details.get("sps") or []
        if len(sps) > 1:
            detail = "; ".join(
                f"#{i}:id={item.get('sps_id')},profile={item.get('profile_idc')},"
                f"level={item.get('level_idc')},{item.get('width')}x{item.get('height')}"
                for i, item in enumerate(sps)
            )
            found.append(Finding("MULTIPLE_SPS", "info", "multiple SPS entries are present",
                                 detail, track.index, 0))
        for edit in track.edits:
            if edit["media_time"] < 0:
                found.append(Finding("EMPTY_EDIT", "warning", "edit list starts with empty edit",
                                     f"media_time={edit['media_time']} "
                                     f"duration={edit['segment_duration']}",
                                     track.index, edit["offset"]))
            if edit["segment_duration"] == 0:
                found.append(Finding("ZERO_EDIT_DURATION", "warning", "edit has zero duration",
                                     f"media_time={edit['media_time']}", track.index,
                                     edit["offset"]))
        if track.edits and result.movie_timescale:
            edit_ms = sum(item["segment_duration"] for item in track.edits) / \
                result.movie_timescale * 1000
            if abs(edit_ms - track.duration_ms) > 1.0:
                found.append(Finding(
                    "EDIT_DURATION_MISMATCH", "info", "edit timeline differs from media duration",
                    f"edit={edit_ms:.3f}ms media={track.duration_ms:.3f}ms", track.index,
                    track.edits[0]["offset"],
                ))
        has_stss = bool(facts.get("has_stss"))
        if has_stss and facts.get("sync_sample_count", 0) == 0 and track.sample_count > 0:
            found.append(Finding("NO_SYNC_SAMPLE", "warning", "track has no sync sample",
                                 "stss table is empty", track.index, facts.get("stco_offset", 0)))
        if result.mdat_ranges:
            for start, end, chunk_number in track.chunk_ranges:
                if not _inside_mdat(start, end, result.mdat_ranges):
                    found.append(Finding(
                        "CHUNK_OFFSET_OUT_OF_RANGE", "error", "chunk bytes fall outside mdat",
                        f"chunk={chunk_number} range={start}..{end}", track.index,
                        start,
                    ))
                intervals.append((start, end, track.index, chunk_number))
    if not result.mdat_ranges:
        offsets = [track.chunk_ranges for track in result.tracks if track.chunk_ranges]
        if offsets and result.container in {"mp4", "mov"}:
            pass
    intervals.sort()
    for previous, current in zip(intervals, intervals[1:]):
        if current[0] < previous[1]:
            found.append(Finding(
                "CHUNK_OVERLAP", "error", "chunk byte ranges overlap",
                f"track {previous[2]} chunk {previous[3]} and track {current[2]} "
                f"chunk {current[3]} overlap", current[2], current[0],
            ))
    for track in result.tracks:
        ranges = sorted(track.chunk_ranges)
        for previous, current in zip(ranges, ranges[1:]):
            gap = current[0] - previous[1]
            if gap > INTERLEAVE_GAP_BYTES:
                found.append(Finding(
                    "INTERLEAVE_GAP", "warning", "media chunks are separated beyond threshold",
                    f"gap={gap} bytes between chunk {previous[2]} and {current[2]}",
                    track.index, current[0],
                ))
        timing: dict[int, tuple[int, int]] = {}
        for sample in track.samples:
            first, last = timing.get(sample.chunk, (sample.dts, sample.dts + sample.duration))
            timing[sample.chunk] = (min(first, sample.dts),
                                    max(last, sample.dts + sample.duration))
        by_chunk = sorted(track.chunk_ranges, key=lambda item: item[2])
        for previous, current in zip(by_chunk, by_chunk[1:]):
            before = timing.get(previous[2])
            after = timing.get(current[2])
            if not before or not after or not track.timescale:
                continue
            time_gap_ms = (after[0] - before[1]) / track.timescale * 1000
            if time_gap_ms > INTERLEAVE_GAP_MS:
                found.append(Finding(
                    "INTERLEAVE_GAP", "warning", "media chunk timeline gap exceeds threshold",
                    f"gap={time_gap_ms:.3f}ms between chunk {previous[2]} and {current[2]}",
                    track.index, current[0],
                ))
    video = result.track_duration("video")
    audio = result.track_duration("audio")
    if video is not None and audio is not None:
        drift = audio - video
        if abs(drift) >= DRIFT_WARNING_MS:
            audio_track = next((track for track in result.tracks if track.kind == "audio"), None)
            audio_index = audio_track.index if audio_track else None
            audio_offset = audio_track.table_facts.get("mdhd_offset", 0) if audio_track else 0
            found.append(Finding(
                "AUDIO_VIDEO_DRIFT", "warning", "audio/video presentation duration drift",
                f"audio={audio:.3f}ms video={video:.3f}ms drift={drift:.3f}ms",
                audio_index, audio_offset,
            ))
    unique: list[Finding] = []
    seen: set[tuple] = set()
    for finding in found:
        key = (finding.rule_id, finding.severity, finding.track_index,
               finding.offset, finding.evidence, finding.message)
        if key not in seen:
            seen.add(key)
            unique.append(finding)
    result.findings = unique
    return unique
