"""色彩元数据裁决测试：缺失补齐、冲突保留来源与确定性选择。"""

import pytest

from mediasync.color import (
    CONFLICT_PRIORITY,
    ColorMetadata,
    fill_missing,
    reconcile,
)
from mediasync.errors import ErrorBook, ErrorCode


def test_fill_missing_records_each_field():
    book = ErrorBook()
    meta = ColorMetadata(range="full")
    out = fill_missing(
        meta,
        {"matrix": "bt2020nc", "transfer": "smpte2084", "primaries": "bt2020"},
        source="v.h265",
        errors=book,
    )
    assert out.range == "full"
    assert out.matrix == "bt2020nc"
    missing = book.by_code(ErrorCode.COLOR_MISSING)
    assert {e.detail["field"] for e in missing} == {"matrix", "transfer", "primaries"}


def test_fill_missing_without_policy_refuses_guess():
    book = ErrorBook()
    out = fill_missing(ColorMetadata(), {}, source="v.av1", errors=book)
    assert out.matrix is None
    conflict = book.by_code(ErrorCode.COLOR_CONFLICT)
    assert len(conflict) == len(("range", "matrix", "transfer", "primaries"))


def test_reconcile_conflict_keeps_sources_and_picks_deterministically():
    book = ErrorBook()
    observations = {
        "rep0": ColorMetadata(range="limited", matrix="bt709", transfer="bt709", primaries="bt709"),
        "rep1": ColorMetadata(range="full", matrix="bt2020nc", transfer="hlg", primaries="bt2020"),
    }
    out = reconcile(observations, errors=book)
    conflicts = book.by_code(ErrorCode.COLOR_CONFLICT)
    conflict_fields = {e.detail["field"] for e in conflicts}
    assert conflict_fields == {"range", "matrix", "transfer", "primaries"}
    # 来源原值保留在错误摘要中。
    range_err = next(e for e in conflicts if e.detail["field"] == "range")
    assert range_err.detail["sources"] == {"rep0": "limited", "rep1": "full"}
    # 裁决值与优先级表一致。
    assert out.range == CONFLICT_PRIORITY["range"][0] == "limited"
    assert out.matrix == "bt709" and out.transfer == "bt709" and out.primaries == "bt709"


def test_reconcile_agreement_then_fill_is_stable():
    book = ErrorBook()
    observations = {
        "rep0": ColorMetadata(range="full", matrix="bt2020nc"),
        "rep1": ColorMetadata(range="full", matrix="bt2020nc"),
    }
    first = reconcile(observations, errors=book)
    second = reconcile(observations, errors=ErrorBook())
    assert first.to_dict() == second.to_dict()
    assert not book.by_code(ErrorCode.COLOR_CONFLICT)
    assert first.transfer == "bt709"  # 缺省补齐


def test_reconcile_unknown_term_flagged():
    book = ErrorBook()
    reconcile(
        {"rep0": ColorMetadata(matrix="weird-matrix")},
        errors=book,
    )
    assert any(e.detail.get("value") == "weird-matrix" for e in book.errors)


def test_metadata_json_roundtrip():
    meta = ColorMetadata(range="full", matrix="bt2020nc", transfer="hlg", primaries="bt2020")
    assert ColorMetadata.from_dict(meta.to_dict()) == meta


@pytest.mark.parametrize("field", ["range", "matrix", "transfer", "primaries"])
def test_priority_table_covers_known_terms(field):
    from mediasync.color import _TERM_TABLE

    assert set(_TERM_TABLE[field]) <= set(CONFLICT_PRIORITY[field])
