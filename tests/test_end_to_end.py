"""End-to-end acceptance: the three documented commands and their contract."""

from __future__ import annotations

import unittest

import numpy as np

from src.fixture_builder import demo_spec, subtitle_spec, write_fixture
from src.mp4_boxes import BoxRecord, parse_boxes
from src.remux import remux
from src.verify import verify_output
from tests.e2e_helpers import TempDirTestCase, run_cli
from tests.helpers import document_from_spec


class AcceptanceCommandTests(TempDirTestCase):
    """The three documented commands, asserted against the stated ranges."""

    def test_inspect_generated_fixture_contract(self) -> None:
        code, envelope = run_cli(
            ["inspect", "--generate-fixture", "--json", "--fixture-path", str(self.work / "fx.mkv")]
        )
        self.assertEqual(code, 0)
        self.assertTrue(envelope["ok"])
        data = envelope["data"]
        summary = data["summary"]
        self.assertGreaterEqual(summary["element_count"], 30)
        self.assertLessEqual(summary["element_count"], 80)
        self.assertGreaterEqual(summary["unknown_length_count"], 2)
        self.assertGreaterEqual(summary["track_count"], 2)
        self.assertLessEqual(summary["track_count"], 3)
        self.assertGreaterEqual(summary["sample_count"], 10)
        self.assertLessEqual(summary["sample_count"], 40)
        self.assertIn("track_mapping", data)
        self.assertIsNotNone(data["timestamp_plan"])
        for entry in data["timestamp_plan"]["tracks"]:
            self.assertGreaterEqual(entry["rewritten_first_pts"], 0.0)
            self.assertLess(entry["rewritten_first_pts"], entry["rewritten_last_pts"])

    def test_remux_generated_fixture_contract(self) -> None:
        output = self.work / "sample.mp4"
        fx = self.work / "fx.mkv"
        code, envelope = run_cli(
            [
                "remux",
                "--generate-fixture",
                "--fixture-path",
                str(fx),
                "--output",
                str(output),
                "--json",
            ]
        )
        self.assertEqual(code, 0)
        self.assertTrue(envelope["ok"])
        data = envelope["data"]
        self.assertTrue(output.is_file())
        self.assertEqual(data["size"], output.stat().st_size)
        self.assertRegex(data["sha256"], r"^[0-9a-f]{64}$")
        self.assertGreaterEqual(data["box_count"], 20)
        self.assertGreaterEqual(data["mdat_offset"], 0)
        tracks = data["track_mapping"]["tracks"]
        self.assertTrue(2 <= len(tracks) <= 3)
        self.assertEqual(len({t["mp4_track_id"] for t in tracks}), len(tracks))
        for entry in data["timestamps"]["tracks"]:
            self.assertGreaterEqual(entry["rewritten_first_pts"], 0.0)
            self.assertGreater(entry["rewritten_last_pts"], entry["rewritten_first_pts"])
            self.assertEqual(len(entry["repairs"]) >= 6, True)

    def test_verify_command_contract(self) -> None:
        output = self.work / "sample.mp4"
        fx = self.work / "fx.mkv"
        run_cli(["remux", "--generate-fixture", "--fixture-path", str(fx), "--output", str(output), "--json"])
        code, envelope = run_cli(["verify", "--input", str(output), "--json"])
        self.assertEqual(code, 0)
        self.assertTrue(envelope["ok"])
        data = envelope["data"]
        self.assertTrue(data["ok"])
        self.assertEqual(data["failure_count"], 0)
        self.assertRegex(data["sha256"], r"^[0-9a-f]{64}$")
        self.assertGreater(data["box_count"], 20)
        names = {check["name"] for check in data["checks"]}
        for required in ("box_parse", "mdat_size", "chunk_offsets_in_mdat"):
            self.assertIn(required, names)

    def test_demo_timestamps_are_monotonic_per_track(self) -> None:
        doc = document_from_spec(demo_spec())
        result = remux(doc, write_fixture(self.work / "in.mkv", demo_spec()), self.work / "out.mp4")
        for index in result.indexes:
            dts = np.array([sample.dts for sample in index.samples])
            self.assertTrue(np.all(np.diff(dts) > 0), index.track_number)
            self.assertTrue(all(sample.pts >= 0 for sample in index.samples))

    def test_cue_count_reported(self) -> None:
        spec = subtitle_spec()
        spec.include_cues = True
        doc = document_from_spec(spec)
        self.assertGreater(doc.summary()["cue_count"], 0)
        result = remux(doc, write_fixture(self.work / "cued.mkv", spec), self.work / "cued.mp4")
        self.assertGreater(len(result.plan.timelines), 0)


class RoundTripTests(TempDirTestCase):
    def test_output_reparses_with_consistent_offsets(self) -> None:
        mkv = write_fixture(self.work / "in.mkv", demo_spec())
        doc = document_from_spec(demo_spec(), path=mkv)
        result = remux(doc, mkv, self.work / "out.mp4")
        data = result.output_path.read_bytes()
        roots = parse_boxes(data)
        self.assertEqual([r.type for r in roots], [b"ftyp", b"moov", b"mdat"])
        self.assertEqual(roots[-1].end, len(data))
        stco = self._find(roots[1], b"stco")
        self.assertIsNotNone(stco)
        import struct

        count = struct.unpack(">I", data[stco.payload_offset + 4 : stco.payload_offset + 8])[0]
        self.assertGreater(count, 0)
        mdat = roots[-1]
        for index in range(count):
            at = stco.payload_offset + 8 + index * 4
            offset = struct.unpack(">I", data[at : at + 4])[0]
            self.assertGreaterEqual(offset, mdat.payload_offset)
            self.assertLess(offset, mdat.end)

    @staticmethod
    def _find(record: BoxRecord, box_type: bytes):
        return record.find(box_type)

    def test_sample_bytes_match_source(self) -> None:
        mkv = write_fixture(self.work / "in.mkv", demo_spec())
        doc = document_from_spec(demo_spec(), path=mkv)
        result = remux(doc, mkv, self.work / "out.mp4")
        source = mkv.read_bytes()
        output = result.output_path.read_bytes()
        for index in result.indexes:
            for sample in index.samples:
                copied = output[sample.output_offset : sample.output_offset + sample.size]
                original = source[sample.source_offset : sample.source_offset + sample.size]
                self.assertEqual(copied, original, f"track {index.track_number} sample {sample.index}")

    def test_sha256_matches_expected(self) -> None:
        mkv = write_fixture(self.work / "in.mkv", demo_spec())
        doc = document_from_spec(demo_spec(), path=mkv)
        result = remux(doc, mkv, self.work / "out.mp4")
        report = verify_output(result.output_path, expected_sha256=result.sha256)
        self.assertTrue(report.ok)

    def test_sha256_mismatch_fails(self) -> None:
        mkv = write_fixture(self.work / "in.mkv", demo_spec())
        doc = document_from_spec(demo_spec(), path=mkv)
        result = remux(doc, mkv, self.work / "out.mp4")
        report = verify_output(result.output_path, expected_sha256="0" * 64)
        self.assertFalse(report.ok)
        self.assertTrue(any(check.name == "sha256" and not check.ok for check in report.checks))




if __name__ == "__main__":
    unittest.main()
