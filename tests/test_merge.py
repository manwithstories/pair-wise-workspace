import pytest

from merge import (OURS_MARKER, SEPARATOR, THEIRS_MARKER, detect_renames,
                   render_conflict, three_way_merge)
from object_store import MODE_FILE, ObjectStore


@pytest.fixture
def store(tmp_path):
    return ObjectStore(tmp_path / "objects")


def tree_of(store, files):
    entries = []
    for name, data in files.items():
        entries.append((MODE_FILE, name.encode(), store.write_blob(data)))
    return store.write_tree(entries)


def kinds(result):
    return sorted((c.path, c.kind) for c in result.conflicts)


def test_identical_sides_merge_clean(store):
    b = tree_of(store, {"a": b"x"})
    r = three_way_merge(store, b, b, b)
    assert r.clean
    assert set(r.entries) == {"a"}


def test_one_sided_edit_wins(store):
    base = tree_of(store, {"a": b"base"})
    ours = tree_of(store, {"a": b"ours"})
    theirs = tree_of(store, {"a": b"base"})
    r = three_way_merge(store, base, ours, theirs)
    assert r.clean and r.entries["a"][1] == store.write_blob(b"ours")


def test_conflicting_edits_conflict(store):
    base = tree_of(store, {"a": b"base"})
    ours = tree_of(store, {"a": b"ours"})
    theirs = tree_of(store, {"a": b"theirs"})
    r = three_way_merge(store, base, ours, theirs)
    assert kinds(r) == [("a", "content")]


def test_both_sides_same_edit_is_clean(store):
    base = tree_of(store, {"a": b"base"})
    same = tree_of(store, {"a": b"agreed"})
    r = three_way_merge(store, base, same, same)
    assert r.clean and r.entries["a"][1] == store.write_blob(b"agreed")


# ---- delete boundaries
def test_delete_versus_untouched_is_clean(store):
    base = tree_of(store, {"a": b"x", "b": b"y"})
    ours = tree_of(store, {"b": b"y"})
    r = three_way_merge(store, base, ours, base)
    assert r.clean and "a" not in r.entries


def test_modify_delete_conflict(store):
    base = tree_of(store, {"a": b"base"})
    ours = tree_of(store, {"a": b"ours"})
    theirs = tree_of(store, {})
    r = three_way_merge(store, base, ours, theirs)
    assert kinds(r) == [("a", "modify/delete")]


def test_delete_modify_conflict(store):
    base = tree_of(store, {"a": b"base"})
    ours = tree_of(store, {})
    theirs = tree_of(store, {"a": b"theirs"})
    r = three_way_merge(store, base, ours, theirs)
    assert kinds(r) == [("a", "delete/modify")]


def test_delete_on_both_sides_is_clean(store):
    base = tree_of(store, {"a": b"base"})
    empty = tree_of(store, {})
    r = three_way_merge(store, base, empty, empty)
    assert r.clean and r.entries == {}


# ---- add boundaries
def test_add_add_same_content_is_clean(store):
    base = tree_of(store, {})
    same = tree_of(store, {"new": b"same"})
    r = three_way_merge(store, base, same, same)
    assert r.clean and "new" in r.entries


def test_add_add_different_content_conflicts(store):
    base = tree_of(store, {})
    ours = tree_of(store, {"new": b"ours"})
    theirs = tree_of(store, {"new": b"theirs"})
    r = three_way_merge(store, base, ours, theirs)
    assert kinds(r) == [("new", "add/add")]


def test_independent_adds_both_survive(store):
    base = tree_of(store, {})
    ours = tree_of(store, {"a": b"1"})
    theirs = tree_of(store, {"b": b"2"})
    r = three_way_merge(store, base, ours, theirs)
    assert r.clean and set(r.entries) == {"a", "b"}


# ---- rename boundaries
def test_detect_rename_by_content():
    oid = "a" * 40
    base = {"old.txt": (MODE_FILE, oid)}
    side = {"new.txt": (MODE_FILE, oid)}
    assert detect_renames(base, side) == {"old.txt": "new.txt"}


def test_detect_rename_requires_removal():
    oid = "a" * 40
    base = {"old.txt": (MODE_FILE, oid)}
    side = {"old.txt": (MODE_FILE, oid), "new.txt": (MODE_FILE, oid)}
    assert detect_renames(base, side) == {}


def test_clean_rename_merges(store):
    base = tree_of(store, {"old.txt": b"payload"})
    ours = tree_of(store, {"new.txt": b"payload"})
    r = three_way_merge(store, base, ours, base)
    assert r.clean
    assert set(r.entries) == {"new.txt"}
    assert r.renames == {"old.txt": "new.txt"}


def test_rename_on_both_sides_agrees(store):
    base = tree_of(store, {"old.txt": b"payload"})
    ours = tree_of(store, {"new.txt": b"payload"})
    theirs = tree_of(store, {"neu.txt": b"payload"})
    r = three_way_merge(store, base, ours, theirs)
    assert r.renames["old.txt"] in {"new.txt", "neu.txt"}


def test_rename_plus_edit_against_matching_rename(store):
    base = tree_of(store, {"old.txt": b"payload"})
    ours = tree_of(store, {"new.txt": b"edited"})
    theirs = tree_of(store, {"new.txt": b"payload"})
    r = three_way_merge(store, base, ours, theirs)
    assert r.clean
    assert r.entries["new.txt"][1] == store.write_blob(b"edited")


def test_rename_versus_modify_conflicts(store):
    base = tree_of(store, {"old.txt": b"payload"})
    ours = tree_of(store, {"new.txt": b"payload"})
    theirs = tree_of(store, {"old.txt": b"modified"})
    r = three_way_merge(store, base, ours, theirs)
    assert r.conflicts


# ---- conflict rendering
def test_conflict_file_keeps_all_three_versions():
    out = render_conflict("f", b"OURS\n", b"BASE\n", b"THEIRS\n")
    assert OURS_MARKER in out and SEPARATOR in out and THEIRS_MARKER in out
    assert b"OURS\n" in out and b"BASE\n" in out and b"THEIRS\n" in out


def test_conflict_file_without_base():
    out = render_conflict("f", b"OURS\n", None, b"THEIRS\n")
    assert b"OURS\n" in out and b"THEIRS\n" in out
    assert b"||||||| base" not in out


def test_conflict_handles_missing_sides():
    out = render_conflict("f", None, b"BASE\n", None)
    assert OURS_MARKER in out and THEIRS_MARKER in out
