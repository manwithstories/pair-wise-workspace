"""Unit tests for the EBML primitives (VINTs, payload codecs, schema table)."""

from __future__ import annotations

import unittest

from src import ebml_schema as ids
from src.ebml_schema import (
    SchemaError,
    decode_float,
    decode_int,
    decode_uint,
    decode_vint,
    encode_float,
    encode_int,
    encode_uint,
    encode_unknown_size,
    encode_vint,
    is_unknown_size,
    vint_mask,
    vint_width,
)


class VintWidthTests(unittest.TestCase):
    def test_widths_from_leading_byte(self) -> None:
        self.assertEqual(vint_width(0x81), 1)
        self.assertEqual(vint_width(0x41), 2)
        self.assertEqual(vint_width(0x21), 3)
        self.assertEqual(vint_width(0x1F), 4)
        self.assertEqual(vint_width(0x01), 8)

    def test_zero_byte_rejected(self) -> None:
        with self.assertRaises(SchemaError) as ctx:
            vint_width(0x00)
        self.assertEqual(ctx.exception.code, "vint_no_marker")

    def test_round_trip_shortest_width(self) -> None:
        for value in (0, 1, 126, 127, 128, 16382, 16383, 10**6, 2**56 - 2):
            encoded = encode_vint(value)
            decoded, width = decode_vint(encoded, keep_marker=False)
            self.assertEqual(decoded, value)
            self.assertEqual(width, len(encoded))

    def test_explicit_width_pads(self) -> None:
        self.assertEqual(encode_vint(1, 4), b"\x10\x00\x00\x01")
        self.assertEqual(decode_vint(encode_vint(1, 4), keep_marker=False), (1, 4))

    def test_overflow_rejected(self) -> None:
        # 127 fits in one octet but would read back as the unknown-size
        # sentinel, so the encoder must widen instead of emitting 0xFF.
        with self.assertRaises(SchemaError) as ctx:
            encode_vint(vint_mask(1), width=1)
        self.assertEqual(ctx.exception.code, "vint_overflow")
        self.assertEqual(encode_vint(vint_mask(1)), b"\x40\x7f")

    def test_never_encodes_unknown_sentinel(self) -> None:
        for value in (127, 16383, 2**21 - 1):
            width = 9 - (encode_vint(value)[0].bit_length())
            self.assertNotEqual(encode_vint(value), b"\xff" * width)

    def test_id_round_trip_keeps_marker(self) -> None:
        raw, width = decode_vint(ids.CLUSTER, keep_marker=True)
        self.assertEqual(width, 4)
        self.assertEqual(raw.to_bytes(4, "big"), ids.CLUSTER)
        self.assertEqual(encode_vint(raw, keep_marker=True), ids.CLUSTER)

    def test_unknown_size_sentinel(self) -> None:
        blob = encode_unknown_size(8)
        value, width = decode_vint(blob, keep_marker=False)
        self.assertTrue(is_unknown_size(value, width))
        self.assertFalse(is_unknown_size(0, 1))

    def test_truncated_vint(self) -> None:
        with self.assertRaises(SchemaError) as ctx:
            decode_vint(b"\x41", keep_marker=False)
        self.assertEqual(ctx.exception.code, "vint_truncated")


class PayloadCodecTests(unittest.TestCase):
    def test_uint_round_trip(self) -> None:
        for value in (0, 1, 255, 256, 1000000, 2**63):
            self.assertEqual(decode_uint(encode_uint(value)), value)
        self.assertEqual(decode_uint(b""), 0)

    def test_int_signed(self) -> None:
        for value in (-128, -1, 0, 1, 127, -32768, 32767):
            self.assertEqual(decode_int(encode_int(value)), value)

    def test_int_width_grows(self) -> None:
        self.assertEqual(len(encode_int(127)), 1)
        self.assertEqual(len(encode_int(128)), 2)
        self.assertEqual(len(encode_int(-129)), 2)

    def test_float_widths(self) -> None:
        self.assertAlmostEqual(decode_float(encode_float(1.5, 4)), 1.5, places=5)
        self.assertEqual(decode_float(encode_float(1.5, 8)), 1.5)
        with self.assertRaises(SchemaError):
            decode_float(b"\x00\x00\x00")

    def test_uint_too_wide(self) -> None:
        with self.assertRaises(SchemaError) as ctx:
            decode_uint(b"\x00" * 9)
        self.assertEqual(ctx.exception.code, "uint_too_wide")


class SchemaTableTests(unittest.TestCase):
    def test_known_ids_present(self) -> None:
        for eid in (ids.SEGMENT, ids.INFO, ids.TRACKS, ids.CLUSTER, ids.SIMPLE_BLOCK, ids.CUES):
            self.assertIn(eid, ids.SCHEMA)

    def test_master_classification(self) -> None:
        self.assertTrue(ids.is_master(ids.SEGMENT))
        self.assertTrue(ids.is_master(ids.CLUSTER))
        self.assertFalse(ids.is_master(ids.SIMPLE_BLOCK))
        self.assertFalse(ids.is_master(ids.CODEC_ID))

    def test_names(self) -> None:
        self.assertEqual(ids.name_for(ids.TIMESTAMP_SCALE), "TimecodeScale")
        self.assertTrue(ids.name_for(b"\x99").startswith("Unknown:"))

    def test_unknown_size_allowed_only_for_containers(self) -> None:
        self.assertIn(ids.SEGMENT, ids.UNKNOWN_SIZE_ALLOWED)
        self.assertIn(ids.CLUSTER, ids.UNKNOWN_SIZE_ALLOWED)
        self.assertNotIn(ids.SIMPLE_BLOCK, ids.UNKNOWN_SIZE_ALLOWED)


if __name__ == "__main__":
    unittest.main()
