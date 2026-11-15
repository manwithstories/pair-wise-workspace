"""End-to-end tests: fixtures -> probe -> remux (both layouts) -> verify."""
import importlib.util
import json
import shutil
import sys
import tempfile
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(REPO_ROOT))

from mkv2mp4 import cli  # noqa: E402

_spec = importlib.util.spec_from_file_location(
    "gen_fixtures", REPO_ROOT / "tools" / "gen_fixtures.py")
gen_fixtures = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(gen_fixtures)

CASES = ["unknown_size", "cues_missing", "b_frame", "lacing"]


class RemuxTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = Path(tempfile.mkdtemp(prefix="mkv2mp4-test-"))
        cls.fixtures = cls.tmp / "fixtures"
        cls.out = cls.tmp / "out"
        cls.out.mkdir()
        gen_fixtures.generate(cls.fixtures, seed=7, cases=CASES)

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.tmp, ignore_errors=True)

    def run_cli(self, argv):
        return cli.main(argv)

    def remux(self, case, layout="faststart"):
        out_mp4 = self.out / ("%s.%s.mp4" % (case, layout))
        out_json = self.out / ("%s.%s.json" % (case, layout))
        rc = self.run_cli([
            "remux", str(self.fixtures / ("%s.mkv" % case)), str(out_mp4),
            "--layout", layout, "--timescale", "90000",
            "--json", str(out_json)])
        self.assertEqual(rc, 0, "remux %s failed" % case)
        return out_mp4, json.loads(out_json.read_text())

    def test_probe_unknown_size(self):
        probe_json = self.out / "probe.json"
        rc = self.run_cli(["probe", str(self.fixtures / "unknown_size.mkv"),
                           "--json", str(probe_json)])
        self.assertEqual(rc, 0)
        report = json.loads(probe_json.read_text())
        self.assertEqual(report["element_count"], 120000)
        self.assertGreaterEqual(report["max_depth"], 6)
        self.assertEqual(len(report["unknown_sized"]), 3)
        kinds = {item["name"] for item in report["unknown_sized"]}
        self.assertEqual(kinds, {"Segment", "Cluster"})
        self.assertEqual(len(report["tracks"]), 2)
        video = report["tracks"][0]
        self.assertEqual((video["width"], video["height"]), (1920, 1080))
        audio = report["tracks"][1]
        self.assertEqual((audio["sample_rate"], audio["channels"]),
                         (48000.0, 2))

    def test_remux_both_layouts_match(self):
        _, fast = self.remux("unknown_size", "faststart")
        _, tail = self.remux("unknown_size", "moov-last")
        self.assertEqual(len(fast["tracks"]), 2)
        counts = {t["type"]: t["sample_count"] for t in fast["tracks"]}
        self.assertEqual(counts, {"video": 1800, "audio": 2813})
        for t in fast["tracks"]:
            self.assertEqual(t["first_pts_ms"], 0.0)
            self.assertLessEqual(
                abs(t["last_pts_ms"] - t["source_last_pts_ms"]), 1.0)
        self.assertEqual(fast["sample_table_sha256"],
                         tail["sample_table_sha256"])

    def test_verify_acceptance_flow(self):
        mp4, report = self.remux("unknown_size", "faststart")
        expect = self.out / "unknown_size.faststart.json"
        rc = self.run_cli(["verify", str(mp4), "--expect", str(expect)])
        self.assertEqual(rc, 0)

    def test_all_cases_remux_and_verify(self):
        for case in CASES:
            with self.subTest(case=case):
                mp4, report = self.remux(case)
                counts = {t["type"]: t["sample_count"]
                          for t in report["tracks"]}
                self.assertEqual(counts, {"video": 1800, "audio": 2813})
                expect = self.out / ("%s.faststart.json" % case)
                rc = self.run_cli(["verify", str(mp4), "--expect",
                                   str(expect)])
                self.assertEqual(rc, 0)

    def test_b_frame_uses_edit_list(self):
        _, report = self.remux("b_frame")
        video = next(t for t in report["tracks"] if t["type"] == "video")
        self.assertTrue(video["has_b_frames"])
        self.assertTrue(video["edit_list"])
        self.assertEqual(video["first_pts_ms"], 0.0)

    def test_cues_missing_still_remuxes(self):
        _, report = self.remux("cues_missing")
        self.assertEqual(report["cues"]["status"], "missing")
        self.assertTrue(report["cues"]["rebuilt"])

    def test_exit_code_argument_error(self):
        with self.assertRaises(SystemExit) as ctx:
            self.run_cli(["remux"])
        self.assertEqual(ctx.exception.code, 2)

    def test_exit_code_ebml_corrupt(self):
        src = self.fixtures / "unknown_size.mkv"
        broken = self.tmp / "broken.mkv"
        data = src.read_bytes()
        broken.write_bytes(data[: len(data) // 2 + 12345])
        rc = self.run_cli(["probe", str(broken)])
        self.assertEqual(rc, 3)

    def test_exit_code_unsupported_codec(self):
        src = self.fixtures / "unknown_size.mkv"
        alien = self.tmp / "alien.mkv"
        alien.write_bytes(src.read_bytes().replace(
            b"V_MPEG4/ISO/AVC", b"V_MPEG4/ISO/HEV"))
        rc = self.run_cli(["remux", str(alien), str(self.tmp / "alien.mp4")])
        self.assertEqual(rc, 4)

    def test_exit_code_bad_timescale(self):
        rc = self.run_cli([
            "remux", str(self.fixtures / "unknown_size.mkv"),
            str(self.tmp / "bad.mp4"), "--timescale", "0"])
        self.assertEqual(rc, 5)

    def test_verify_detects_mismatch(self):
        mp4, report = self.remux("unknown_size", "faststart")
        report["tracks"][0]["sample_count"] += 1
        expect = self.tmp / "doctored.json"
        expect.write_text(json.dumps(report))
        rc = self.run_cli(["verify", str(mp4), "--expect", str(expect)])
        self.assertEqual(rc, 6)


if __name__ == "__main__":
    unittest.main()
