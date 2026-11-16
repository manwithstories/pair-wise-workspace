"""Tests for UID-based track mapping and its reported downgrades."""

from __future__ import annotations

import unittest

from src.fixture_builder import FixtureSpec, FixtureTrack
from src.track_mapper import (
    TrackMappingError,
    build_track_mapping,
    derive_uid,
)
from tests.helpers import document_from_spec


def spec_with(*tracks: FixtureTrack, **kwargs) -> FixtureSpec:
    return FixtureSpec(tracks=list(tracks), samples_per_cluster=3, **kwargs)


def track(number: int, kind: str, uid: int = 0, **kwargs) -> FixtureTrack:
    codec = {"video": "V_MPEG4/ISO/AVC", "audio": "A_AAC", "subtitle": "S_TEXT/UTF8"}[kind]
    return FixtureTrack(
        number=number,
        uid=uid or number,
        kind=kind,
        codec_id=kwargs.pop("codec_id", codec),
        ticks=kwargs.pop("ticks", [0, 40]),
        **kwargs,
    )


class MappingIdentityTests(unittest.TestCase):
    def test_track_uid_preserved(self) -> None:
        doc = document_from_spec(
            spec_with(track(1, "video", uid=0xABC), track(2, "audio", uid=0xDEF))
        )
        mapping = build_track_mapping(doc)
        self.assertEqual(mapping.by_source_number(1).uid, 0xABC)
        self.assertEqual(mapping.by_source_number(2).uid, 0xDEF)
        self.assertEqual(mapping.by_source_number(1).uid_source, "tag")

    def test_mp4_track_ids_dense_and_ordered(self) -> None:
        doc = document_from_spec(
            spec_with(track(5, "video", uid=1), track(9, "audio", uid=2))
        )
        mapping = build_track_mapping(doc)
        self.assertEqual([t.mp4_track_id for t in mapping.tracks], [1, 2])
        self.assertEqual([t.source_number for t in mapping.tracks], [5, 9])

    def test_original_numbers_preserved_in_report(self) -> None:
        doc = document_from_spec(spec_with(track(7, "video", uid=1)))
        row = build_track_mapping(doc).by_source_number(7).as_dict()
        self.assertEqual(row["source_track_number"], 7)
        self.assertEqual(row["mp4_track_id"], 1)

    def test_language_and_default_preserved(self) -> None:
        doc = document_from_spec(
            spec_with(
                track(1, "video", uid=1, language="eng"),
                track(2, "audio", uid=2, language="jpn", default=False),
            )
        )
        mapping = build_track_mapping(doc)
        self.assertEqual(mapping.by_source_number(1).language, "eng")
        self.assertFalse(mapping.by_source_number(2).default)
        self.assertEqual(mapping.by_source_number(2).language, "jpn")

    def test_default_flag_true_by_default(self) -> None:
        doc = document_from_spec(spec_with(track(1, "audio", uid=1)))
        self.assertTrue(build_track_mapping(doc).by_source_number(1).default)


class DowngradeTests(unittest.TestCase):
    def _doc_without_uid(self):
        spec = spec_with(track(1, "video", uid=3))
        spec.tracks[0].include_uid = False
        return document_from_spec(spec)

    def test_missing_uid_derives_hash_and_reports(self) -> None:
        doc = self._doc_without_uid()
        mapping = build_track_mapping(doc)
        mapped = mapping.by_source_number(1)
        self.assertEqual(mapped.uid_source, "hash")
        self.assertTrue(any(w["code"] == "track_uid_derived" for w in mapping.warnings))
        self.assertTrue(any(w["code"] == "track_uid_missing" for w in doc.warnings))

    def test_derived_uid_is_deterministic(self) -> None:
        first = build_track_mapping(self._doc_without_uid()).by_source_number(1).uid
        second = build_track_mapping(self._doc_without_uid()).by_source_number(1).uid
        self.assertEqual(first, second)

    def test_derived_uid_differs_by_track_attributes(self) -> None:
        doc = self._doc_without_uid()
        source = doc.tracks[0]
        other = source.__class__(**{**source.__dict__, "number": 2, "codec_id": "V_VP9"})
        self.assertNotEqual(derive_uid(source, None)[0], derive_uid(other, None)[0])

    def test_derived_uid_nonzero(self) -> None:
        doc = self._doc_without_uid()
        self.assertGreater(build_track_mapping(doc).by_source_number(1).uid, 0)


class CodecPolicyTests(unittest.TestCase):
    def test_known_codecs_are_remuxable(self) -> None:
        doc = document_from_spec(
            spec_with(track(1, "video", codec_id="V_MPEG4/ISO/AVC"), track(2, "audio", codec_id="A_OPUS"))
        )
        mapping = build_track_mapping(doc)
        self.assertTrue(all(t.remuxable for t in mapping.tracks))
        self.assertEqual(mapping.by_source_number(1).sample_entry, "avc1")

    def test_hevc_maps_to_hvc1(self) -> None:
        doc = document_from_spec(spec_with(track(1, "video", codec_id="V_MPEGH/ISO/HEVC")))
        self.assertEqual(build_track_mapping(doc).by_source_number(1).sample_entry, "hvc1")

    def test_unknown_codec_warns_and_skips(self) -> None:
        doc = document_from_spec(
            spec_with(track(1, "video", codec_id="V_MPEG4/ISO/AVC"), track(2, "audio", codec_id="A_REAL/COOK"))
        )
        mapping = build_track_mapping(doc)
        self.assertFalse(mapping.by_source_number(2).remuxable)
        self.assertEqual(mapping.skipped[0]["source_track_number"], 2)
        self.assertTrue(any(w["code"] == "unsupported_codec" for w in mapping.warnings))

    def test_unknown_codec_strict_raises(self) -> None:
        doc = document_from_spec(
            spec_with(track(1, "video", codec_id="V_MPEG4/ISO/AVC"), track(2, "audio", codec_id="A_REAL/COOK"))
        )
        with self.assertRaises(TrackMappingError) as ctx:
            build_track_mapping(doc, strict_codecs=True)
        self.assertEqual(ctx.exception.code, "unsupported_codec")

    def test_all_unsupported_fails(self) -> None:
        doc = document_from_spec(spec_with(track(1, "audio", codec_id="A_REAL/COOK")))
        with self.assertRaises(TrackMappingError) as ctx:
            build_track_mapping(doc)
        self.assertEqual(ctx.exception.code, "no_remuxable_tracks")

    def test_subtitles_mapped_but_not_remuxable(self) -> None:
        doc = document_from_spec(
            spec_with(track(1, "video"), track(2, "subtitle", codec_id="S_TEXT/UTF8"))
        )
        mapping = build_track_mapping(doc)
        sub = mapping.by_source_number(2)
        self.assertEqual(sub.track_type, "subtitle")
        self.assertEqual(sub.sample_entry, "tx3g")
        self.assertFalse(sub.remuxable)
        self.assertTrue(any("subtitle" in n for n in sub.notes))

    def test_video_dimensions_and_audio_params(self) -> None:
        doc = document_from_spec(
            spec_with(track(1, "video", width=1280, height=720), track(2, "audio", channels=2))
        )
        mapping = build_track_mapping(doc)
        self.assertEqual(mapping.by_source_number(1).width, 1280)
        self.assertEqual(mapping.by_source_number(2).channels, 2)

    def test_summary_shape(self) -> None:
        doc = document_from_spec(spec_with(track(1, "video"), track(2, "audio")))
        summary = build_track_mapping(doc).summary()
        self.assertEqual(summary["track_count"], 2)
        self.assertEqual(summary["remuxable_count"], 2)
        self.assertIn("sample_entry", summary["tracks"][0])


class ConflictTests(unittest.TestCase):
    def test_duplicate_number_rejected(self) -> None:
        doc = document_from_spec(spec_with(track(1, "video", uid=1)))
        doc.tracks.append(doc.tracks[0])
        with self.assertRaises(TrackMappingError) as ctx:
            build_track_mapping(doc)
        self.assertEqual(ctx.exception.code, "track_number_conflict")

    def test_duplicate_uid_rejected(self) -> None:
        doc = document_from_spec(
            spec_with(track(1, "video", uid=42), track(2, "audio", uid=42))
        )
        with self.assertRaises(TrackMappingError) as ctx:
            build_track_mapping(doc)
        self.assertEqual(ctx.exception.code, "track_uid_conflict")


if __name__ == "__main__":
    unittest.main()
