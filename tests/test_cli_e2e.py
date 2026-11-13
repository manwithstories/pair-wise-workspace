import io
import json
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path

from tlrender.cli import main
from tlrender.fixtures_gen import generate_fixtures


class CliEndToEndTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.fixtures = self.root / "fixtures"
        self.out = self.root / "out"
        generate_fixtures(self.fixtures)

    def tearDown(self):
        self.tmp.cleanup()

    def _run(self, *args):
        buf = io.StringIO()
        with redirect_stdout(buf):
            code = main(list(args))
        return code, buf.getvalue()

    def test_full_pipeline_matches_expected(self):
        expected = json.loads((self.fixtures / "expected.json").read_text())
        code, output = self._run(
            "run",
            "--timeline", str(self.fixtures / "timeline_v2.tl"),
            "--legacy", str(self.fixtures / "timeline_v1.tl"),
            "--out", str(self.out),
            "--json", "--no-timestamp",
        )
        self.assertEqual(code, 0)
        lines = [ln for ln in output.splitlines() if ln.startswith("{")]
        payloads = [json.loads(ln) for ln in lines]
        self.assertEqual(len(payloads), expected["summary"]["total"])
        summary_line = output.splitlines()[-1]
        self.assertIn("total 42, resolved 17, dropped 1, frames 42, hash_mismatch 0",
                      summary_line)
        manifest = json.loads((self.out / "manifest.json").read_text())
        self.assertEqual(manifest["summary"], {
            **expected["summary"],
            "warnings": 0,
            "inputs": manifest["summary"]["inputs"],
        })
        pngs = list(self.out.glob("*.png"))
        self.assertEqual(len(pngs), 42)
        for row in manifest["cues"]:
            self.assertEqual(len(row["sha256"]), 16)

    def test_transition_expectations(self):
        expected = json.loads((self.fixtures / "expected.json").read_text())
        self._run(
            "run",
            "--timeline", str(self.fixtures / "timeline_v2.tl"),
            "--legacy", str(self.fixtures / "timeline_v1.tl"),
            "--out", str(self.out), "--no-timestamp",
        )
        manifest = json.loads((self.out / "manifest.json").read_text())
        rows = {r["cue"]: r for r in manifest["cues"]}
        for cid, want in expected["transition_expect"].items():
            self.assertEqual(rows[cid]["final"], want["final"], cid)
            self.assertEqual(rows[cid]["moved"], want["moved"], cid)
            self.assertEqual(rows[cid]["resolved"], want["resolved"], cid)
        drop = rows[expected["drop_expect"]["cue"]]
        self.assertTrue(drop["dropped"])
        self.assertEqual(drop["final"][1] - drop["final"][0],
                         expected["drop_expect"]["final_end_minus_start"])

    def test_repeat_run_byte_identical_and_no_mismatch(self):
        args = [
            "run", "--timeline", str(self.fixtures / "timeline_v2.tl"),
            "--legacy", str(self.fixtures / "timeline_v1.tl"),
            "--out", str(self.out), "--no-timestamp",
        ]
        self._run(*args)
        first = (self.out / "manifest.json").read_bytes()
        first_png = (self.out / "s01a.png").read_bytes()
        code, output = self._run(*args)
        self.assertEqual(code, 0)
        self.assertEqual((self.out / "manifest.json").read_bytes(), first)
        self.assertEqual((self.out / "s01a.png").read_bytes(), first_png)
        self.assertIn("hash_mismatch 0", output.splitlines()[-1])

    def test_missing_file_exit_1(self):
        code, _ = self._run(
            "run", "--timeline", str(self.fixtures / "missing.tl"),
            "--out", str(self.out),
        )
        self.assertEqual(code, 1)

    def test_malformed_file_exit_1(self):
        bad = self.fixtures / "bad.tl"
        bad.write_text("@timeline v2\ntrack 1\ncue oops\n", encoding="utf-8")
        code, _ = self._run(
            "run", "--timeline", str(bad), "--out", str(self.out),
        )
        self.assertEqual(code, 1)

    def test_too_long_cue_exit_2(self):
        long_ms = 10 * 60 * 1000 + 10
        long_file = self.fixtures / "long.tl"
        long_file.write_text(
            f"@timeline v1\nshot s 0 {long_ms + 100}\n"
            f"cue z 0 {long_ms} \"long\"\n",
            encoding="utf-8",
        )
        code, _ = self._run(
            "run", "--timeline", str(long_file),
            "--out", str(self.out), "--no-timestamp",
        )
        self.assertEqual(code, 2)
        self.assertTrue((self.out / "manifest.json").exists())

    def test_non_utf8_exit_1(self):
        bad = self.fixtures / "enc.tl"
        bad.write_bytes(b"@timeline v2\n\xff\xff")
        code, _ = self._run(
            "run", "--timeline", str(bad), "--out", str(self.out),
        )
        self.assertEqual(code, 1)


if __name__ == "__main__":
    unittest.main()
