"""Tests for sample table reconstruction: stts/ctts/stss/chunks/offsets."""

from __future__ import annotations

import unittest

from src.index_rebuilder import (
    IndexError_,
    build_track_index,
    lay_out_media,
    sample_source_ranges,
    total_media_bytes,
)
from src.timestamp import plan_rewrite
from tests.helpers import make_document


def first_track_index(doc, number: int = 1):
    plan = plan_rewrite(doc)
    blocks = doc.blocks_for_track(doc.track_by_number(number))
    return build_track_index(plan.timeline(number), blocks), plan


class SttsTests(unittest.TestCase):
    def test_constant_frame_rate(self) -> None:
        doc = make_document([("video", 1, [0, 40, 80, 120])], scale_ns=1_000_000)
        index, _ = first_track_index(doc)
        self.assertEqual(index.stts, [(4, 3600)])
        self.assertEqual(index.media_duration, 14_400)
        self.assertFalse(index.has_ctts)

    def test_variable_frame_rate_runs(self) -> None:
        doc = make_document(
            [("video", 1, [0, 40, 80, 120, 200])], scale_ns=1_000_000
        )
        index, _ = first_track_index(doc)
        # Four decode gaps (3600 ×3, 7200 ×1) plus the trailing sample's own
        # nominal duration (3600).
        self.assertEqual(index.stts[0], (3, 3600))
        self.assertEqual(index.stts[1], (1, 7200))
        self.assertEqual(index.stts[2], (1, 3600))

    def test_audio_uses_its_own_timescale(self) -> None:
        doc = make_document([("audio", 2, [0, 21, 42])], scale_ns=1_000_000)
        plan = plan_rewrite(doc)
        blocks = doc.blocks_for_track(doc.track_by_number(2))
        index = build_track_index(plan.timeline(2), blocks)
        self.assertEqual(index.timescale, 48_000)
        self.assertEqual(index.stts, [(3, 1008)])

    def test_empty_track_rejected(self) -> None:
        doc = make_document([("video", 1, [0])], scale_ns=1_000_000)
        plan = plan_rewrite(doc)
        blocks = doc.blocks_for_track(doc.track_by_number(1))
        with self.assertRaises(IndexError_) as ctx:
            build_track_index(plan.timeline(1), blocks[:0])
        self.assertEqual(ctx.exception.code, "sample_count_mismatch")


class CttsTests(unittest.TestCase):
    def test_out_of_order_presentation_produces_ctts(self) -> None:
        # Storage order is decode order; a block whose presentation time is
        # earlier than the previous one is ordinary B-frame reordering, so the
        # PTS is preserved and the difference lands in the composition table.
        doc = make_document([("video", 1, [0, 80, 40, 120])], scale_ns=1_000_000)
        index, _ = first_track_index(doc)
        self.assertTrue(index.has_ctts)
        offsets = [sample.ctts for sample in index.samples]
        self.assertIn(-3600, offsets)
        dts = [sample.dts for sample in index.samples]
        self.assertTrue(all(b > a for a, b in zip(dts, dts[1:])))

    def test_in_order_has_no_ctts(self) -> None:
        doc = make_document([("video", 1, [0, 40, 80])], scale_ns=1_000_000)
        index, _ = first_track_index(doc)
        self.assertFalse(index.has_ctts)
        self.assertEqual(index.ctts, [])

    def test_dts_strictly_increasing_with_reordering(self) -> None:
        doc = make_document([("video", 1, [0, 80, 40, 120, 160])], scale_ns=1_000_000)
        index, _ = first_track_index(doc)
        dts = [sample.dts for sample in index.samples]
        self.assertTrue(all(b > a for a, b in zip(dts, dts[1:])))


class SyncTests(unittest.TestCase):
    def test_keyframes_become_one_based_stss(self) -> None:
        doc = make_document([("video", 1, [0, 40, 80, 120, 160, 200])], scale_ns=1_000_000)
        index, _ = first_track_index(doc)
        self.assertTrue(index.stss)
        self.assertEqual(index.stss[0], 1)
        self.assertTrue(all(i >= 1 for i in index.stss))

    def test_all_intra_omits_stss(self) -> None:
        doc = make_document([("audio", 2, [0, 21, 42])], scale_ns=1_000_000)
        plan = plan_rewrite(doc)
        blocks = doc.blocks_for_track(doc.track_by_number(2))
        index = build_track_index(plan.timeline(2), blocks)
        self.assertEqual(index.stss, [])
        self.assertTrue(any("sync" in note for note in index.notes))


class ChunkTests(unittest.TestCase):
    def test_chunks_partition_samples(self) -> None:
        doc = make_document([("video", 1, list(range(0, 40 * 25, 40)))], scale_ns=1_000_000)
        index, _ = first_track_index(doc)
        covered = [i for chunk in index.chunks for i in chunk.sample_indices]
        self.assertEqual(covered, list(range(25)))
        self.assertEqual(len(index.chunks), 3)

    def test_interleaving_alternates_tracks(self) -> None:
        doc = make_document(
            [("video", 1, [0, 40, 80]), ("audio", 2, [0, 21, 42])],
            scale_ns=1_000_000,
        )
        plan = plan_rewrite(doc)
        indexes = []
        for number in (1, 2):
            blocks = doc.blocks_for_track(doc.track_by_number(number))
            indexes.append(build_track_index(plan.timeline(number), blocks))
        order = lay_out_media(indexes, mdat_data_offset=1000)
        self.assertEqual({chunk.track_number for _, chunk in order}, {1, 2})
        first_offsets = [sample.output_offset for index in indexes for sample in index.samples]
        self.assertEqual(min(first_offsets), 1000)

    def test_offsets_are_contiguous(self) -> None:
        doc = make_document([("video", 1, [0, 40, 80]), ("audio", 2, [0, 21, 42])], scale_ns=1_000_000)
        plan = plan_rewrite(doc)
        indexes = [
            build_track_index(
                plan.timeline(n), doc.blocks_for_track(doc.track_by_number(n))
            )
            for n in (1, 2)
        ]
        lay_out_media(indexes, mdat_data_offset=0)
        spans = sorted(
            (s.output_offset, s.output_offset + s.size)
            for index in indexes
            for s in index.samples
        )
        for (_, end), (start, _) in zip(spans, spans[1:]):
            self.assertEqual(end, start)
        self.assertEqual(total_media_bytes(indexes), spans[-1][1])

    def test_copy_plan_matches_source_offsets(self) -> None:
        doc = make_document([("video", 1, [0, 40])], scale_ns=1_000_000)
        index, _ = first_track_index(doc)
        lay_out_media([index], mdat_data_offset=64)
        for source, output, size in sample_source_ranges(index):
            self.assertGreater(source, 0)
            self.assertGreaterEqual(output, 64)
            self.assertGreater(size, 0)


class IndexSummaryTests(unittest.TestCase):
    def test_summary_is_serializable(self) -> None:
        import json

        doc = make_document([("video", 1, [0, 80, 40, 120])], scale_ns=1_000_000)
        index, _ = first_track_index(doc)
        payload = json.dumps(index.as_dict())
        self.assertIn("sample_count", payload)
        self.assertIn("has_ctts", payload)

    def test_samples_share_track_number(self) -> None:
        doc = make_document([("audio", 2, [0, 21])], scale_ns=1_000_000)
        plan = plan_rewrite(doc)
        index = build_track_index(
            plan.timeline(2), doc.blocks_for_track(doc.track_by_number(2))
        )
        self.assertEqual({s.track_number for s in index.samples}, {2})


if __name__ == "__main__":
    unittest.main()
