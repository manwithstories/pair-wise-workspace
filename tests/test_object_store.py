import hashlib
import io
import os

import pytest

from object_store import (MODE_EXEC, MODE_FILE, MODE_LINK, MODE_TREE, Commit,
                          DeltaCorruption, ObjectStore, hash_blob)


@pytest.fixture
def store(tmp_path):
    return ObjectStore(tmp_path / "objects")


@pytest.mark.parametrize("payload", [b"", b"x", b"a" * 17, bytes(range(256)),
                                     os.urandom(5000)])
def test_blob_roundtrip(store, payload):
    oid = store.write_blob(payload)
    assert oid == hash_blob(payload)
    assert store.read_blob(oid) == payload
    assert b"".join(store.read_blob_stream(oid)) == payload


def test_stream_write_matches_buffered(store):
    data = os.urandom(100_000) + b"geo" * 500
    a = store.write_blob(data)
    b = store.write_blob_stream(io.BytesIO(data))
    assert a == b
    assert store.read_blob(b) == data


def test_write_is_idempotent(store):
    data = b"same content"
    assert store.write_blob(data) == store.write_blob(data)
    assert len(store) == 1


def test_tree_entries_sorted_and_typed(store):
    blob = store.write_blob(b"payload")
    t = store.write_tree([
        (MODE_FILE, b"z.txt", blob),
        (MODE_EXEC, b"a.sh", blob),
        (MODE_LINK, b"m", blob),
    ])
    entries = store.read_tree(t)
    assert [e.name for e in entries] == [b"a.sh", b"m", b"z.txt"]
    assert entries[0].mode == MODE_EXEC
    assert entries[1].mode == MODE_LINK


def test_nested_tree(store):
    leaf = store.write_blob(b"leaf")
    sub = store.write_tree([(MODE_FILE, b"f.txt", leaf)])
    top = store.write_tree([(MODE_TREE, b"sub", sub), (MODE_FILE, b"top", leaf)])
    names = {e.name for e in store.read_tree(top)}
    assert names == {b"sub", b"top"}


def test_commit_roundtrip(store):
    blob = store.write_blob(b"x")
    tree = store.write_tree([(MODE_FILE, b"f", blob)])
    c = Commit(tree, ["deadbeef"], "A <a@b>", "A <a@b>", "message here")
    oid = store.write_commit(c)
    back = store.read_commit(oid)
    assert back.tree == tree
    assert back.parents == ["deadbeef"]
    assert back.message == "message here"


def test_corrupt_object_raises_not_truncates(store):
    oid = store.write_blob(b"important payload" * 100)
    path = store.path_for(oid)
    raw = open(path, "rb").read()
    with open(path, "wb") as fh:
        fh.write(raw[: len(raw) // 2])
    with pytest.raises(DeltaCorruption):
        store.read_blob(oid)


def test_missing_object_raises_keyerror(store):
    with pytest.raises(KeyError):
        store.read_blob("0" * 40)


def test_hash_detects_tampering(store):
    oid = store.write_blob(b"content")
    import zlib
    path = store.path_for(oid)
    with open(path, "wb") as fh:
        fh.write(zlib.compress(b"blob 7\0different", 6))
    with pytest.raises(DeltaCorruption):
        store.read_blob(oid)


def test_iter_oids_lists_everything(store):
    a = store.write_blob(b"a")
    b = store.write_tree([(MODE_FILE, b"a", a)])
    assert set(store.iter_oids()) == {a, b}
