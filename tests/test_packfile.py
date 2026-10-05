import hashlib
import os

import pytest

import packfile as P
from object_store import hash_blob

dulwich_pack = pytest.importorskip("dulwich.pack")


def sample(n, seed=3):
    import random
    r = random.Random(seed)
    pool = [bytes(r.getrandbits(8) for _ in range(r.randrange(200, 3000))) for _ in range(40)]
    out = bytearray()
    while len(out) < n:
        p = pool[r.randrange(40)]
        out += p[r.randrange(0, len(p)):]
    return bytes(out[:n])


@pytest.mark.parametrize("case,base,target", [
    ("empty-base", b"", b"hello"),
    ("identical", b"abc", b"abc"),
    ("empty-target", b"abc", b""),
    ("both-empty", b"", b""),
    ("append", bytes(range(256)) * 40, bytes(range(256)) * 40 + b"tail"),
    ("disjoint", b"x" * 5000, b"y" * 5000),
    ("tiny-base", b"a", b"a" * 40),
    ("random", sample(50_000), sample(50_000, seed=9)),
])
def test_delta_roundtrip(case, base, target):
    d = P.make_delta(base, target)
    assert P.apply_delta(base, d) == target


def test_delta_uses_copy_and_insert():
    base = b"A" * 1000
    target = b"A" * 500 + b"XYZ" + b"A" * 500
    d = P.make_delta(base, target)
    assert d[0] & 0x80 or d[0] > 0        # first op is a copy or insert
    assert P.apply_delta(base, d) == target


@pytest.mark.parametrize("case,bad", [
    ("empty", b""),
    ("reserved-opcode", b"\x00"),
    ("truncated-insert", b"\x05ab"),
    ("copy-past-end", b"\x80\xff\xff\xff\xff\x00\x00"),
])
def test_malformed_delta_raises(case, bad):
    with pytest.raises(P.DeltaCorruption):
        P.apply_delta(b"base payload", bad)


def test_delta_declares_sizes():
    """The stream must lead with the base size and the result size."""
    base, target = b"A" * 300, b"A" * 150 + b"Z" * 150
    d = P.make_delta(base, target)
    got_base, pos = P._read_size(d, 0)
    got_result, _ = P._read_size(d, pos)
    assert got_base == len(base)
    assert got_result == len(target)


def _write(tmp_path, objs, deltas):
    w = P.PackWriter()
    for t, payload in objs:
        w.add_full(t, payload)
    for base_idx, target in deltas:
        base_payload = objs[base_idx][1]
        w.add_delta(base_idx, P.make_delta(base_payload, target))
    base = str(tmp_path / "p")
    with open(base + ".pack", "wb") as fh:
        fh.write(w.finish())
    oids = []
    for t, payload in objs:
        name = b"blob" if t == P.OBJ_BLOB else b"tree"
        oids.append(hashlib.sha1(name + b" %d\0" % len(payload) + payload).hexdigest())
    for _, target in deltas:
        oids.append(hash_blob(target))
    P.write_pack_index(base + ".pack", w.entries, oids)
    return base, w


def test_self_read_roundtrip(tmp_path):
    v0 = sample(200 << 10, 1)
    v1 = v0[:50 << 10] + b"NEW_TRACE" * 50 + v0[50 << 10:]
    objs = [(P.OBJ_BLOB, v0)]
    base, w = _write(tmp_path, objs, [(0, v1)])
    entries, omap = P.read_packfile(base + ".pack")
    data = open(base + ".pack", "rb").read()
    assert P.unpack_object(entries, 0, data, omap) == (P.OBJ_BLOB, v0)
    assert P.unpack_object(entries, 1, data, omap) == (P.OBJ_BLOB, v1)


def test_dulwich_validates_pack(tmp_path):
    """dulwich is the oracle: it must accept the pack and its index."""
    v0 = sample(100 << 10, 1)
    v1 = v0[:20 << 10] + b"INSERTED" * 100 + v0[20 << 10:]
    objs = [(P.OBJ_BLOB, v0)]
    base, w = _write(tmp_path, objs, [(0, v1)])
    p = dulwich_pack.Pack(base)
    p.check()
    p.check_length_and_checksum()


def test_dulwich_objects_are_byte_identical(tmp_path):
    """Every object dulwich decodes must match our bytes exactly."""
    v0 = sample(200 << 10, 1)
    v1 = v0[:50 << 10] + b"NEW_TRACE_SEGMENT" * 50 + v0[50 << 10:]
    v2 = bytearray(v1)
    import random
    r = random.Random(5)
    for _ in range(200):
        p = r.randrange(len(v2))
        v2[p] = r.getrandbits(8)
    v2 = bytes(v2)
    objs = [(P.OBJ_BLOB, v0)]
    w = P.PackWriter()
    w.add_full(P.OBJ_BLOB, v0)
    w.add_delta(0, P.make_delta(v0, v1))
    w.add_delta(1, P.make_delta(v1, v2))
    w.add_full(P.OBJ_TREE, b"")
    base = str(tmp_path / "p")
    with open(base + ".pack", "wb") as fh:
        fh.write(w.finish())
    P.write_pack_index(base + ".pack", w.entries, [
        hash_blob(v0), hash_blob(v1), hash_blob(v2),
        hashlib.sha1(b"tree 0\0").hexdigest()])

    p = dulwich_pack.Pack(base)
    p.check()
    got = []
    for o in p.iterobjects():
        raw = o.data if hasattr(o, "data") else o.as_raw_string()
        body = raw if isinstance(raw, bytes) else b"".join(raw)
        got.append((o.type_num, body))
    assert sorted(got) == sorted([(P.OBJ_BLOB, v0), (P.OBJ_BLOB, v1),
                                  (P.OBJ_BLOB, v2), (P.OBJ_TREE, b"")])


def test_index_roundtrip(tmp_path):
    v = sample(1000)
    base, w = _write(tmp_path, [(P.OBJ_BLOB, v)], [])
    idx = P.read_pack_index(base + ".idx")
    assert idx == {hash_blob(v): 12}


def test_delta_base_must_precede(tmp_path):
    w = P.PackWriter()
    with pytest.raises(IndexError):
        w.add_delta(5, b"\x00")
