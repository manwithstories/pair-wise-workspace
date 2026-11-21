"""Tests for the transactional SQLite index and checkpoint compatibility."""

import pytest

from archive_media.errors import FatalMediaError
from archive_media.index import (
    MediaIndex,
    canonical_params_digest,
    file_content_hash,
)


def _make_source(tmp_path, name="a.mp4", payload=b"media-bytes"):
    path = tmp_path / name
    path.write_bytes(payload)
    return str(path)


def test_register_then_resume_compatible(tmp_path):
    db = str(tmp_path / "idx.db")
    src = _make_source(tmp_path)
    params = {"chunk_size": 4, "target_kbps": 900}
    with MediaIndex(db) as index:
        first = index.register_or_verify(src, file_content_hash(src), params)
        assert first.chunk_index == 0 and first.resumed is False
        index.commit_chunk(src, 1, 0.5, segments=[{"segment_id": 0, "track": "v"}])
        second = index.register_or_verify(src, file_content_hash(src), params)
        assert second.chunk_index == 1
    # Durable across reopen.
    with MediaIndex(db) as reopened:
        assert len(reopened.load_segments(src)) == 1


def test_changed_params_rejected(tmp_path):
    db = str(tmp_path / "idx.db")
    src = _make_source(tmp_path)
    with MediaIndex(db) as index:
        index.register_or_verify(src, file_content_hash(src), {"chunk_size": 4})
        with pytest.raises(FatalMediaError) as exc:
            index.register_or_verify(src, file_content_hash(src), {"chunk_size": 8})
        assert exc.value.code == "incompatible_checkpoint"


def test_changed_content_rejected(tmp_path):
    db = str(tmp_path / "idx.db")
    src = _make_source(tmp_path)
    params = {"chunk_size": 4}
    with MediaIndex(db) as index:
        index.register_or_verify(src, file_content_hash(src), params)
        _make_source(tmp_path, payload=b"different-bytes")
        with pytest.raises(FatalMediaError):
            index.register_or_verify(src, file_content_hash(src), params)


def test_chunk_commit_is_atomic_and_readable(tmp_path):
    db = str(tmp_path / "idx.db")
    src = _make_source(tmp_path)
    with MediaIndex(db) as index:
        index.register_or_verify(src, file_content_hash(src), {})
        index.commit_chunk(
            src,
            0,
            1.0,
            silence=[{"start_s": 0.1, "end_s": 0.2}],
            fingerprints=[("video", 0, "hash123")],
            errors=[{"code": "corrupt_packet"}],
        )
        assert index.load_silence(src)[0]["end_s"] == 0.2
        assert index.load_fingerprint_hashes(src) == ["hash123"]
        assert index.load_errors(src)[0]["code"] == "corrupt_packet"


def test_canonical_digest_order_independent():
    assert canonical_params_digest({"a": 1, "b": 2}) == canonical_params_digest(
        {"b": 2, "a": 1}
    )
