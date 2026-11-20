"""工作目录路径越界防护测试。"""

import pytest

from mediasync.errors import ErrorBook, ErrorCode
from mediasync.safety import PathTraversalError, ensure_within, resolve_within


def test_resolve_within_accepts_nested_relative(tmp_path):
    target = resolve_within(tmp_path, "segments/part0/a.wav")
    assert str(target).startswith(str(tmp_path.resolve()))
    assert target.name == "a.wav"


def test_resolve_within_rejects_parent_escape(tmp_path):
    with pytest.raises(PathTraversalError) as exc:
        resolve_within(tmp_path, "../../etc/passwd")
    assert exc.value.code is ErrorCode.PATH_TRAVERSAL


def test_resolve_within_rejects_absolute_outside(tmp_path):
    with pytest.raises(PathTraversalError):
        resolve_within(tmp_path, "/etc/hosts")


def test_ensure_within_records_fatal_and_returns_none(tmp_path):
    book = ErrorBook()
    result = ensure_within(tmp_path, "../escape.bin", book)
    assert result is None
    assert book.has(ErrorCode.PATH_TRAVERSAL)
    assert book.any_fatal()
