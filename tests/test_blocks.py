"""Tests for block payload decoding (track numbers, lacing, keyframe flags)."""

from __future__ import annotations

import unittest

from src.matroska_model import ModelError, parse_block_payload


def block_payload(
    track: int, relative: int, flags: int, frames: bytes, *, lace_count: int | None = None
) -> bytes:
    from src.ebml_schema import encode_vint

    body = encode_vint(track) + relative.to_bytes(2, "big", signed=True) + bytes([flags])
    if lace_count is not None:
        body += bytes([lace_count - 1])
    return body + frames


class SimpleBlockTests(unittest.TestCase):
    def test_single_frame(self) -> None:
        payload = block_payload(1, -5, 0x80, b"abcdef")
        track, rel, flags, lacing, frames = parse_block_payload(
            payload, data_offset=1000, cluster_timecode=100, is_simple=True
        )
        self.assertEqual(track, 1)
        self.assertEqual(rel, -5)
        self.assertEqual(flags, 0x80)
        self.assertEqual(lacing, "none")
        self.assertEqual(frames, [(1000 + len(payload) - 6, 6)])

    def test_two_byte_track_number(self) -> None:
        from src.ebml_schema import encode_vint

        prefix = encode_vint(200) + (0).to_bytes(2, "big", signed=True) + b"\x00"
        track, _, _, _, frames = parse_block_payload(
            prefix + b"xyz", data_offset=0, cluster_timecode=0, is_simple=True
        )
        self.assertEqual(track, 200)
        self.assertEqual(frames[0][1], 3)

    def test_keyframe_flag_semantics(self) -> None:
        payload = block_payload(1, 0, 0x00, b"ab")
        flags = parse_block_payload(payload, data_offset=0, cluster_timecode=0, is_simple=True)[2]
        self.assertFalse(flags & 0x80)

    def test_truncated_block(self) -> None:
        with self.assertRaises(ModelError) as ctx:
            parse_block_payload(b"\x81\x00", data_offset=0, cluster_timecode=0, is_simple=True)
        self.assertEqual(ctx.exception.code, "block_truncated")


class LacingTests(unittest.TestCase):
    def test_fixed_lacing(self) -> None:
        payload = block_payload(1, 0, 0x04, b"aaaabbbbcccc", lace_count=3)
        lacing, frames = parse_block_payload(
            payload, data_offset=0, cluster_timecode=0, is_simple=True
        )[3:]
        self.assertEqual(lacing, "fixed")
        self.assertEqual([size for _, size in frames], [4, 4, 4])

    def test_fixed_lacing_not_divisible(self) -> None:
        payload = block_payload(1, 0, 0x04, b"aaabbbcc", lace_count=3)
        with self.assertRaises(ModelError) as ctx:
            parse_block_payload(payload, data_offset=0, cluster_timecode=0, is_simple=True)
        self.assertEqual(ctx.exception.code, "block_lacing_invalid")

    def test_xiph_lacing(self) -> None:
        # Two explicit sizes (2, 3); the last frame absorbs the remaining 3.
        lace = bytes([2, 3]) + b"a" * 2 + b"b" * 3 + b"c" * 3
        payload = block_payload(1, 0, 0x02, lace, lace_count=3)
        lacing, frames = parse_block_payload(
            payload, data_offset=0, cluster_timecode=0, is_simple=True
        )[3:]
        self.assertEqual(lacing, "xiph")
        self.assertEqual([size for _, size in frames], [2, 3, 3])

    def test_xiph_lacing_continuation_bytes(self) -> None:
        # First lace size 300 => encoded as 255, 45; the last frame takes 7.
        lace = bytes([255, 45]) + b"x" * 300 + b"y" * 7
        payload = block_payload(1, 0, 0x02, lace, lace_count=2)
        frames = parse_block_payload(payload, data_offset=0, cluster_timecode=0, is_simple=True)[4]
        self.assertEqual([size for _, size in frames], [300, 7])

    def test_ebml_lacing_negative_delta(self) -> None:
        # VINT 0xAA => first frame 42 bytes; delta VINT 0xFA carries raw 122,
        # which is above the sign bit (0x40), so with the bias of 127 it means
        # -5 => 37 bytes; the final frame absorbs the remaining 5.
        lace = bytes([0xAA, 0xFA]) + b"z" * (42 + 37 + 5)
        payload = block_payload(1, 0, 0x06, lace, lace_count=3)
        lacing, frames = parse_block_payload(
            payload, data_offset=0, cluster_timecode=0, is_simple=True
        )[3:]
        self.assertEqual(lacing, "ebml")
        self.assertEqual([size for _, size in frames], [42, 37, 5])

    def test_ebml_lacing_positive_delta(self) -> None:
        # VINT 0xAA => 42; delta VINT 0x83 carries raw 3, below the sign bit
        # (0x40) => +3 => 45; the final frame takes the remaining 13.
        lace = bytes([0xAA, 0x83]) + b"z" * (42 + 45 + 13)
        payload = block_payload(1, 0, 0x06, lace, lace_count=3)
        frames = parse_block_payload(payload, data_offset=0, cluster_timecode=0, is_simple=True)[4]
        self.assertEqual([size for _, size in frames], [42, 45, 13])

    def test_negative_final_frame_rejected(self) -> None:
        # First frame 200 bytes, second 30 bytes => the remainder underflows.
        lace = bytes([0xB1, 0xC8, 0x9E]) + b"z" * 10
        payload = block_payload(1, 0, 0x06, lace, lace_count=3)
        with self.assertRaises(ModelError) as ctx:
            parse_block_payload(payload, data_offset=0, cluster_timecode=0, is_simple=True)
        self.assertEqual(ctx.exception.code, "block_lacing_invalid")

    def test_lacing_count_zero_means_one_frame(self) -> None:
        payload = block_payload(1, 0, 0x02, b"abcd", lace_count=1)
        frames = parse_block_payload(payload, data_offset=0, cluster_timecode=0, is_simple=True)[4]
        self.assertEqual(len(frames), 1)
        self.assertEqual(frames[0][1], 4)


if __name__ == "__main__":
    unittest.main()
