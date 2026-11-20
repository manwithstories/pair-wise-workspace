"""Untrusted index validation, rebuild and random-access verification."""

from __future__ import annotations

import pytest

from mediacore.clock import TimeBase
from mediacore.index import (
    IndexEntry, build_index, random_access_index, seek_target, validate_index,
    verify_seeks,
)


def _supplied(tiny_units):
    entries = [u for u in tiny_units if u.keyframe]
    return {
        "keyframes": [
            {"offset": e.offset, "pts": e.index, "size": e.size} for e in entries
        ]
    }


def test_rebuild_matches_stream_keyframes(tiny_units):
    validation = build_index(tiny_units, time_base=TimeBase(1, 30))
    expected = [u.offset for u in tiny_units if u.keyframe]
    assert [e.offset for e in validation.accepted] == expected
    assert all(e.keyframe for e in validation.accepted)


def test_out_of_range_and_unaligned_entries_are_rejected_with_offsets(tiny_units):
    payload = _supplied(tiny_units)
    good = payload["keyframes"][0]
    payload["keyframes"] += [
        {"offset": 10**9, "pts": 0},
        {"offset": good["offset"] + 1, "pts": 0},
        {"offset": -5, "pts": 0},
    ]
    validation = validate_index(payload, tiny_units, time_base=TimeBase(1, 30))
    codes = [i.code for i in validation.rejected]
    assert "index_offset_out_of_range" in codes
    assert "index_offset_not_aligned" in codes
    assert all(i.offset is not None for i in validation.rejected)


def test_wrong_pts_duplicate_and_missing_size_are_handled(tiny_units):
    payload = _supplied(tiny_units)
    payload["keyframes"].append(dict(payload["keyframes"][0]))  # duplicate
    payload["keyframes"][1]["pts"] = 999                       # wrong PTS
    payload["keyframes"][2]["size"] = 1                         # wrong size
    payload["keyframes"][2]["pts"] = None                       # missing PTS
    validation = validate_index(payload, tiny_units, time_base=TimeBase(1, 30))
    codes = [i.code for i in validation.rejected]
    assert "index_pts_mismatch" in codes
    assert "index_duplicate" in codes
    assert any(c["field"] == "size" for c in validation.corrected)
    assert any(c["field"] == "pts" for c in validation.corrected)


def test_non_keyframe_offset_is_rejected(tiny_units):
    non_key = next(u for u in tiny_units if not u.keyframe)
    payload = {"keyframes": [{"offset": non_key.offset, "pts": non_key.index}]}
    validation = validate_index(payload, tiny_units, time_base=TimeBase(1, 30))
    assert validation.accepted == []
    assert validation.rejected[0].code == "index_not_keyframe"


def test_malformed_payload_is_an_input_error(tiny_units):
    from mediacore.errors import MediaInputError

    with pytest.raises(MediaInputError):
        validate_index({"nope": []}, tiny_units, time_base=TimeBase(1, 30))
    with pytest.raises(MediaInputError):
        validate_index("bad", tiny_units, time_base=TimeBase(1, 30))


def test_seek_target_selects_nearest_entry_at_or_before():
    entries = [
        IndexEntry(0, 0, 10, 0, 0.0, True),
        IndexEntry(15, 100, 10, 15, 0.5, True),
        IndexEntry(30, 200, 10, 30, 1.0, True),
    ]
    assert seek_target(entries, 0.4).unit_index == 0
    assert seek_target(entries, 0.5).unit_index == 15
    assert seek_target(entries, 5.0).unit_index == 30


def test_seek_verification_uses_observed_times():
    entries = [IndexEntry(i * 15, i * 100, 10, i * 15, i * 0.5, True)
               for i in range(3)]
    checks = verify_seeks(
        entries, [0.5, 0.5, 0.5], targets_ms=[0.0, 500.0, 1000.0],
        observed_ms=[1.5, 498.0, 1004.0])
    assert [c.error_ms for c in checks] == [1.5, 2.0, 4.0]


def test_random_access_index_reports_max_error_and_empty_case():
    entries = [IndexEntry(i * 15, i * 100, 10, i * 15, i * 0.5, True)
               for i in range(2)]
    result = random_access_index(
        [], time_base=TimeBase(1, 30), supplied=None)
    assert result.entries == []
    assert any(i.code == "index_empty" for i in result.issues)
