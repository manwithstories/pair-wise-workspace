"""End-to-end tests: fixtures are generated first, then the CLI is driven."""

import json

from conftest import run_cli, read_json


def test_inspect_unknown_len(fixtures_dir, work_dir):
    tree_json = work_dir / "tree.json"
    rc = run_cli("inspect", str(fixtures_dir / "unknown_len.mkv"),
                 "--depth", "4", "--json", str(tree_json))
    assert rc == 0
    doc = read_json(tree_json)
    summary = doc["summary"]
    assert 4000 <= summary["elements_total"] <= 4400
    assert summary["unknown_size_elements"] == 2
    assert summary["max_depth"] == 4
    assert summary["clusters"] == 8
    assert summary["blocks"] == 320
    assert summary["truncated_elements"] == 0
    assert doc["elements"]
    assert all(el["depth"] <= 4 for el in doc["elements"])


def test_remux_unknown_len(fixtures_dir, work_dir):
    mp4 = work_dir / "unknown_len.mp4"
    report_path = work_dir / "report.json"
    rc = run_cli("remux", str(fixtures_dir / "unknown_len.mkv"),
                 "-o", str(mp4), "--report", str(report_path))
    assert rc == 0
    report = read_json(report_path)
    t1, t2 = report["tracks"]
    assert (t1["track"], t1["codec"], t1["timescale"]) == (1, "avc1", 90000)
    assert t1["frames"] == 300
    assert t1["first_pts_ns"] == 0
    assert t1["last_pts_ns"] == 9993333333
    assert (t2["track"], t2["codec"], t2["timescale"]) == (2, "mp4a", 48000)
    assert t2["frames"] == 1560
    assert t2["first_pts_ns"] == 0
    assert report["av_start_delta_ms"] == 0.0
    assert report["co64_used"] is False
    assert report["issues"] == 0


def test_verify_unknown_len(fixtures_dir, work_dir):
    mp4 = work_dir / "unknown_len.mp4"
    rc = run_cli("remux", str(fixtures_dir / "unknown_len.mkv"), "-o", str(mp4))
    assert rc == 0
    from mkvkit.isobmff.verify import verify_mp4
    result = verify_mp4(mp4)
    assert result["ok"] is True
    assert result["dts_monotonic"] is True
    assert result["ctts_negative_count"] == 0
    assert result["elst_media_time"] == 0
    assert result["stco_entries"] == 8
    assert result["mdat_bounds_ok"] is True


def test_bframes_elst(fixtures_dir, work_dir):
    mp4 = work_dir / "bframes.mp4"
    rc = run_cli("remux", str(fixtures_dir / "bframes.mkv"), "-o", str(mp4))
    assert rc == 0
    from mkvkit.isobmff.verify import verify_mp4
    result = verify_mp4(mp4)
    assert result["ok"] is True
    assert result["dts_monotonic"] is True
    assert result["ctts_negative_count"] == 0
    assert result["elst_media_time"] == 2970  # one 33ms frame at 90kHz


def test_subtitles_tx3g(fixtures_dir, work_dir):
    report_path = work_dir / "sub_report.json"
    rc = run_cli("remux", str(fixtures_dir / "subtitles.mkv"),
                 "-o", str(work_dir / "subtitles.mp4"),
                 "--report", str(report_path))
    assert rc == 0
    report = read_json(report_path)
    codecs = [t["codec"] for t in report["tracks"]]
    assert codecs == ["avc1", "mp4a", "tx3g"]
    assert [t["frames"] for t in report["tracks"]] == [30, 100, 10]


def test_tx3g_sample_has_length_prefix(fixtures_dir, work_dir):
    import struct
    from mkvkit.isobmff.verify import _iter_boxes, _trak_tables

    mp4 = work_dir / "subtitles_prefix.mp4"
    assert run_cli("remux", str(fixtures_dir / "subtitles.mkv"),
                   "-o", str(mp4)) == 0
    data = mp4.read_bytes()
    top = list(_iter_boxes(data, 0, len(data)))
    moov = next(b for b in top if b[0] == b"moov")
    traks = [b for b in _iter_boxes(data, moov[3], moov[1] + moov[2])
             if b[0] == b"trak"]
    text = next(t for t in traks if _trak_tables(data, t)["handler"] == b"text")
    tables = _trak_tables(data, text)
    stco = tables["stco"]
    count = struct.unpack_from(">I", data, stco[3] + 4)[0]
    offset = struct.unpack_from(">I", data, stco[3] + 8)[0]
    sample_size = struct.unpack_from(">I", data, tables["stsz"][3] + 12)[0]
    sample = data[offset:offset + sample_size]
    text_length = struct.unpack_from(">H", sample, 0)[0]
    assert count > 0
    assert text_length == len(sample) - 2
    assert sample[2:].startswith(b"subtitle line 0")


def test_bad_codec_exit3(fixtures_dir, work_dir):
    rc = run_cli("remux", str(fixtures_dir / "bad_codec.mkv"),
                 "-o", str(work_dir / "x.mp4"))
    assert rc == 3


def test_missing_codec_private_exit3(fixtures_dir, work_dir):
    rc = run_cli("remux", str(fixtures_dir / "missing_private.mkv"),
                 "-o", str(work_dir / "x.mp4"))
    assert rc == 3


def test_not_mkv_exit2(fixtures_dir, work_dir):
    assert run_cli("inspect", str(fixtures_dir / "not_mkv.bin")) == 2
    assert run_cli("remux", str(fixtures_dir / "not_mkv.bin"),
                   "-o", str(work_dir / "x.mp4")) == 2


def test_truncated_header_exit2(fixtures_dir, work_dir):
    assert run_cli("inspect", str(fixtures_dir / "truncated_header.mkv")) == 2
    assert run_cli("remux", str(fixtures_dir / "truncated_header.mkv"),
                   "-o", str(work_dir / "x.mp4")) == 2


def test_truncated_block_skipped(fixtures_dir, work_dir):
    report_path = work_dir / "trunc_report.json"
    rc = run_cli("remux", str(fixtures_dir / "truncated_block.mkv"),
                 "-o", str(work_dir / "trunc.mp4"),
                 "--report", str(report_path))
    assert rc == 0
    report = read_json(report_path)
    assert report["issues"] == 1
    assert report["tracks"][0]["frames"] == 30


def test_jitter_timestamps_repaired(fixtures_dir, work_dir):
    report_path = work_dir / "jitter_report.json"
    mp4 = work_dir / "jitter.mp4"
    rc = run_cli("remux", str(fixtures_dir / "jitter.mkv"),
                 "-o", str(mp4), "--report", str(report_path))
    assert rc == 0
    report = read_json(report_path)
    assert report["issues"] == 4
    from mkvkit.isobmff.verify import verify_mp4
    assert verify_mp4(mp4)["dts_monotonic"] is True


def test_ten_ms_scale_and_lacing_fallback(fixtures_dir, work_dir):
    report_path = work_dir / "scale_10ms.json"
    mp4 = work_dir / "scale_10ms.mp4"
    rc = run_cli("remux", str(fixtures_dir / "scale_10ms.mkv"),
                 "-o", str(mp4), "--report", str(report_path))
    assert rc == 0
    report = read_json(report_path)
    video, audio = report["tracks"]
    assert video["frames"] == 30
    assert video["last_pts_ns"] == 290_000_000
    assert audio["frames"] == 130
    assert audio["first_pts_ns"] == 0
    assert report["issues"] == 0
    from mkvkit.isobmff.verify import verify_mp4
    result = verify_mp4(mp4)
    assert result["ok"] is True
    assert result["dts_monotonic"] is True


def test_audio_start_offset_preserved(fixtures_dir, work_dir):
    report_path = work_dir / "offset_report.json"
    mp4 = work_dir / "audio_offset.mp4"
    rc = run_cli("remux", str(fixtures_dir / "audio_offset.mkv"),
                 "-o", str(mp4), "--report", str(report_path))
    assert rc == 0
    report = read_json(report_path)
    assert report["tracks"][0]["first_pts_ns"] == 0
    assert report["tracks"][1]["first_pts_ns"] == 100_000_000
    assert report["av_start_delta_ms"] == 100.0
    from mkvkit.isobmff.verify import verify_mp4
    result = verify_mp4(mp4)
    assert result["ok"] is True
    assert result["av_first_frame_delta_ms"] == 100.0
    assert result["tracks"][1]["empty_edit_ms"] == 100


def test_bad_timestamps_exit4(fixtures_dir, work_dir):
    rc = run_cli("remux", str(fixtures_dir / "bad_timestamps.mkv"),
                 "-o", str(work_dir / "x.mp4"))
    assert rc == 4


def test_verify_exit_codes(fixtures_dir, work_dir):
    mp4 = work_dir / "unknown_len2.mp4"
    assert run_cli("remux", str(fixtures_dir / "unknown_len.mkv"),
                   "-o", str(mp4)) == 0
    assert run_cli("verify", str(mp4), "--json") == 0
    assert run_cli("verify", str(fixtures_dir / "not_mkv.bin")) == 2


def test_self_check_failure_exit5(fixtures_dir, work_dir, monkeypatch):
    monkeypatch.setattr(
        "mkvkit.isobmff.mux.verify_mp4", lambda _path: {"ok": False}
    )
    rc = run_cli("remux", str(fixtures_dir / "unknown_len.mkv"),
                 "-o", str(work_dir / "bad_selfcheck.mp4"))
    assert rc == 5


def test_remux_refuses_to_overwrite_input(fixtures_dir):
    source = fixtures_dir / "unknown_len.mkv"
    before = source.read_bytes()
    rc = run_cli("remux", str(source), "-o", str(source))
    assert rc == 1
    assert source.read_bytes() == before
