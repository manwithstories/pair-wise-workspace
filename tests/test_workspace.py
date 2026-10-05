import os
import stat

import pytest

from object_store import MODE_EXEC, MODE_FILE, MODE_LINK, ObjectStore
from workspace import Workspace


@pytest.fixture
def repo(tmp_path):
    store = ObjectStore(tmp_path / "objects")
    ws = Workspace(str(tmp_path / "work"), store)
    os.makedirs(ws.root)
    return ws, store


def build_tree(ws, files):
    index = ws.scan()
    return ws.write_tree_from_index(index), index


def test_scan_and_checkout_roundtrip(repo):
    ws, store = repo
    with open(os.path.join(ws.root, "a.txt"), "wb") as fh:
        fh.write(b"hello\n")
    tree, _ = build_tree(ws, None)
    fresh = Workspace(os.path.dirname(ws.root) + "/fresh", store)
    os.makedirs(fresh.root)
    fresh.checkout(tree)
    assert open(os.path.join(fresh.root, "a.txt"), "rb").read() == b"hello\n"


def test_empty_file_preserved(repo):
    ws, store = repo
    open(os.path.join(ws.root, "empty"), "wb").close()
    tree, _ = build_tree(ws, None)
    fresh = Workspace(os.path.dirname(ws.root) + "/fresh", store)
    os.makedirs(fresh.root)
    fresh.checkout(tree)
    p = os.path.join(fresh.root, "empty")
    assert os.path.isfile(p) and os.path.getsize(p) == 0


def test_trailing_newline_and_absence_preserved(repo):
    ws, store = repo
    open(os.path.join(ws.root, "with_nl"), "wb").write(b"line\n")
    open(os.path.join(ws.root, "without_nl"), "wb").write(b"line")
    tree, _ = build_tree(ws, None)
    fresh = Workspace(os.path.dirname(ws.root) + "/fresh", store)
    os.makedirs(fresh.root)
    fresh.checkout(tree)
    assert open(os.path.join(fresh.root, "with_nl"), "rb").read() == b"line\n"
    assert open(os.path.join(fresh.root, "without_nl"), "rb").read() == b"line"


def test_executable_bit_preserved(repo):
    ws, store = repo
    p = os.path.join(ws.root, "run.sh")
    open(p, "wb").write(b"#!/bin/sh\n")
    os.chmod(p, 0o755)
    tree, _ = build_tree(ws, None)
    fresh = Workspace(os.path.dirname(ws.root) + "/fresh", store)
    os.makedirs(fresh.root)
    fresh.checkout(tree)
    out = os.path.join(fresh.root, "run.sh")
    assert os.stat(out).st_mode & stat.S_IXUSR


def test_symlink_preserved(repo):
    ws, store = repo
    open(os.path.join(ws.root, "target"), "wb").write(b"t")
    os.symlink("target", os.path.join(ws.root, "link"))
    tree, _ = build_tree(ws, None)
    fresh = Workspace(os.path.dirname(ws.root) + "/fresh", store)
    os.makedirs(fresh.root)
    fresh.checkout(tree)
    link = os.path.join(fresh.root, "link")
    assert os.path.islink(link) and os.readlink(link) == "target"


def test_nested_directories(repo):
    ws, store = repo
    os.makedirs(os.path.join(ws.root, "a/b/c"))
    open(os.path.join(ws.root, "a/b/c/deep.bin"), "wb").write(bytes(range(256)))
    tree, _ = build_tree(ws, None)
    fresh = Workspace(os.path.dirname(ws.root) + "/fresh", store)
    os.makedirs(fresh.root)
    fresh.checkout(tree)
    assert open(os.path.join(fresh.root, "a/b/c/deep.bin"), "rb").read() == bytes(range(256))


def test_binary_content_is_exact(repo):
    ws, store = repo
    payload = bytes(range(256)) * 40
    open(os.path.join(ws.root, "bin"), "wb").write(payload)
    tree, _ = build_tree(ws, None)
    fresh = Workspace(os.path.dirname(ws.root) + "/fresh", store)
    os.makedirs(fresh.root)
    fresh.checkout(tree)
    assert open(os.path.join(fresh.root, "bin"), "rb").read() == payload


def test_verify_detects_content_change(repo):
    ws, store = repo
    open(os.path.join(ws.root, "f"), "wb").write(b"original")
    tree, _ = build_tree(ws, None)
    assert ws.verify(tree) == []
    open(os.path.join(ws.root, "f"), "wb").write(b"tampered")
    assert ws.verify(tree) == ["f"]


def test_verify_detects_missing_file(repo):
    ws, store = repo
    open(os.path.join(ws.root, "f"), "wb").write(b"x")
    tree, _ = build_tree(ws, None)
    os.unlink(os.path.join(ws.root, "f"))
    assert ws.verify(tree) == ["f"]


def test_verify_detects_mode_change(repo):
    ws, store = repo
    p = os.path.join(ws.root, "run.sh")
    open(p, "wb").write(b"x")
    os.chmod(p, 0o755)
    tree, _ = build_tree(ws, None)
    os.chmod(p, 0o644)
    assert ws.verify(tree) == ["run.sh"]


def test_tree_hash_is_stable_and_sensitive(repo):
    ws, store = repo
    open(os.path.join(ws.root, "a"), "wb").write(b"1")
    open(os.path.join(ws.root, "b"), "wb").write(b"2")
    t1, _ = build_tree(ws, None)
    h1 = ws.tree_hash(t1)
    assert h1 == ws.tree_hash(t1)
    open(os.path.join(ws.root, "b"), "wb").write(b"3")
    t2, _ = build_tree(ws, None)
    assert ws.tree_hash(t2) != h1


def test_checkout_clean_removes_extraneous(repo):
    """A file added after the commit is removed by a clean checkout."""
    ws, store = repo
    open(os.path.join(ws.root, "keep"), "wb").write(b"k")
    tree, _ = build_tree(ws, None)          # commit contains only "keep"
    open(os.path.join(ws.root, "junk"), "wb").write(b"j")   # appears afterwards
    ws.checkout(tree, clean=True)
    assert os.path.exists(os.path.join(ws.root, "keep"))
    assert not os.path.exists(os.path.join(ws.root, "junk"))


def test_many_files_checkout(repo):
    ws, store = repo
    for i in range(500):
        open(os.path.join(ws.root, f"f{i:04d}.dat"), "wb").write(f"content {i}".encode())
    tree, _ = build_tree(ws, None)
    fresh = Workspace(os.path.dirname(ws.root) + "/fresh", store)
    os.makedirs(fresh.root)
    assert fresh.checkout(tree) == 500
    assert fresh.verify(tree) == []
