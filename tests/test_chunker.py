import hashlib
import io
import time

import pytest

import chunker


def sample(n, seed=3):
    """Data with local structure, like a real binary trace."""
    import random
    r = random.Random(seed)
    pool = [bytes(r.getrandbits(8) for _ in range(r.randrange(200, 3000))) for _ in range(64)]
    out = bytearray()
    while len(out) < n:
        p = pool[r.randrange(64)]
        out += p[r.randrange(0, len(p)):]
    return bytes(out[:n])


def test_coverage_is_exact():
    data = sample(200_000)
    blocks = chunker.chunk_bytes(data)
    assert b"".join(data[o:o + l] for o, l in blocks) == data


def test_min_max_bounds_enforced():
    data = sample(400_000)
    blocks = chunker.chunk_bytes(data)
    for _, length in blocks[:-1]:
        assert chunker.MIN_SIZE <= length <= chunker.MAX_SIZE
    assert 0 < blocks[-1].length <= chunker.MAX_SIZE


def test_empty_and_tiny_inputs():
    assert chunker.chunk_bytes(b"") == []
    assert [l for _, l in chunker.chunk_bytes(b"a")] == [1]
    assert [l for _, l in chunker.chunk_bytes(b"ab" * 100)] == [200]


def test_data_without_anchor_byte_still_works():
    """Content with no 0x01 byte must fall back to the max-size boundary."""
    data = bytes(x for x in bytes(range(256)) * 40 if x != 0x01)[:9000]
    blocks = chunker.chunk_bytes(data)
    assert b"".join(data[o:o + l] for o, l in blocks) == data
    for _, length in blocks[:-1]:
        assert length <= chunker.MAX_SIZE


def test_boundaries_depend_on_content_not_offset():
    """A prefix insertion must not shift later boundaries."""
    data = sample(300_000)
    prefix = b"XYZ" * 500
    shifted = prefix + data
    a = chunker.chunk_bytes(data)
    b = chunker.chunk_bytes(shifted)
    # Blocks fully inside the shared region must resync to identical content.
    ha = {hashlib.sha1(data[o:o + l]).digest() for o, l in a}
    matches = sum(1 for o, l in b
                  if hashlib.sha1(shifted[o:o + l]).digest() in ha)
    assert matches >= 0.9 * len(a)


def test_dedup_after_mid_file_insert():
    v1 = sample(400_000)
    v2 = v1[:50_000] + b"INSERTED_TRACE" * 100 + v1[50_000:]
    h1 = {hashlib.sha1(v1[o:o + l]).digest() for o, l in chunker.chunk_bytes(v1)}
    shared = sum(1 for o, l in chunker.chunk_bytes(v2)
                 if hashlib.sha1(v2[o:o + l]).digest() in h1)
    assert shared >= 0.95 * len(chunker.chunk_bytes(v1))


def test_dedup_after_front_insert():
    v1 = sample(300_000)
    v2 = b"\x7f" + v1
    h1 = {hashlib.sha1(v1[o:o + l]).digest() for o, l in chunker.chunk_bytes(v1)}
    shared = sum(1 for o, l in chunker.chunk_bytes(v2)
                 if hashlib.sha1(v2[o:o + l]).digest() in h1)
    assert shared >= 0.95 * len(chunker.chunk_bytes(v1))


def test_rabin_slide_matches_recompute():
    """The O(1) window slide must equal recomputing the fingerprint."""
    import random
    data = bytes(random.Random(1).getrandbits(8) for _ in range(600))
    fp = chunker.Rabin()
    fp.seed(data, chunker.WINDOW)
    for i in range(chunker.WINDOW, 600):
        assert fp.slide(data[i - chunker.WINDOW], data[i]) == fp.value_at(data, i + 1)


def test_stream_matches_bytes_path():
    data = sample(200_000)
    streamed = [(o, len(b)) for o, b in chunker.chunk_stream(io.BytesIO(data))]
    assert streamed == [(o, l) for o, l in chunker.chunk_bytes(data)]


def test_stream_handles_small_read_sizes():
    data = sample(50_000)
    for rs in (1, 97, 4096, 1 << 20):
        streamed = b"".join(b for _, b in chunker.chunk_stream(io.BytesIO(data), read_size=rs))
        assert streamed == data


def test_throughput_meets_floor():
    """Single-core chunking must stay above 20 MB/s."""
    data = sample(8 << 20)
    best = float("inf")
    for _ in range(3):
        t = time.perf_counter()
        chunker.chunk_bytes(data)
        best = min(best, time.perf_counter() - t)
    mbps = len(data) / best / 1e6
    assert mbps >= 20.0, f"chunking throughput {mbps:.1f} MB/s is below the 20 MB/s floor"


def test_invalid_bounds_rejected():
    with pytest.raises(ValueError):
        chunker.chunker(min_size=0)
    with pytest.raises(ValueError):
        chunker.chunker(min_size=100, max_size=50)
