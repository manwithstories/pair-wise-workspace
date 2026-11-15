from __future__ import annotations

from contextlib import redirect_stdout
import io
import json
import sqlite3
import tempfile
import unittest
from pathlib import Path

import numpy as np

from audiosplit.cli import main
from audiosplit.fixtures import generate_fixtures
from tests.helpers import write_tmp_wav


def run_cli(*args: str) -> tuple[int, dict]:
    output = io.StringIO()
    with redirect_stdout(output):
        code = main(list(args))
    return code, json.loads(output.getvalue())


class EndToEndTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.fixture_dir = tempfile.TemporaryDirectory()
        cls.fixtures = Path(cls.fixture_dir.name)
        generate_fixtures(cls.fixtures, seed=7, files=6)

    @classmethod
    def tearDownClass(cls) -> None:
        cls.fixture_dir.cleanup()

    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.db = self.root / "index.sqlite"

    def tearDown(self) -> None:
        self.temp.cleanup()

    def test_scan_is_idempotent(self) -> None:
        source = str(self.fixtures / "take_a.wav")
        first_code, first = run_cli("scan", source, "--db", str(self.db), "--json")
        second_code, second = run_cli("scan", source, "--db", str(self.db), "--json")
        self.assertEqual((first_code, second_code), (0, 0))
        self.assertEqual(first["file_id"], second["file_id"])
        self.assertEqual(second["segment_count"], 5)
        with sqlite3.connect(self.db) as connection:
            self.assertEqual(connection.execute("SELECT count(*) FROM files").fetchone()[0], 1)
            self.assertEqual(
                connection.execute("SELECT count(*) FROM segments").fetchone()[0],
                second["segment_count"],
            )
            self.assertEqual(connection.execute("SELECT count(*) FROM batches").fetchone()[0], 2)
            self.assertEqual(connection.execute("PRAGMA user_version").fetchone()[0], 1)
            version = connection.execute(
                "SELECT value FROM meta WHERE key='schema_version'"
            ).fetchone()[0]
            self.assertEqual(version, "1")

    def test_export_manifest_report_and_rerun(self) -> None:
        out = self.root / "segments"
        manifest = self.root / "manifest.json"
        args = (
            "export", "--dir", str(self.fixtures), "--db", str(self.db),
            "--out", str(out), "--manifest", str(manifest),
        )
        code, summary = run_cli(*args)
        self.assertEqual(code, 0)
        payload = json.loads(manifest.read_text())
        self.assertEqual(summary["segment_count"], 32)
        self.assertEqual(payload["file_count"], 6)
        self.assertEqual(payload["segment_count"], 32)
        self.assertEqual(
            sum(len(item["segments"]) for item in payload["files"]),
            len(list(out.glob("*.wav"))),
        )
        self.assertEqual(len(list(out.glob("*.wav"))), 32)
        report_code, report = run_cli("report", "--db", str(self.db), "--json")
        self.assertEqual(report_code, 0)
        self.assertEqual(report["file_count"], 6)
        self.assertEqual(report["total_segments"], 32)
        self.assertGreaterEqual(report["total_silence_ms"], 9000)
        self.assertLessEqual(report["total_silence_ms"], 13000)
        rerun_code, _ = run_cli(*args)
        self.assertEqual(rerun_code, 0)
        self.assertEqual(len(list(out.glob("*.wav"))), 32)
        with sqlite3.connect(self.db) as connection:
            self.assertEqual(connection.execute("SELECT count(*) FROM files").fetchone()[0], 6)
            self.assertEqual(
                connection.execute("SELECT count(*) FROM exported_segments").fetchone()[0], 32
            )

    def test_selftest_passes(self) -> None:
        code, payload = run_cli(
            "selftest", "--fixtures", str(self.fixtures), "--tol-samples", "48"
        )
        self.assertEqual(code, 0)
        self.assertEqual(payload["status"], "PASS")
        self.assertLessEqual(payload["max_boundary_error_samples"], 48)
        strict_code, strict = run_cli(
            "selftest", "--fixtures", str(self.fixtures), "--tol-samples", "0"
        )
        self.assertEqual(strict_code, 4)
        self.assertEqual(strict["status"], "FAIL")

    def test_schema_migration_and_future_version_guard(self) -> None:
        legacy = self.root / "legacy.sqlite"
        sqlite3.connect(legacy).close()
        source = str(self.fixtures / "take_a.wav")
        code, payload = run_cli("scan", source, "--db", str(legacy))
        self.assertEqual(code, 0)
        with sqlite3.connect(legacy) as connection:
            self.assertEqual(connection.execute("PRAGMA user_version").fetchone()[0], 1)
        with sqlite3.connect(legacy) as connection:
            connection.execute("PRAGMA user_version = 99")
        code, payload = run_cli("report", "--db", str(legacy))
        self.assertEqual((code, payload["error"]["code"]), (5, "schema_too_new"))

    def test_error_exit_codes(self) -> None:
        source = str(self.fixtures / "take_a.wav")
        code, payload = run_cli(
            "scan", source, "--db", str(self.db), "--enter-db", "3", "--exit-db", "6"
        )
        self.assertEqual((code, payload["error"]["code"]), (2, "invalid_parameters"))
        code, payload = run_cli("scan", str(self.root / "missing.wav"), "--db", str(self.db))
        self.assertEqual((code, payload["error"]["code"]), (3, "filesystem_error"))
        code, payload = run_cli("report", "--db", str(self.root / "missing.sqlite"))
        self.assertEqual((code, payload["error"]["code"]), (5, "database_unavailable"))
        stereo = write_tmp_wav(
            self.root / "stereo.wav", np.zeros((4800, 2), dtype=np.float32), bits=16
        )
        code, payload = run_cli("scan", str(stereo), "--db", str(self.db))
        self.assertEqual((code, payload["error"]["code"]), (3, "stereo_not_supported"))
        single = write_tmp_wav(
            self.root / "single.wav", np.array([0.0], dtype=np.float32), bits=16
        )
        code, payload = run_cli("scan", str(single), "--db", str(self.db))
        self.assertEqual((code, payload["error"]["code"]), (3, "audio_too_short"))


if __name__ == "__main__":
    unittest.main()

