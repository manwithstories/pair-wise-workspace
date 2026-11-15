"""Shared report builders; probe and remux consume the same document."""


def element_summary(doc):
    stats = doc.stats
    return {
        "element_count": stats["element_count"],
        "max_depth": stats["max_depth"],
        "unknown_sized": stats["unknown_sized"],
    }


def track_mapping(doc):
    """TrackNumber -> track_ID mapping with preserved metadata."""
    mapping = []
    for track_id, t in enumerate(doc.tracks, 1):
        entry = {
            "track_ID": track_id,
            "track_number": t.number,
            "type": t.type_name,
            "codec": t.codec_id,
            "language": t.language,
            "name": t.name,
            "default": t.flag_default,
        }
        if t.type == 1:
            entry["width"] = t.width
            entry["height"] = t.height
        elif t.type == 2:
            entry["sample_rate"] = t.sample_rate
            entry["channels"] = t.channels
        mapping.append(entry)
    return mapping


def cues_summary(doc):
    return {
        "present": doc.cues_present,
        "status": doc.cues_status,
        "cue_points": len(doc.cue_positions),
    }


def probe_report(doc):
    report = {
        "file": doc.path,
        "size_bytes": doc.file_size,
        "timecode_scale_ns": doc.timecode_scale,
        "duration_ms": doc.duration_ms,
        "tracks": track_mapping(doc),
        "cues": cues_summary(doc),
    }
    report.update(element_summary(doc))
    return report


def _source_pts_ms(doc, track_number):
    pts = [s.pts_ns for s in doc.samples if s.track_number == track_number]
    if not pts:
        return None, None
    base = min(pts)
    return round(base / 1e6, 3), round(max(pts) / 1e6, 3)


def remux_report(doc, result):
    tracks = []
    for plan in result.plans:
        src_first, src_last = _source_pts_ms(doc, plan.track.number)
        entry = {
            "track_ID": plan.track_id,
            "track_number": plan.track.number,
            "type": plan.track.type_name,
            "codec": plan.track.codec_id,
            "language": plan.track.language,
            "name": plan.track.name,
            "default": plan.track.flag_default,
            "timescale": result.timescale,
            "sample_count": plan.timing.sample_count,
            "first_pts_ms": round(plan.timing.first_pts_ms, 3),
            "last_pts_ms": round(plan.timing.last_pts_ms, 3),
            "source_first_pts_ms": src_first,
            "source_last_pts_ms": src_last,
            "has_b_frames": plan.timing.has_b_frames,
            "edit_list": plan.timing.edit_media_time > 0,
        }
        if plan.track.type == 1:
            entry["width"] = plan.track.width
            entry["height"] = plan.track.height
        elif plan.track.type == 2:
            entry["sample_rate"] = plan.track.sample_rate
            entry["channels"] = plan.track.channels
        tracks.append(entry)
    report = {
        "input": doc.path,
        "output": result.output_path,
        "layout": result.layout,
        "timescale": result.timescale,
        "tracks": tracks,
        "cues": dict(cues_summary(doc), rebuilt=True),
        "sample_table_sha256": result.sample_table_sha256,
        "mdat": {"offset": result.mdat_offset, "size": result.mdat_size},
        "moov": {"offset": result.moov_offset, "size": result.moov_size},
    }
    report.update(element_summary(doc))
    return report
