"""错误模型的序列化、分类与合并测试。"""

from mediasync.errors import ErrorBook, ErrorCode


def test_error_book_fatal_classification():
    book = ErrorBook()
    book.add(ErrorCode.BITSTREAM_TRUNCATED, "裸码流被截断", source="v.h264", offset=128)
    book.add(ErrorCode.COLOR_MISSING, "缺少色彩矩阵", source="v.h264")

    assert len(book) == 2
    assert book.any_fatal() is True
    assert book.fatal()[0].code == "bitstream_truncated"
    assert book.has(ErrorCode.COLOR_MISSING)
    assert book.summary() == {"bitstream_truncated": 1, "color_missing": 1}


def test_error_book_json_roundtrip():
    book = ErrorBook()
    book.add(
        ErrorCode.INDEX_DUPLICATE,
        "重复索引项",
        source="v.h265",
        offset=64,
        detail={"tick": 100},
        fatal=True,
    )

    restored = ErrorBook.from_json(book.to_json())
    assert len(restored) == 1
    err = restored.errors[0]
    assert err.code == "index_duplicate"
    assert err.offset == 64
    assert err.detail == {"tick": 100}
    assert err.fatal is True
    # 键排序保证序列化结果稳定，可用于重复运行一致性核对。
    assert book.to_json() == restored.to_json()


def test_error_book_merge_and_unknown_code_defaults_nonfatal():
    left = ErrorBook()
    left.add("custom_note", "自定义非致命记录")
    right = ErrorBook()
    right.add(ErrorCode.CHECKPOINT_CORRUPT, "块校验失败")

    left.merge(right)
    assert len(left) == 2
    assert left.fatal()[0].code == "checkpoint_corrupt"
    assert left.errors[0].fatal is False
