import os
import subprocess
import sys

import pytest

import engine
from object_store import MODE_FILE, Commit, ObjectStore
from workspace import Workspace

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def run_demo(*args):
    return subprocess.run(
        [sys.executable, "-m", "engine", "demo", *args],
        cwd=ROOT, capture_output=True, text=True,
    )


def test_demo_passes_byte_exact_verification():
    r = run_demo("--verify-byte-exact")
    assert r.returncode == 0, r.stdout + r.stderr
    assert "RESULT: PASS" in r.stdout
    assert "MISMATCH" not in r.stdout


def test_demo_reports_dedup_and_conflicts():
    r = run_demo("--verify-byte-exact")
    assert "still dedup" in r.stdout
    assert "merge:" in r.stdout
    assert ".conflict" in r.stdout


def test_demo_requires_no_arguments_to_run():
    r = run_demo()
    assert r.returncode == 0
    assert "RESULT:" in r.stdout


@pytest.fixture
def repo(tmp_path):
    r = engine.Repository(str(tmp_path))
    r.init()
    return r


def test_commit_and_checkout(repo):
    open(os.path.join(repo.path, "f.txt"), "wb").write(b"data\n")
    oid = repo.commit("first")
    assert repo.head() == oid
    os.unlink(os.path.join(repo.path, "f.txt"))
    repo.checkout(oid)
    assert open(os.path.join(repo.path, "f.txt"), "rb").read() == b"data\n"


def test_commit_chain_records_parents(repo):
    open(os.path.join(repo.path, "a"), "wb").write(b"1")
    c1 = repo.commit("one")
    open(os.path.join(repo.path, "b"), "wb").write(b"2")
    c2 = repo.commit("two")
    assert repo.store.read_commit(c2).parents == [c1]


def test_verify_reports_clean_tree(repo):
    open(os.path.join(repo.path, "a"), "wb").write(b"1")
    repo.commit("c")
    assert repo.verify() == []
    open(os.path.join(repo.path, "a"), "wb").write(b"2")
    assert repo.verify() == ["a"]


def test_gc_packs_loose_objects(repo):
    for i in range(5):
        open(os.path.join(repo.path, f"f{i}"), "wb").write(b"payload %d" % i)
    repo.commit("batch")
    loose_before = len(list(repo.store.iter_oids()))
    stats = repo.gc()
    assert stats["objects"] == loose_before
    assert stats["pack_bytes"] > 0
    assert os.path.exists(stats["pack"])


def test_packed_objects_readable_by_dulwich(repo):
    import hashlib
    import packfile as P
    payload = b"seismic trace " * 500
    open(os.path.join(repo.path, "trace.bin"), "wb").write(payload)
    repo.commit("trace")
    stats = repo.gc()
    from dulwich.pack import Pack
    p = Pack(stats["pack"][:-5])
    p.check()
    found = []
    for o in p.iterobjects():
        raw = o.data if hasattr(o, "data") else o.as_raw_string()
        found.append(raw if isinstance(raw, bytes) else b"".join(raw))
    assert payload in found


def _commit_with(store, files, message):
    """Wrap a file set in a real commit so merge() receives commit oids."""
    entries = [(MODE_FILE, k.encode(), store.write_blob(v)) for k, v in files.items()]
    tree = store.write_tree(entries)
    stamp = "t <t@t> 1 +0000"
    return store.write_commit(Commit(tree, [], stamp, stamp, message))


def test_merge_cli_writes_conflict_files(repo, tmp_path):
    store = repo.store
    base = _commit_with(store, {"a": b"base", "b": b"keep"}, "base")
    ours = _commit_with(store, {"a": b"ours", "b": b"keep"}, "ours")
    theirs = _commit_with(store, {"a": b"theirs", "b": b"keep"}, "theirs")
    out = os.path.join(str(tmp_path), "conf")
    n = repo.merge(base, ours, theirs, conflict_dir=out)
    assert n == 1
    files = os.listdir(out)
    assert files and files[0].endswith(".conflict")
    body = open(os.path.join(out, files[0]), "rb").read()
    assert b"<<<<<<< ours" in body and b"=======" in body and b">>>>>>> theirs" in body
    assert b"ours" in body and b"theirs" in body
