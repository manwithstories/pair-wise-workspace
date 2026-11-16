"""Tests for the streaming EBML reader: nesting, unknown sizes, truncation."""

from __future__ import annotations

import unittest

from src import ebml_schema as ids
from src.ebml_reader import ReaderError, ReaderOptions, parse_bytes
from src.ebml_schema import Strictness, encode_uint, encode_vint, encode_unknown_size


def element(eid: bytes, payload: bytes = b"", *, size: int | None = None) -> bytes:
    raw_size = encode_vint(len(payload) if size is None else size)
    return eid + raw_size + payload


def header_sized(eid: bytes, payload: bytes, declared: bytes) -> bytes:
    return eid + declared + payload


class ReaderBasicsTests(unittest.TestCase):
    def test_flat_document(self) -> None:
        doc = element(ids.EBML_HEADER, element(ids.DOC_TYPE, b"matroska"))
        root, reader = parse_bytes(doc)
        self.assertEqual(reader.element_count, 2)
        children = root.children[0].children
        self.assertEqual(children[0].data, b"matroska")
        self.assertEqual(children[0].data_offset, 8)

    def test_offsets_are_absolute(self) -> None:
        doc = element(ids.EBML_HEADER, element(ids.DOC_TYPE, b"matroska"))
        root, _ = parse_bytes(doc)
        ebml = root.children[0]
        self.assertEqual(ebml.offset, 0)
        self.assertEqual(ebml.size_width, 1)
        self.assertEqual(ebml.data_offset, 5)
        self.assertEqual(ebml.end_offset, len(doc))

    def test_known_size_master_children(self) -> None:
        payload = element(ids.TIMESTAMP_SCALE, encode_uint(1_000_000))
        doc = element(ids.SEGMENT, element(ids.INFO, payload))
        root, _ = parse_bytes(doc)
        segment = root.children[0]
        info = segment.children[0]
        self.assertEqual(info.name, "Info")
        self.assertEqual(info.children[0].name, "TimecodeScale")
        self.assertEqual(info.children[0].declared_size, 3)

    def test_declared_size_zero(self) -> None:
        doc = element(ids.SEGMENT, element(ids.CUES, b""))
        root, _ = parse_bytes(doc)
        cues = root.children[0].children[0]
        self.assertEqual(cues.children, [])
        self.assertEqual(cues.resolved_size, 0)


class UnknownSizeTests(unittest.TestCase):
    def _segment_with_unknown_size(self) -> bytes:
        cluster = (
            ids.CLUSTER
            + encode_unknown_size(8)
            + element(ids.TIMESTAMP, encode_uint(0))
            + element(ids.SIMPLE_BLOCK, b"\x81\x00\x00\x00", size=4)
        )
        payload = element(ids.INFO, element(ids.TIMESTAMP_SCALE, encode_uint(1))) + cluster
        return element(ids.EBML_HEADER, element(ids.DOC_TYPE, b"matroska")) + (
            ids.SEGMENT + encode_unknown_size(8) + payload
        )

    def test_unknown_size_segment_closes_at_eof(self) -> None:
        doc = self._segment_with_unknown_size()
        root, reader = parse_bytes(
            doc, ReaderOptions(keep_payload_ids=frozenset({ids.SIMPLE_BLOCK}))
        )
        self.assertEqual(reader.unknown_length_count, 2)
        segment = root.children[1]
        self.assertTrue(segment.unknown_size)
        self.assertEqual(segment.end_offset, len(doc))
        self.assertEqual([c.name for c in segment.children], ["Info", "Cluster"])

    def test_unknown_size_cluster_closes_when_sibling_arrives(self) -> None:
        cluster = ids.CLUSTER + encode_unknown_size(8) + element(ids.TIMESTAMP, encode_uint(7))
        cues = element(ids.CUES, b"")
        doc = element(ids.SEGMENT, cluster + cues)
        root, _ = parse_bytes(doc)
        segment = root.children[0]
        self.assertEqual([c.name for c in segment.children], ["Cluster", "Cues"])
        self.assertEqual(segment.children[0].children[0].data, encode_uint(7))

    def test_strict_rejects_unknown_size_on_leaf(self) -> None:
        doc = element(ids.SEGMENT, ids.TIMESTAMP + encode_unknown_size(1))
        with self.assertRaises(ReaderError) as ctx:
            parse_bytes(doc)
        self.assertEqual(ctx.exception.code, "ebml_unknown_size_illegal")


class ErrorTests(unittest.TestCase):
    def test_truncated_payload(self) -> None:
        doc = ids.SEGMENT + encode_vint(10) + b"\x15\x49\xa9\x66"
        with self.assertRaises(ReaderError) as ctx:
            parse_bytes(doc)
        self.assertEqual(ctx.exception.code, "ebml_truncated")

    def test_zero_id_byte(self) -> None:
        with self.assertRaises(ReaderError) as ctx:
            parse_bytes(b"\x00\x00")
        self.assertEqual(ctx.exception.code, "vint_no_marker")

    def test_id_wider_than_four_octets(self) -> None:
        with self.assertRaises(ReaderError) as ctx:
            parse_bytes(b"\x04\x00\x00\x00\x00\x81\x00")
        self.assertEqual(ctx.exception.code, "ebml_id_too_wide")

    def test_unknown_element_strict_fails(self) -> None:
        doc = b"\x99" + encode_vint(1) + b"\x00"
        with self.assertRaises(ReaderError) as ctx:
            parse_bytes(doc)
        self.assertEqual(ctx.exception.code, "ebml_unknown_element")

    def test_unknown_element_lenient_warns(self) -> None:
        doc = b"\x99" + encode_vint(1) + b"\x00"
        root, reader = parse_bytes(
            doc, ReaderOptions(strictness=Strictness.LENIENT)
        )
        self.assertEqual(len(root.children), 1)
        self.assertEqual(reader.warnings[0]["code"], "unknown_element")

    def test_child_overruns_parent_boundary(self) -> None:
        payload = element(ids.INFO, b"")
        # Declare a parent size that stops inside the INFO header.
        doc = ids.SEGMENT + encode_vint(2) + payload
        with self.assertRaises(ReaderError) as ctx:
            parse_bytes(doc)
        self.assertEqual(ctx.exception.code, "ebml_truncated")

    def test_depth_limit(self) -> None:
        block = element(ids.SIMPLE_BLOCK, b"\x81\x00\x00\x00")
        doc = element(ids.SEGMENT, element(ids.CLUSTER, block))
        with self.assertRaises(ReaderError) as ctx:
            parse_bytes(doc, ReaderOptions(max_depth=1))
        self.assertEqual(ctx.exception.code, "ebml_depth_limit")

    def test_element_limit(self) -> None:
        payload = element(ids.INFO, b"") + element(ids.TRACKS, b"")
        doc = element(ids.SEGMENT, payload)
        with self.assertRaises(ReaderError) as ctx:
            parse_bytes(doc, ReaderOptions(max_elements=2))
        self.assertEqual(ctx.exception.code, "ebml_element_limit")

    def test_payload_not_retained_when_large(self) -> None:
        entry = element(ids.TRACK_ENTRY, element(ids.CODEC_PRIVATE, b"x" * 32))
        doc = element(ids.SEGMENT, element(ids.TRACKS, entry))
        root, _ = parse_bytes(doc, ReaderOptions(max_payload=8))
        leaf = root.children[0].children[0].children[0].children[0]
        self.assertIsNone(leaf.data)
        self.assertEqual(leaf.declared_size, 32)


class StateTests(unittest.TestCase):
    def test_parent_mismatch_strict(self) -> None:
        # TimecodeScale is only legal inside Info.
        doc = element(ids.TRACKS, element(ids.TIMESTAMP_SCALE, encode_uint(1)))
        with self.assertRaises(ReaderError) as ctx:
            parse_bytes(doc)
        self.assertEqual(ctx.exception.code, "ebml_parent_invalid")

    def test_unknown_size_container_with_unknown_element_kept(self) -> None:
        cluster = ids.CLUSTER + encode_unknown_size(1) + b"\x99" + encode_vint(0)
        doc = element(ids.SEGMENT, cluster)
        root, reader = parse_bytes(doc, ReaderOptions(strictness=Strictness.LENIENT))
        self.assertEqual(len(root.children[0].children[0].children), 1)
        self.assertTrue(any(w["code"] == "unknown_element" for w in reader.warnings))


if __name__ == "__main__":
    unittest.main()
