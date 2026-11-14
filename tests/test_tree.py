import pytest

from mkvtool.ebml.tree import parse_ebml
from mkvtool.errors import InputParseError


def _element(element_id: int, payload: bytes, *, unknown: bool = False) -> bytes:
    from mkvtool.ebml.vint import encode_element_id, encode_unknown_size, encode_vint

    size = encode_unknown_size() if unknown else encode_vint(len(payload))
    return encode_element_id(element_id) + size + payload


def _header() -> bytes:
    doctype = _element(0x4282, b"matroska")
    return _element(0x1A45DFA3, doctype)


def test_unknown_size_segment_and_cluster_stop_on_sibling():
    segment_body = _element(0x1549A966, b"")
    cluster = _element(0xE7, b"\x00") + _element(0xA3, b"\x81\x00\x00\x80x")
    segment_body += _element(0x1F43B675, cluster, unknown=True)
    segment_body += _element(0x1C53BB6B, b"")
    data = _header() + _element(0x18538067, segment_body, unknown=True)

    result = parse_ebml(data)
    segment = result.first(0x18538067)
    cluster = result.first(0x1F43B675)
    assert segment.unknown_size and segment.boundary_reason == "eof"
    assert cluster.unknown_size
    assert cluster.boundary_reason.startswith("next_sibling:")
    assert result.first(0x1C53BB6B) is not None


def test_truncated_unknown_cluster_degrades_to_prefix():
    segment_body = _element(0x1F43B675, _element(0xE7, b"\x00"), unknown=True)
    segment_body += b"\xa3\x90\x00\x00"  # Block claims 144 bytes, only four present.
    data = _header() + _element(0x18538067, segment_body, unknown=True)

    result = parse_ebml(data)
    assert result.truncated
    assert result.first(0xE7) is not None
    assert result.first(0xA3) is None


def test_corrupt_header_is_fatal():
    with pytest.raises(InputParseError):
        parse_ebml(b"not-ebml")


def test_known_parent_overrun_is_fatal():
    segment = _element(0x18538067, b"\xa3\x90\x00")
    with pytest.raises(InputParseError):
        parse_ebml(_header() + segment)


def test_unknown_segment_stops_at_next_ebml_document():
    first = _element(0x18538067, _element(0x1549A966, b""), unknown=True)
    second = _element(0x18538067, _element(0x1549A966, b""))
    result = parse_ebml(_header() + first + _header() + second)
    segments = [item for item in result.roots if item.element_id == 0x18538067]
    assert len(segments) == 2
    assert segments[0].unknown_size
    assert segments[0].boundary_reason == "next_sibling:EBML"
    assert not segments[1].unknown_size
