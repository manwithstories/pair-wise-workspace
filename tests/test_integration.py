import json

import pytest

from mp4qc.main import main


@pytest.fixture(scope="module")
def workspace(tmp_path_factory):
    root = tmp_path_factory.mktemp("ws")
    fixtures = root / "fixtures"
    out = root / "out"
    assert main(["fixtures", "--out", str(fixtures), "--seed", "7"]) == 0
    return root, fixtures, out


def test_sample_table_expected_fields(workspace):
    _root, fixtures, out = workspace
    rc = main(["sample-table", str(fixtures / "valid_dash.mp4"),
               "--track", "1", "--json", str(out / "samples.json")])
    assert rc == 0
    data = json.loads((out / "samples.json").read_text())["data"]
    assert data["sample_count"] == 1480
    assert data["stts_entries"] == 312
    assert data["stsc_entries"] == 5


def test_verify_drift_expected_fields(workspace):
    _root, fixtures, out = workspace
    rc = main(["verify", str(fixtures / "drift_sidx.mp4"),
               "--json", str(out / "verify.json")])
    assert rc == 2
    data = json.loads((out / "verify.json").read_text())["data"]
    assert data["sidx_references"] == 37
    assert data["offset_violations"] == 12
    assert data["max_drift_bytes"] == 4864
    assert data["first_violation_index"] == 25


def test_rebuild_then_verify_clean(workspace):
    _root, fixtures, out = workspace
    rc = main(["rebuild", str(fixtures / "drift_sidx.mp4"),
               "--out", str(out / "rebuilt.mp4"),
               "--json", str(out / "rebuild.json")])
    assert rc == 0
    data = json.loads((out / "rebuild.json").read_text())["data"]
    assert data["recheck"]["offset_violations"] == 0
    assert abs(data["length_delta"]) < 1000
    assert data["boundaries"][0]["new_offset"] is not None
    assert main(["verify", str(out / "rebuilt.mp4"), "--strict"]) == 0


def test_truncated_exit_code_3(workspace):
    _root, fixtures, _out = workspace
    assert main(["verify", str(fixtures / "truncated.mp4")]) == 3


def test_multitrack_side_by_side_and_encv_warn(workspace):
    _root, fixtures, out = workspace
    rc = main(["sample-table", str(fixtures / "multitrack.mp4"),
               "--json", str(out / "mt.json")])
    assert rc == 0
    doc = json.loads((out / "mt.json").read_text())
    assert doc["status"] == "warn"  # encv track warns, does not abort
    tracks = doc["data"]["tracks"]
    assert [t["track_id"] for t in tracks] == [1, 2]
    assert tracks[0]["sample_count"] == 600
    assert tracks[1]["sample_count"] == 480


def test_co64_and_stz2_fixtures(workspace):
    _root, fixtures, out = workspace
    assert main(["sample-table", str(fixtures / "co64.mp4"),
                 "--track", "1", "--json", str(out / "co64.json")]) == 0
    co64 = json.loads((out / "co64.json").read_text())["data"]
    assert co64["sample_count"] == 240
    assert co64["chunk_count"] == 8
    assert main(["sample-table", str(fixtures / "stz2.mp4"),
                 "--track", "1", "--json", str(out / "stz2.json")]) == 0
    stz2 = json.loads((out / "stz2.json").read_text())["data"]
    assert stz2["sample_count"] == 100
    assert stz2["has_ctts"] is True


def test_missing_file_exit_code_3():
    assert main(["verify", "no/such/file.mp4"]) == 3

