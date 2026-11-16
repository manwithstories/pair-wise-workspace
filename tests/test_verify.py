"""Verification tests: the checker must actually fail on broken output."""

from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from src.fixture_builder import demo_spec, write_fixture
from src.remux import remux
from src.verify import verify_output
from tests.helpers import document_from_spec


class VerifierFailureTests(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory(prefix="mkv2mp4-verify-")
        self.work = Path(self._tmp.name)
        mkv = write_fixture(self.work / "in.mkv", demo_spec())
        doc = document_from_spec(demo_spec(), path=mkv)
        self.result = remux(doc, mkv, self.work / "out.mp4")
        self.good = self.result.output_path.read_bytes()

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def _write(self, name: str, data: bytes) -> Path:
        path = self.work / name
        path.write_bytes(data)
        return path

    def test_untouched_output_passes(self) -> None:
        report = verify_output(self.result.output_path, expected_sha256=self.result.sha256)
        self.assertTrue(report.ok, [c.detail for c in report.failures])
        self.assertEqual(report.box_count, self.result.layout.box_count)

    def test_truncated_file_fails(self) -> None:
        path = self._write("trunc.mp4", self.good[: len(self.good) // 2])
        report = verify_output(path)
        self.assertFalse(report.ok)

    def test_extra_trailing_bytes_fail(self) -> None:
        path = self._write("tail.mp4", self.good + b"\x00" * 16)
        report = verify_output(path)
        self.assertFalse(report.ok)

    def test_chunk_offset_outside_mdat_fails(self) -> None:
        data = bytearray(self.good)
        stco = data.find(b"stco")
        first_offset = stco + 4 + 8
        data[first_offset : first_offset + 4] = (100).to_bytes(4, "big")
        report = verify_output(self._write("bad_co.mp4", bytes(data)))
        self.assertFalse(report.ok)
        self.assertTrue(any(c.name == "chunk_offsets_in_mdat" for c in report.failures))

    def test_missing_ftyp_fails_top_level_check(self) -> None:
        # Drop the ftyp box (32 bytes) and fix nothing else: sizes now disagree.
        data = self.good[32:]
        report = verify_output(self._write("noftyp.mp4", data))
        self.assertFalse(report.ok)

    def test_stsz_count_mismatch_fails(self) -> None:
        data = bytearray(self.good)
        stsz = data.find(b"stsz")
        # sample_count lives at payload +4 (version/flags) +4 (sample_size)
        count_at = stsz + 4 + 8
        data[count_at : count_at + 4] = (999).to_bytes(4, "big")
        report = verify_output(self._write("bad_stsz.mp4", bytes(data)))
        self.assertFalse(report.ok)
        self.assertTrue(any("track[1]" == c.name for c in report.failures))

    def test_stts_covering_too_few_samples_fails(self) -> None:
        data = bytearray(self.good)
        stts = data.find(b"stts")
        count_at = stts + 4 + 4
        data[count_at : count_at + 4] = (1).to_bytes(4, "big")
        # Widen the single run so the file still parses but under-covers.
        run_at = stts + 4 + 8
        data[run_at : run_at + 4] = (1).to_bytes(4, "big")
        report = verify_output(self._write("bad_stts.mp4", bytes(data)))
        self.assertFalse(report.ok)

    def test_stss_out_of_range_fails(self) -> None:
        data = bytearray(self.good)
        stss = data.find(b"stss")
        if stss < 0:
            self.skipTest("fixture has no sync sample table")
        first = stss + 4 + 8
        data[first : first + 4] = (500).to_bytes(4, "big")
        report = verify_output(self._write("bad_stss.mp4", bytes(data)))
        self.assertFalse(report.ok)

    def test_source_byte_comparison(self) -> None:
        doc = document_from_spec(demo_spec(), path=self.work / "in.mkv")
        report = verify_output(self.result.output_path, document=doc)
        names = {check.name for check in report.checks}
        self.assertIn("source_byte_coverage", names)
        self.assertTrue(report.ok)

    def test_missing_file_is_reported(self) -> None:
        report = verify_output(self.work / "ghost.mp4")
        self.assertFalse(report.ok)
        self.assertEqual(report.size, 0)

    def test_non_mp4_bytes_fail(self) -> None:
        path = self._write("not.mp4", b"this is not an mp4 file at all")
        report = verify_output(path)
        self.assertFalse(report.ok)


if __name__ == "__main__":
    unittest.main()
