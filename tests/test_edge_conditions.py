"""Edge conditions: structural corner cases and failure-mode contracts."""

from __future__ import annotations

import unittest

from src.remux import RemuxError, RemuxOptions, remux
from src.fixture_builder import (
    FixtureSpec,
    FixtureTrack,
    build_fixture,
    demo_spec,
    minimal_spec,
    subtitle_spec,
    write_fixture,
)
from src.timestamp import TimingConfig
from tests.e2e_helpers import TempDirTestCase, _build_laced_document, run_cli
from tests.helpers import document_from_bytes, document_from_spec


class EdgeConditionTests(TempDirTestCase):
    def test_known_size_segment_remuxes(self) -> None:
        spec = minimal_spec()
        mkv = write_fixture(self.work / "min.mkv", spec)
        doc = document_from_spec(spec, path=mkv)
        self.assertFalse(doc.segment_unknown_size)
        result = remux(doc, mkv, self.work / "min.mp4")
        self.assertTrue(result.output_path.is_file())

    def test_multiple_clusters_with_unknown_size(self) -> None:
        spec = demo_spec()
        spec.unknown_clusters = True
        spec.samples_per_cluster = 2
        mkv = write_fixture(self.work / "many.mkv", spec)
        doc = document_from_spec(spec, path=mkv)
        self.assertGreater(doc.summary()["cluster_count"], 3)
        result = remux(doc, mkv, self.work / "many.mp4")
        self.assertEqual(
            sum(index.sample_count for index in result.indexes), doc.summary()["sample_count"]
        )

    def test_empty_cluster_is_skipped(self) -> None:
        spec = demo_spec()
        spec.empty_cluster_at_start = True
        doc = document_from_spec(spec)
        self.assertEqual(doc.clusters[0].blocks, [])
        mkv = write_fixture(self.work / "empty.mkv", spec)
        result = remux(doc, mkv, self.work / "empty.mp4")
        self.assertTrue(result.output_path.is_file())

    def test_missing_cues_is_acceptable(self) -> None:
        spec = subtitle_spec()
        spec.include_cues = False
        doc = document_from_spec(spec)
        self.assertEqual(doc.cues, [])
        mkv = write_fixture(self.work / "nocue.mkv", spec)
        result = remux(doc, mkv, self.work / "nocue.mp4")
        self.assertTrue(result.output_path.is_file())

    def test_three_track_file_maps_video_audio_subtitle(self) -> None:
        spec = subtitle_spec()
        doc = document_from_spec(spec)
        result = remux(doc, write_fixture(self.work / "sub.mkv", spec), self.work / "sub.mp4")
        types = {track.track_type for track in result.mapping.tracks}
        self.assertEqual(types, {"video", "audio", "subtitle"})
        written = {index.track_number for index in result.indexes}
        self.assertEqual(len(written), len(result.mapping.remuxable))

    def test_subtitle_only_plus_audio_still_remuxes(self) -> None:
        spec = FixtureSpec(
            tracks=[
                FixtureTrack(number=1, uid=1, kind="audio", codec_id="A_OPUS", ticks=[0, 20, 40]),
                FixtureTrack(number=2, uid=2, kind="subtitle", codec_id="S_TEXT/UTF8", ticks=[0]),
            ]
        )
        doc = document_from_spec(spec)
        result = remux(doc, write_fixture(self.work / "a.mkv", spec), self.work / "a.mp4")
        self.assertEqual(len(result.indexes), 1)

    def _mixed_codec_spec(self) -> FixtureSpec:
        return FixtureSpec(
            tracks=[
                FixtureTrack(number=1, uid=1, kind="video", codec_id="V_MPEG4/ISO/AVC", ticks=[0, 40]),
                FixtureTrack(number=2, uid=2, kind="audio", codec_id="A_REAL/COOK", ticks=[0, 20]),
            ]
        )

    def test_unsupported_codec_skipped_when_allowed(self) -> None:
        spec = self._mixed_codec_spec()
        doc = document_from_spec(spec)
        mkv = write_fixture(self.work / "mix.mkv", spec)
        result = remux(doc, mkv, self.work / "mix.mp4", options=RemuxOptions(strict_codecs=False))
        self.assertEqual(len(result.indexes), 1)
        self.assertTrue(result.mapping.skipped)

    def test_unsupported_codec_blocks_write_by_default(self) -> None:
        spec = self._mixed_codec_spec()
        doc = document_from_spec(spec)
        mkv = write_fixture(self.work / "mix.mkv", spec)
        output = self.work / "strict.mp4"
        with self.assertRaises(RemuxError) as ctx:
            remux(doc, mkv, output)
        self.assertEqual(ctx.exception.code, "track_mapping_failed")
        self.assertFalse(output.exists())

    def test_cli_unsupported_codec_exit_codes(self) -> None:
        spec = self._mixed_codec_spec()
        mkv = write_fixture(self.work / "mix.mkv", spec)
        default_out = self.work / "default.mp4"
        code, envelope = run_cli(
            ["remux", "--input", str(mkv), "--output", str(default_out), "--json"]
        )
        self.assertEqual(code, 3)
        self.assertEqual(envelope["error"]["code"], "track_mapping_failed")
        self.assertFalse(default_out.exists())

        lenient_out = self.work / "lenient.mp4"
        code, envelope = run_cli(
            ["remux", "--input", str(mkv), "--output", str(lenient_out), "--json", "--lenient"]
        )
        self.assertEqual(code, 0)
        self.assertTrue(lenient_out.is_file())
        self.assertGreaterEqual(envelope["data"]["warning_count"], 1)

    def test_strict_timestamps_rejects_duplicates(self) -> None:
        spec = FixtureSpec(
            tracks=[
                FixtureTrack(
                    number=1, uid=1, kind="video", codec_id="V_MPEG4/ISO/AVC", ticks=[0, 40, 40]
                )
            ]
        )
        doc = document_from_spec(spec)
        mkv = write_fixture(self.work / "dup.mkv", spec)
        with self.assertRaises(RemuxError) as ctx:
            remux(
                doc,
                mkv,
                self.work / "dup.mp4",
                options=RemuxOptions(
                    timing=TimingConfig(repair_duplicates=False)
                ),
            )
        self.assertEqual(ctx.exception.code, "timestamp_rewrite_failed")

    def test_truncated_source_fails_without_partial_output(self) -> None:
        spec = demo_spec()
        raw = build_fixture(spec)
        truncated = raw[: len(raw) - 60]
        broken = self.work / "broken.mkv"
        broken.write_bytes(truncated)
        output = self.work / "broken.mp4"
        with self.assertRaises((cli.CliError, ValueError)):
            cli.main(["remux", "--input", str(broken), "--output", str(output), "--json"])
        self.assertFalse(output.exists())

    def test_corrupt_ebml_exit_code_3(self) -> None:
        bad = self.work / "bad.mkv"
        bad.write_bytes(b"\x1a\x45\xdf\xa3\xff\xff")
        code, envelope = run_cli(["inspect", "--input", str(bad), "--json"])
        self.assertEqual(code, 3)
        self.assertEqual(envelope["error"]["code"], "ebml_corrupt")

    def test_missing_input_exit_code_2(self) -> None:
        code, envelope = run_cli(["remux", "--input", str(self.work / "ghost.mkv"), "--output", str(self.work / "o.mp4"), "--json"])
        self.assertEqual(code, 2)
        self.assertEqual(envelope["error"]["code"], "input_not_found")

    def test_verify_broken_output_exit_code_5(self) -> None:
        broken = self.work / "fake.mp4"
        broken.write_bytes(b"not an mp4 at all")
        code, envelope = run_cli(["verify", "--input", str(broken), "--json"])
        self.assertEqual(code, 5)
        self.assertFalse(envelope["data"]["ok"])

    def test_no_keyframe_video_omits_stss(self) -> None:
        doc = document_from_spec(minimal_spec())
        # Flip every video block to non-keyframe by rewriting the flag byte.
        raw = bytearray(build_fixture(minimal_spec()))
        for block in doc.all_blocks:
            raw[block.data_offset + 3] &= 0x7F
        rebuilt = bytes(raw)
        self.assertEqual(len(rebuilt), len(raw))
        parsed = document_from_bytes(rebuilt)
        self.assertTrue(all(not block.keyframe for block in parsed.all_blocks))
        mkv = self.work / "nokey.mkv"
        mkv.write_bytes(rebuilt)
        result = remux(parsed, mkv, self.work / "nokey.mp4")
        self.assertTrue(all(index.stss == [] for index in result.indexes))

    def test_laced_block_is_rejected(self) -> None:
        # Fixed lacing packs two frames into one Block; an MP4 sample table
        # needs one sample per stsz entry, so the remux must refuse instead of
        # silently dropping the second frame.
        laced = _build_laced_document()
        mkv = self.work / "laced.mkv"
        mkv.write_bytes(laced)
        doc = document_from_bytes(laced, path=mkv)
        self.assertTrue(any(block.lacing == "fixed" for block in doc.all_blocks))
        output = self.work / "laced.mp4"
        with self.assertRaises(RemuxError) as ctx:
            remux(doc, mkv, output)
        self.assertEqual(ctx.exception.code, "timestamp_index_failed")
        self.assertFalse(output.exists())

    def test_truncated_source_fails_without_partial_output(self) -> None:
        spec = demo_spec()
        raw = build_fixture(spec)
        truncated = raw[: len(raw) - 60]
        broken = self.work / "broken.mkv"
        broken.write_bytes(truncated)
        output = self.work / "broken.mp4"
        code, envelope = run_cli(
            ["remux", "--input", str(broken), "--output", str(output), "--json"]
        )
        self.assertEqual(code, 3)
        self.assertEqual(envelope["error"]["code"], "ebml_corrupt")
        self.assertFalse(output.exists())


class CopyIntegrityTests(TempDirTestCase):
    def test_read_back_mismatch_aborts_without_output(self) -> None:
        """A write that does not land at its declared offset must abandon the remux."""
        mkv = write_fixture(self.work / "in.mkv", demo_spec())
        doc = document_from_spec(demo_spec(), path=mkv)
        output = self.work / "out.mp4"
        from src import _remux

        original = _remux._verify_sample_write
        calls = {"n": 0}

        def sabotage(sink, offset, expected, sample):
            calls["n"] += 1
            if calls["n"] == 2:
                raise _remux.RemuxError("copy_mismatch", "injected")
            return original(sink, offset, expected, sample)

        _remux._verify_sample_write = sabotage
        try:
            with self.assertRaises(RemuxError) as ctx:
                remux(doc, mkv, output)
        finally:
            _remux._verify_sample_write = original
        self.assertEqual(ctx.exception.code, "copy_mismatch")
        self.assertFalse(output.exists())
        self.assertEqual([p.name for p in self.work.iterdir() if p.name.endswith(".tmp")], [])


class ResidueTests(TempDirTestCase):
    def test_no_temp_files_after_success(self) -> None:
        mkv = write_fixture(self.work / "in.mkv", demo_spec())
        doc = document_from_spec(demo_spec(), path=mkv)
        remux(doc, mkv, self.work / "out.mp4")
        leftovers = [p.name for p in self.work.iterdir() if p.name.endswith(".tmp")]
        self.assertEqual(leftovers, [])

    def test_overwrite_same_path_is_atomic(self) -> None:
        mkv = write_fixture(self.work / "in.mkv", demo_spec())
        doc = document_from_spec(demo_spec(), path=mkv)
        out = self.work / "out.mp4"
        first = remux(doc, mkv, out)
        second = remux(doc, mkv, out)
        self.assertEqual(first.sha256, second.sha256)




if __name__ == "__main__":
    unittest.main()
