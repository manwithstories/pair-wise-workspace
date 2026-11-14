"""Matroska track identities mapped to the generated MP4 tracks."""

from __future__ import annotations

from mkvtool.mkv.demux import DemuxResult
from mkvtool.mp4.muxer import MuxResult

MP4_CODECS = {
    "V_MPEG4/ISO/AVC": "avc1",
    "A_AAC": "mp4a",
    "A_AAC/MPEG4/LC": "mp4a",
}


def build_track_map(demux: DemuxResult, mux: MuxResult | None = None) -> dict:
    mp4_by_source = {
        track.source_number: track for track in (mux.tracks if mux else [])
    }
    records: list[dict] = []
    for track in demux.tracks:
        output = mp4_by_source.get(track.number)
        records.append({
            "track_number": track.number,
            "track_uid": track.uid,
            "track_type": track.kind,
            "track_type_number": track.track_type,
            "codec_id": track.codec_id,
            "timecode_scale_ns": demux.timecode_scale_ns,
            "timebase_ns": demux.timecode_scale_ns,
            "default_duration_ns": track.default_duration_ns,
            "language": track.language,
            "default": track.flag_default,
            "flag_default": track.flag_default,
            "supported": track.supported,
            "mp4_track_id": output.mp4_track_id if output else None,
            "mp4_codec": MP4_CODECS.get(track.codec_id) if output else None,
            "video": (
                {"width": track.pixel_width, "height": track.pixel_height}
                if track.kind == "video" else None
            ),
            "audio": (
                {"sample_rate": track.sample_rate, "channels": track.channels}
                if track.kind == "audio" else None
            ),
        })
    return {
        "track_count": len(records),
        "supported_track_count": sum(1 for record in records if record["supported"]),
        "tracks": records,
    }
