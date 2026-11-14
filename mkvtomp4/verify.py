"""Compare MP4 readback summaries with normalized Matroska source data."""

from __future__ import annotations

import struct

from mkvtomp4.mp4.boxes import BoxError
from mkvtomp4.mp4.readback import ReadbackError, read_output_tracks
from mkvtomp4.timing.normalize import NormalizedMedia

VerifyError = ReadbackError


def verify_output(path: str, source: NormalizedMedia) -> dict[str, object]:
    try:
        output_tracks, fragmented, fragment_count = read_output_tracks(path)
    except (OSError, BoxError, ReadbackError, IndexError, struct.error) as exc:
        raise VerifyError(f"cannot parse output MP4: {exc}") from exc
    details: list[dict[str, object]] = []
    sample_delta = timestamp_delta = keyframe_delta = 0
    ok = True
    for expected in source.tracks:
        actual = output_tracks.get(expected.track.mp4_track_id)
        if actual is None:
            ok = False
            details.append({"mp4_track_id": expected.track.mp4_track_id, "missing": True})
            continue
        deltas = {
            "samples": abs(actual.sample_count - expected.sample_count),
            "first_pts": abs(actual.first_pts - expected.first_pts),
            "last_pts": abs(actual.last_pts - expected.last_pts),
            "first_dts": abs(actual.first_dts - expected.first_dts),
            "last_dts": abs(actual.last_dts - expected.last_dts),
            "keyframes": abs(actual.keyframe_count - expected.keyframe_count),
        }
        sample_delta = max(sample_delta, deltas["samples"])
        timestamp_delta = max(
            timestamp_delta,
            deltas["first_pts"],
            deltas["last_pts"],
            deltas["first_dts"],
            deltas["last_dts"],
        )
        keyframe_delta = max(keyframe_delta, deltas["keyframes"])
        track_ok = all(value <= 1 for value in deltas.values())
        if actual.timescale != expected.track.timescale:
            track_ok = False
        ok = ok and track_ok
        details.append(
            {
                "mp4_track_id": expected.track.mp4_track_id,
                "mkv_track": expected.track.mkv_track,
                "timescale": actual.timescale,
                "source_samples": expected.sample_count,
                "output_samples": actual.sample_count,
                "source_first_pts": expected.first_pts,
                "output_first_pts": actual.first_pts,
                "source_last_pts": expected.last_pts,
                "output_last_pts": actual.last_pts,
                "source_keyframes": expected.keyframe_count,
                "output_keyframes": actual.keyframe_count,
                "ok": track_ok,
            }
        )
    return {
        "ok": ok,
        "plain_ok": ok,
        "layout": "fragmented" if fragmented else "plain",
        "sample_delta": sample_delta,
        "timestamp_delta": timestamp_delta,
        "keyframe_delta": keyframe_delta,
        "fragment_count": fragment_count,
        "tracks": details,
    }
