"""分块检查点测试：原子提交、损坏回滚、校验失败隔离与恢复边界。"""

import json

import pytest

from mediasync.checkpoint import Block, CheckpointStore, sha256_bytes, canonical_json
from mediasync.errors import ErrorBook, ErrorCode


def _store(tmp_path, errors=None):
    book = errors if errors is not None else ErrorBook()
    return CheckpointStore("checkpoint", tmp_path, book), book


def test_commit_block_atomic_and_manifest_updated(tmp_path):
    store, _ = _store(tmp_path)
    block = Block.build(0, [{"keyframe_ms": 0}, {"keyframe_ms": 500}])
    path = store.commit_block(block)

    assert path.exists()
    assert not list(store.blocks_dir.glob("*.tmp"))  # 无临时残留
    manifest = store.load_manifest()
    assert manifest["confirmed_blocks"] == ["000000.json"]
    loaded = store.load_block(0)
    assert loaded.records == block.records
    assert loaded.checksum == block.checksum


def test_interrupted_tmp_block_does_not_poison_committed_state(tmp_path):
    store, _ = _store(tmp_path)
    store.commit_block(Block.build(0, [{"a": 1}]))

    # 模拟块写入中断：只留下 .tmp，没有原子替换。
    tmp = store.blocks_dir / "000001.tmp"
    tmp.write_bytes(canonical_json(Block.build(1, [{"b": 2}]).to_dict())[:12])

    assert store.committed_count() == 1  # 未确认块不计入
    verified = store.verify_blocks()
    assert len(verified) == 1
    # 新运行可从块 1 重新提交，不影响块 0。
    store.commit_block(Block.build(1, [{"b": 2}]))
    assert store.committed_count() == 2


def test_corrupt_confirmed_block_detected(tmp_path):
    book = ErrorBook()
    store, _ = _store(tmp_path, book)
    store.commit_block(Block.build(0, [{"a": 1}]))

    target = store.blocks_dir / "000000.json"
    data = json.loads(target.read_bytes())
    data["records"].append({"injected": True})  # 篡改记录但不改校验和
    target.write_bytes(canonical_json(data))

    with pytest.raises(ValueError):
        store.verify_blocks()
    assert book.has(ErrorCode.CHECKPOINT_CORRUPT)
    assert book.any_fatal()

def test_quarantine_moves_checkpoint_and_allows_restart(tmp_path):
    book = ErrorBook()
    store, _ = _store(tmp_path, book)
    store.commit_block(Block.build(0, [{"a": 1}]))

    dest = store.quarantine("输入摘要不匹配")
    assert dest.exists()
    assert (dest / "QUARANTINE_REASON.txt").read_text().startswith("输入摘要")
    # 原检查点已清空，可从首个安全边界重算。
    assert store.committed_count() == 0
    assert store.load_manifest() is None
    store.commit_block(Block.build(0, [{"a": 1}]))
    assert store.committed_count() == 1


def test_checksum_deterministic_across_runs():
    block = Block.build(3, [{"x": 1}, {"y": 2}])
    again = Block.build(3, [{"x": 1}, {"y": 2}])
    assert block.checksum == again.checksum
    assert len(sha256_bytes(b"abc")) == 64  # sha256 十六进制长度
