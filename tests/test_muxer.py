from types import SimpleNamespace

from mkvtool.mkv.demux import DemuxResult, Sample, Track
from mkvtool.mkv.timeline import build_timeline
from mkvtool.mp4.inspect import inspect_mp4_bytes
from mkvtool.mp4.muxer import mux_timeline


def _timeline():
    video = Track(
        1, 1, 1, "V_MPEG4/ISO/AVC", b"\x01\x42\x00\x1e\xff",
        default_duration_ns=40_000_000, pixel_width=640, pixel_height=360,
    )
    audio = Track(
        2, 2, 2, "A_AAC", b"\x11\x90",
        default_duration_ns=21_000_000, sample_rate=48000, channels=2,
    )
    video_samples = [
        Sample(1, -80 + index * 40, None, index % 2 == 0, b"v" + bytes([index]) * 10,
               0, index, composition_offset_ticks=80 if index < 2 else 0)
        for index in range(10)
    ]
    audio_samples = [
        Sample(2, index * 21, None, True, b"a", 0, index) for index in range(10)
    ]
    demux = DemuxResult(
        parse=SimpleNamespace(issues=[]),
        segment=SimpleNamespace(data_offset=0),
        tracks=[video, audio],
        samples={1: video_samples, 2: audio_samples},
        timecode_scale_ns=1_000_000,
        duration_ns=None,
    )
    return build_timeline(demux)


def test_mux_and_self_inspect_tables():
    muxed = mux_timeline(_timeline())
    info = inspect_mp4_bytes(muxed.data)
    assert info["has_ftyp"] and info["has_mdat"]
    video, audio = info["tracks"]
    assert video["handler"] == "vide"
    assert video["codec"] == "avc1"
    assert video["frame_count"] == 10
    assert video["keyframes"] == 5
    assert video["dts_monotonic"]
    assert video["chunk_offsets_valid"]
    assert audio["handler"] == "soun"
    assert audio["codec"] == "mp4a"
    assert audio["frame_count"] == 10
    assert audio["chunk_offsets_valid"]
