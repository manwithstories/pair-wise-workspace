"""Build the remux/verification JSON report from one parsed source."""

from __future__ import annotations

from mkvtool.mkv.demux import DemuxResult
from mkvtool.mkv.timeline import NS_PER_MS, TimelineResult, TrackTimeline
from mkvtool.mp4.inspect import inspect_mp4_bytes
from mkvtool.mp4.muxer import MuxResult
from mkvtool.report.track_map import build_track_map


def _ms(value_ns: int) -> float:
    return round(value_ns / NS_PER_MS, 3)


def _validate_cues(timeline: TimelineResult) -> dict:
    demux = timeline.demux
    cluster_offsets = {cluster.offset for cluster in demux.clusters}
    keyframe_by_track: dict[int, list[int]] = {}
    for track in demux.tracks:
        if track.supported and track.kind == "video":
            keyframe_by_track[track.number] = [
                sample.timestamp_ticks + sample.composition_offset_ticks
                for sample in demux.samples.get(track.number, [])
                if sample.keyframe
            ]
    expected = sum(len(times) for times in keyframe_by_track.values())
    checked = len(demux.cues)
    invalid = 0
    corrected = 0
    matched: set[tuple[int, int]] = set()
    for cue in demux.cues:
        track_times = keyframe_by_track.get(cue.track_number, [])
        cluster_ok = cue.absolute_cluster_offset in cluster_offsets
        time_ok = cue.time_ticks in track_times
        if cluster_ok and time_ok:
            matched.add((cue.track_number, cue.time_ticks))
        else:
            invalid += 1
            if time_ok:
                corrected += 1
    missing = max(0, expected - len(matched))
    corrected_total = corrected + missing
    return {
        "cues_present": demux.cues_element_present,
        "cue_entries": checked,
        "expected_keyframes": expected,
        "invalid_entries": invalid,
        "corrected_entries": corrected_total,
        "missing_entries": missing,
        "cues_valid": demux.cues_element_present and invalid == 0 and missing == 0,
        "index_source": "cues" if demux.cues_element_present else "scan",
        "mp4_index_source": "scan",
        "mp4_index_rebuilt": True,
    }


def _track_record(track_timeline: TrackTimeline) -> dict:
    track = track_timeline.track
    mapping = []
    for sample in track_timeline.samples:
        mapping.append({
            "source_index": sample.source_index,
            "source_dts_ms": _ms(sample.source_dts_ns),
            "dts_ms": _ms(sample.dts_ns),
            "pts_ms": _ms(sample.pts_ns),
            "duration_ms": _ms(sample.duration_ns),
            "keyframe": sample.keyframe,
            "synthetic": sample.synthetic,
        })
    return {
        "track_number": track.number,
        "type": track.kind,
        "codec_id": track.codec_id,
        "frame_count": len(track_timeline.samples),
        "source_first_dts_ms": _ms(track_timeline.source_first_dts_ns),
        "first_dts_ms": _ms(track_timeline.rewritten_first_dts_ns),
        "first_pts_ms": _ms(track_timeline.first_pts_ns),
        "last_pts_ms": _ms(track_timeline.last_pts_ns),
        "keyframes": track_timeline.keyframes,
        "default_duration_ms": _ms(track_timeline.default_duration_ns),
        "dts_adjustments": track_timeline.dts_adjustments,
        "gaps": [
            {
                "start_ms": _ms(gap.start_ns),
                "end_ms": _ms(gap.end_ns),
                "duration_ms": _ms(gap.duration_ns),
                "filled_frames": gap.filled_frames,
            }
            for gap in track_timeline.gaps
        ],
        "rewrite_map": mapping,
    }


def _expected_mp4(timeline: TimelineResult) -> dict:
    records = []
    for track_timeline in timeline.tracks.values():
        first_dts = track_timeline.samples[0].dts_ns
        first_pts = min(sample.pts_ns - first_dts for sample in track_timeline.samples)
        last_pts = max(sample.pts_ns - first_dts for sample in track_timeline.samples)
        records.append({
            "track_number": track_timeline.track.number,
            "handler": "vide" if track_timeline.track.kind == "video" else "soun",
            "first_dts_ms": 0.0,
            "first_pts_ms": _ms(first_pts),
            "last_pts_ms": _ms(last_pts),
        })
    return {"tracks": records}


def build_remux_report(
    timeline: TimelineResult,
    mux: MuxResult,
    *,
    input_path: str,
    output_path: str,
) -> dict:
    demux = timeline.demux
    track_records = [_track_record(item) for item in timeline.tracks.values()]
    video_tracks = [item for item in track_records if item["type"] == "video"]
    audio_tracks = [item for item in track_records if item["type"] == "audio"]
    source_first = min(
        (item["source_first_dts_ms"] for item in track_records), default=0.0
    )
    rewritten_first = min((item["first_dts_ms"] for item in track_records), default=0.0)
    last_pts = max((item["last_pts_ms"] for item in track_records), default=0.0)
    mp4_info = inspect_mp4_bytes(mux.data)
    return {
        "input": input_path,
        "output": output_path,
        "tracks": len(track_records),
        "source_tracks": len(demux.tracks),
        "video_frames": sum(item["frame_count"] for item in video_tracks),
        "audio_frames": sum(item["frame_count"] for item in audio_tracks),
        "first_dts_ms": source_first,
        "rewritten_first_dts_ms": rewritten_first,
        "first_dts_ms_rewritten": rewritten_first,
        "shift_ms": _ms(timeline.shift_ns),
        "last_pts_ms": last_pts,
        "keyframes": sum(item["keyframes"] for item in video_tracks),
        "gap_threshold_ms": _ms(timeline.gap_threshold_ns),
        "filled_gap_frames": timeline.filled_gaps,
        "index": _validate_cues(timeline),
        "track_map": build_track_map(demux, mux),
        "expected_timestamps": _expected_mp4(timeline),
        "tracks_detail": track_records,
        "mp4": mp4_info,
        "source": {
            "truncated": demux.parse.truncated,
            "truncation_offset": demux.parse.truncation_offset,
            "parse_issues": demux.parse.issues,
            "demux_issues": demux.issues,
            "skipped_tracks": [
                {
                    "track_number": track.number,
                    "codec_id": track.codec_id,
                    "type": track.kind,
                    "reason": "codec not supported by local MP4 writer",
                }
                for track in demux.tracks if not track.supported
            ],
        },
    }


def verification_result(mp4_info: dict, report: dict) -> dict:
    expected_video = report.get("video_frames", 0)
    expected_audio = report.get("audio_frames", 0)
    actual_video = sum(
        track["frame_count"] for track in mp4_info["tracks"] if track["handler"] == "vide"
    )
    actual_audio = sum(
        track["frame_count"] for track in mp4_info["tracks"] if track["handler"] == "soun"
    )
    expected_keyframes = report.get("keyframes", 0)
    actual_keyframes = sum(
        track["keyframes"] for track in mp4_info["tracks"] if track["handler"] == "vide"
    )
    frame_count_match = actual_video == expected_video and actual_audio == expected_audio
    dts_monotonic = all(track["dts_monotonic"] for track in mp4_info["tracks"])
    offsets_valid = all(track["chunk_offsets_valid"] for track in mp4_info["tracks"])
    index_matches = actual_keyframes == expected_keyframes and offsets_valid
    expected_mp4 = report.get("expected_timestamps", {}).get("tracks", [])
    timestamps_match = len(expected_mp4) == len(mp4_info["tracks"])
    if timestamps_match:
        for expected, actual in zip(expected_mp4, mp4_info["tracks"]):
            timestamps_match = timestamps_match and all([
                expected.get("handler") == actual.get("handler"),
                abs((expected.get("first_pts_ms") or 0) - (actual.get("first_pts_ms") or 0)) < 0.001,
                abs((expected.get("last_pts_ms") or 0) - (actual.get("last_pts_ms") or 0)) < 0.001,
            ])
    return {
        "frame_count_match": frame_count_match,
        "dts_monotonic": dts_monotonic,
        "index_matches": index_matches,
        "timestamps_match": timestamps_match,
        "expected": {
            "video_frames": expected_video,
            "audio_frames": expected_audio,
            "keyframes": expected_keyframes,
        },
        "actual": {
            "video_frames": actual_video,
            "audio_frames": actual_audio,
            "keyframes": actual_keyframes,
            "chunk_offsets_valid": offsets_valid,
        },
        "tracks": mp4_info["tracks"],
    }
