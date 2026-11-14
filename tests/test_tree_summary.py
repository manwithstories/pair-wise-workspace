from mkvtool.ebml.vint import encode_element_id, encode_unknown_size, encode_vint
from mkvtool.ebml.tree import parse_ebml
from mkvtool.report.tree_summary import build_tree_summary


def _element(element_id, payload, unknown=False):
    size = encode_unknown_size() if unknown else encode_vint(len(payload))
    return encode_element_id(element_id) + size + payload


def test_tree_summary_counts_unknown_and_depth():
    header = _element(0x1A45DFA3, _element(0x4282, b"matroska"))
    cluster = _element(0x1F43B675, _element(0xE7, b"\x00"), unknown=True)
    segment = _element(0x18538067, cluster, unknown=True)
    result = parse_ebml(header + segment)

    summary = build_tree_summary(result, "sample.mkv")
    assert summary["element_count"] == 5
    assert summary["unknown_size_elements"] == 2
    assert summary["max_depth"] == 3
    assert len(summary["unknown_size_paths"]) == 2
    assert summary["unknown_size_paths"][0]["boundary_reason"] == "eof"
    assert summary["depth_counts"]["1"] == 2
