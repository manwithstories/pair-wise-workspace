"""Checkpoints: atomic writes, fingerprint reuse, conflict detection, recovery."""

from __future__ import annotations

import json

import pytest

from mediacore.checkpoint import (
    Checkpoint, StageCommit, config_digest, find_partial_writes,
    fingerprint_inputs, write_json_atomic,
)
from mediacore.errors import CheckpointConflict


@pytest.fixture()
def inputs(tmp_path):
    a = tmp_path / "a.bin"
    b = tmp_path / "b.bin"
    a.write_bytes(b"alpha" * 1000)
    b.write_bytes(b"beta" * 700)
    return fingerprint_inputs([a, b])


def test_commit_roundtrip_and_stage_lookup(tmp_path, inputs):
    path = tmp_path / "cp.json"
    checkpoint = Checkpoint.new(path, inputs=inputs, config={"codec": "h264"})
    commit = checkpoint.commit(
        "scan", unit_start=0, unit_end=12, byte_start=0, byte_end=999,
        records=12, payload={"units": 12})
    checkpoint.save()
    loaded = Checkpoint.load(path, inputs=inputs, config={"codec": "h264"})
    assert loaded is not None
    assert loaded.is_committed("scan")
    assert loaded.get("scan").records == 12
    assert loaded.get("scan").digest == commit.digest
    assert loaded.input_digest == checkpoint.input_digest


def test_recommit_replaces_stage_and_is_idempotent(tmp_path, inputs):
    checkpoint = Checkpoint.new(tmp_path / "c.json", inputs=inputs, config={})
    checkpoint.commit("scan", unit_start=0, unit_end=5, byte_start=0,
                      byte_end=10, records=5)
    first = checkpoint.get("scan").digest
    checkpoint.commit("scan", unit_start=0, unit_end=5, byte_start=0,
                      byte_end=10, records=5)
    assert checkpoint.stages() == ["scan"]
    assert checkpoint.get("scan").digest == first


def test_config_or_input_change_is_a_named_conflict(tmp_path, inputs):
    path = tmp_path / "cp.json"
    checkpoint = Checkpoint.new(path, inputs=inputs, config={"seed": 41, "gop": 15})
    checkpoint.commit("scan", unit_start=0, unit_end=1, byte_start=0,
                      byte_end=1, records=1)
    checkpoint.save()
    with pytest.raises(CheckpointConflict) as excinfo:
        Checkpoint.load(path, inputs=inputs, config={"seed": 42, "gop": 15})
    assert excinfo.value.reason == "config_changed"
    assert "seed" in excinfo.value.message

    inputs[0].digest = "0" * 40
    with pytest.raises(CheckpointConflict) as excinfo2:
        Checkpoint.load(path, inputs=inputs, config={"seed": 41, "gop": 15})
    assert excinfo2.value.reason == "inputs_changed"


def test_unreadable_and_versioned_checkpoints_are_rejected(tmp_path, inputs):
    path = tmp_path / "cp.json"
    path.write_text("{broken")
    with pytest.raises(CheckpointConflict) as excinfo:
        Checkpoint.load(path, inputs=inputs, config={})
    assert excinfo.value.reason == "unreadable"
    path.write_text(json.dumps({"version": 999}))
    with pytest.raises(CheckpointConflict) as excinfo:
        Checkpoint.load(path, inputs=inputs, config={})
    assert excinfo.value.reason == "version"


def test_partial_commits_are_not_resumed(tmp_path, inputs):
    path = tmp_path / "cp.json"
    checkpoint = Checkpoint.new(path, inputs=inputs, config={})
    checkpoint.commit("scan", unit_start=0, unit_end=3, byte_start=0,
                      byte_end=10, records=3)
    checkpoint.save()
    raw = json.loads(path.read_text())
    # Simulate a crash that left a half-written commit record behind.
    raw["commits"].append({"stage": "derive", "unit_start": 0})
    raw["commits"][0]["records"] = -1
    path.write_text(json.dumps(raw))
    loaded = Checkpoint.load(path, inputs=inputs, config={})
    assert loaded is not None
    assert loaded.stages() == []  # both records are invalid or partial


def test_atomic_write_leaves_no_partial_target(tmp_path):
    target = tmp_path / "out.json"
    write_json_atomic(target, {"a": 1})
    assert json.loads(target.read_text()) == {"a": 1}
    write_json_atomic(target, {"a": 2})
    assert json.loads(target.read_text()) == {"a": 2}
    assert find_partial_writes(tmp_path) == []
    (tmp_path / ".out.json.999.tmp").write_text("{")
    assert [p.name for p in find_partial_writes(tmp_path)] == [".out.json.999.tmp"]


def test_config_digest_is_order_insensitive():
    assert config_digest({"a": 1, "b": 2}) == config_digest({"b": 2, "a": 1})
    assert config_digest({"a": 1}) != config_digest({"a": 2})


def test_stage_commit_rejects_inverted_ranges():
    assert StageCommit.from_dict({
        "stage": "x", "unit_start": 5, "unit_end": 4, "byte_start": 0,
        "byte_end": 1, "records": 1, "digest": "d"}) is None
