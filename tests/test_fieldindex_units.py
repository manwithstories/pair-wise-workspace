"""timecode / subtitles / repair 单元测试：多制式、截断、回绕与冲突。"""
from __future__ import annotations

from fractions import Fraction

import pytest

from fieldindex.model import MAX_INPUT_BYTES, FieldIndexError, SourceRef, TimecodeRecord
from fieldindex.repair import RepairConfig, repair_cues, repair_timecodes
from fieldindex.subtitles import SubtitleParser, parse_subtitle_bytes
from fieldindex.timecode import (
    TimecodeError,
    format_timecode,
    frame_to_time,
    parse_rate,
    parse_timecode,
)
from fieldindex.timecode_list import parse_timecode_bytes

DF_RATE = Fraction(30000, 1001)


# ---- timecode ---------------------------------------------------------

def test_drop_frame_illegal_frame_numbers_rejected():
    for code in ("00:01:00;00", "00:01:00;01", "00:03:00;01", "00:59:00;00"):
        with pytest.raises(TimecodeError):
            parse_timecode(code, DF_RATE)


def test_drop_frame_legal_codes_roundtrip_exhaustive():
    mismatches = 0
    for hh in range(2):
        for mm in range(60):
            for ss in range(60):
                for ff in range(30):
                    if ss == 0 and mm % 10 != 0 and ff < 2:
                        continue
                    code = f"{hh:02d}:{mm:02d}:{ss:02d};{ff:02d}"
                    frame, drop = parse_timecode(code, DF_RATE)
                    if format_timecode(frame, DF_RATE, True) != code or not drop:
                        mismatches += 1
    assert mismatches == 0


def test_drop_frame_identity_over_ten_minute_block():
    bad = [
        n for n in range(17982)
        if parse_timecode(format_timecode(n, DF_RATE, True), DF_RATE)[0] != n
    ]
    assert bad == []


def test_ten_minute_boundary_has_no_dropped_frames():
    frame, _ = parse_timecode("00:10:00;00", DF_RATE)
    assert frame == 17982
    assert frame_to_time(frame, DF_RATE) == Fraction(17982, DF_RATE)


@pytest.mark.parametrize(
    "code,rate",
    [("00:00:01:00", "24"), ("01:02:03:24", "25"),
     ("23:59:59:29", "30"), ("23:59:59:23", "23.976")],
)
def test_non_drop_frame_rates(code, rate):
    fps = parse_rate(rate)
    frame, drop = parse_timecode(code, fps)
    assert format_timecode(frame, fps, drop) == code and drop is False


def test_drop_frame_rejected_on_non_30_family_rate():
    with pytest.raises(TimecodeError):
        parse_timecode("00:01:00;02", Fraction(25))


@pytest.mark.parametrize(
    "code", ["99:00:00:00", "00:60:00:00", "garbage", "00:00:00:30", "1;2;3"]
)
def test_malformed_timecodes_rejected(code):
    with pytest.raises(TimecodeError):
        parse_timecode(code, DF_RATE)


# ---- subtitles --------------------------------------------------------

def test_srt_bom_crlf_and_offsets():
    data = (
        "﻿1\r\n00:00:01,000 --> 00:00:02,500\r\n你好\r\n\r\n"
        "2\r\n00:00:03,000 --> 00:00:04,000\r\nsecond\r\n"
    ).encode()
    cues, errors = parse_subtitle_bytes(data, "a.srt", chunk=5)
    assert len(cues) == 2 and not errors
    assert cues[0].text == "你好" and cues[0].ref.start >= 3
    assert b"-->" in data[cues[1].ref.start:cues[1].ref.end]


def test_webvtt_cue_id_and_note_block():
    vtt = (
        "WEBVTT\r\n\r\nNOTE skip me\r\n\r\n"
        "cue-1\r\n00:00:05.250 --> 00:00:06.750 align:start\r\nhello\r\n"
    ).encode()
    cues, errors = parse_subtitle_bytes(vtt, "b.vtt", chunk=7)
    assert [c.text for c in cues] == ["hello"] and not errors


def test_truncated_tail_recorded_but_later_data_survives():
    data = (
        "1\n00:00:01,000 --> 00:00:02,000\nok\n\n"
        "2\n00:00:03,000 --> 00:00:04,00"
    ).encode()
    cues, errors = parse_subtitle_bytes(data, "c.srt", chunk=4)
    assert [c.text for c in cues] == ["ok"]
    assert len(errors) == 1 and errors[0].action == "truncated-skip"
    assert errors[0].end == len(data)


def test_bad_middle_block_does_not_stop_pipeline():
    data = (
        "1\n00:00:01,000 --> 00:00:02,000\ngood\n\nBROKEN\n\n"
        "3\n00:00:05,000 --> 00:00:06,000\nafter\n"
    ).encode()
    cues, errors = parse_subtitle_bytes(data, "d.srt", chunk=3)
    assert [c.text for c in cues] == ["good", "after"]
    assert len(errors) == 1 and errors[0].action == "quarantine"


def test_incremental_feed_split_invariance():
    sample = (
        "1\n00:00:01,500 --> 00:00:02,500\nA\nB\n\n"
        "2\n00:00:03,000 --> 00:00:04,000\nC\n"
    ).encode()
    ref, _ = parse_subtitle_bytes(sample, "x.srt", chunk=1000)
    expected = [(c.seq, c.text, c.ref.start, c.ref.end) for c in ref]
    for cut in range(1, len(sample)):
        parser = SubtitleParser("x.srt")
        parser.feed(sample[:cut])
        parser.feed(sample[cut:])
        cues, _ = parser.close()
        assert [(c.seq, c.text, c.ref.start, c.ref.end) for c in cues] == expected


def test_input_size_limit_raises_with_offset():
    parser = SubtitleParser("big.srt")
    with pytest.raises(FieldIndexError) as exc:
        parser.feed(b"x" * (MAX_INPUT_BYTES + 1))
    assert exc.value.offset >= 0


def test_path_resolution_confined_to_workdir(tmp_path):
    from fieldindex.model import resolve_within

    inside = resolve_within(tmp_path, "nested/input.srt")
    assert tmp_path in inside.parents or inside.parent == tmp_path
    for escape in ("../outside.srt", "../../etc/passwd", "/etc/passwd"):
        with pytest.raises(FieldIndexError):
            resolve_within(tmp_path, escape)
    link = tmp_path / "link.srt"
    link.symlink_to(tmp_path / "target.srt")
    with pytest.raises(FieldIndexError):
        resolve_within(tmp_path, "link.srt")


# ---- timecode list ----------------------------------------------------

def test_timecode_list_parsing_and_quarantine():
    data = b"# rate=24\n00:00:00:00\n00:00:00:01\nbad-code\n00:00:00:02\n"
    records, errors = parse_timecode_bytes(data, "tc.txt", Fraction(24))
    assert [r.frame for r in records] == [0, 1, 2]
    assert len(errors) == 1 and errors[0].stage == "timecode"
    assert errors[0].start >= 0 and errors[0].end > errors[0].start


def test_timecode_list_rate_directive_supports_df_and_comments():
    data = b"# rate=30000/1001\n00:00:00:00\n00:00:00;02\n"
    records, errors = parse_timecode_bytes(data, "tc.txt", Fraction(24))
    assert not errors
    assert records[0].rate_fps == Fraction(30000, 1001)
    assert records[1].drop_frame is True and records[1].frame == 2
    trailing, _ = parse_timecode_bytes(
        b"# rate=25 trailing note\n00:00:01:00\n", "tc.txt", Fraction(30)
    )
    assert trailing[0].rate_fps == Fraction(25)


# ---- repair -----------------------------------------------------------

def _rec(code: str, i: int, rate=Fraction(24)):
    frame, drop = parse_timecode(code, rate)
    return TimecodeRecord(
        frame=frame, rate_fps=rate, drop_frame=drop, raw=code,
        ref=SourceRef("tc.txt", i * 16, i * 16 + 12),
    )


def test_cross_day_wrap_uses_monotonicity():
    recs = [_rec("23:59:59:23", 0), _rec("00:00:00:00", 1), _rec("00:00:00:01", 2)]
    out, errors = repair_timecodes(recs, RepairConfig(enable_drift=False))
    assert out[0].timeline < out[1].timeline < out[2].timeline
    assert any(e.action == "wrap" and e.recoverable for e in errors)


def test_small_rewind_is_ambiguous_and_kept():
    recs = [_rec("00:00:05:00", 0), _rec("00:00:04:00", 1)]
    out, errors = repair_timecodes(recs, RepairConfig(enable_drift=False))
    assert "ambiguous-rewind" in out[1].flags
    assert errors[0].action == "flag-ambiguous"


def test_drift_under_threshold_is_clamped():
    recs = [_rec(f"00:00:{s:02d}:00", i) for i, s in enumerate(range(0, 12, 2))]
    for i in (3, 4, 5):
        recs[i].frame += 1
    out, errors = repair_timecodes(recs, RepairConfig())
    assert all("drift-conflict" not in r.flags for r in out)
    assert sum(e.action == "drift-clamp" for e in errors) >= 3


def test_drift_over_threshold_marked_not_moved():
    recs = [_rec(f"00:00:{s:02d}:00", i) for i, s in enumerate(range(0, 12, 2))]
    recs[3].frame += 12
    out, errors = repair_timecodes(recs, RepairConfig())
    assert "drift-conflict" in out[3].flags
    assert any(e.action == "conflict-mark" for e in errors)


def test_overlapping_cues_are_marked_never_moved():
    from fieldindex.model import Cue

    cues = [
        Cue(1, Fraction(0), Fraction(2), "a", SourceRef("s", 0, 10)),
        Cue(2, Fraction(1), Fraction(3), "b", SourceRef("s", 11, 21)),
    ]
    out, errors = repair_cues(cues)
    assert all("overlap-conflict" in c.flags for c in out)
    assert out[0].start == 0 and out[1].start == 1  # 时间戳未被搬动
    assert any(e.action == "conflict-mark" for e in errors)
