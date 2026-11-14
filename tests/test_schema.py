from mkvtool.ebml.schema import (
    CLUSTER_ID,
    SEGMENT_ID,
    SIMPLE_BLOCK_ID,
    TRACKS_ID,
    can_contain,
    get_spec,
    is_master,
)


def test_known_types_and_master_ids():
    assert is_master(SEGMENT_ID)
    assert is_master(TRACKS_ID)
    assert not is_master(SIMPLE_BLOCK_ID)
    assert get_spec(SIMPLE_BLOCK_ID).type == "binary"


def test_parent_child_constraints():
    assert can_contain(SEGMENT_ID, TRACKS_ID)
    assert can_contain(CLUSTER_ID, SIMPLE_BLOCK_ID)
    assert not can_contain(CLUSTER_ID, TRACKS_ID)


def test_unknown_vendor_elements_are_allowed():
    assert can_contain(CLUSTER_ID, 0x26A1)
    assert get_spec(0x26A1).type == "binary"
