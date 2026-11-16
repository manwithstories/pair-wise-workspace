"""Error-path tests: format rejection, batch isolation, export and verify."""

from __future__ import annotations

import contextlib
import csv
import io
import os
import sqlite3
import unittest

import numpy as np

from helpers import (
    TempWorkspace,
    generate_fixtures,
    tone,
    write_float_wav,
    write_truncated_wav,
    write_wav,
)

from audio_segmenter import cli
from audio_segmenter.exporter import ExportError, export_all, read_jsonl
from audio_segmenter.model import DetectionConfig
from audio_segmenter.pipeline import discover_wavs, execute_batch, run_batch
from audio_segmenter.store import Store
from audio_segmenter.wavio import WavError, file_sha256, read_wav

SR = 48000


class WavValidationTests(TempWorkspace):
    def _expect(self, path: str, code: str) -> WavError:
        with self.assertRaises(WavError) as ctx:
            read_wav(path)
        self.assertEqual(ctx.exception.code, code)
        return ctx.exception

    def test_rejects_missing_file(self) -> None:
        self._expect(self.path("absent.wav"), "wav_not_found")

    def test_rejects_empty_file(self) -> None:
        path = self.path("empty.wav")
        open(path, "wb").close()
        self._expect(path, "wav_empty")

    def test_rejects_float_wav(self) -> None:
        self._expect(write_float_wav(self.path("f.wav")), "wav_unsupported_float")

    def test_rejects_truncated_data(self) -> None:
        self._expect(write_truncated_wav(self.path("t.wav")), "wav_corrupt")

    def test_rejects_non_riff_bytes(self) -> None:
        path = self.path("g.wav")
        with open(path, "wb") as fh:
            fh.write(b"<html>not audio</html>")
        self._expect(path, "wav_corrupt")

    def test_rejects_unsupported_bit_depth(self) -> None:
        import wave

        path = self.path("b8.wav")
        with wave.open(path, "wb") as wf:
            wf.setnchannels(1)
            wf.setsampwidth(1)
            wf.setframerate(SR)
            wf.writeframes(b"\x80" * 100)
        self._expect(path, "wav_unsupported_bit_depth")

    def test_rejects_three_channels(self) -> None:
        import wave

        path = self.path("c3.wav")
        with wave.open(path, "wb") as wf:
            wf.setnchannels(3)
            wf.setsampwidth(2)
            wf.setframerate(SR)
            wf.writeframes(b"\x00" * 300)
        self._expect(path, "wav_unsupported_channels")

    def test_accepts_pcm24_stereo(self) -> None:
        path = self.path("s24.wav")
        data = np.stack([tone(SR, 0.1), tone(SR, 0.1) * 0.5], axis=1)
        write_wav(path, data, 44100, 3)
        decoded = read_wav(path)
        self.assertEqual(decoded.info.channels, 2)
        self.assertEqual(decoded.info.sample_width, 3)
        self.assertEqual(decoded.info.sample_rate, 44100)

    def test_digest_is_stable_and_content_addressed(self) -> None:
        a = self.path("a.wav")
        b = self.path("b.wav")
        write_wav(a, tone(SR, 0.05), SR, 2)
        write_wav(b, tone(SR, 0.05), SR, 2)
        self.assertEqual(file_sha256(a), file_sha256(b))


class BatchIsolationTests(TempWorkspace):
    def setUp(self) -> None:
        super().setUp()
        generate_fixtures(self.root)
        write_float_wav(self.path("bad_float.wav"))
        write_truncated_wav(self.path("bad_trunc.wav"))

    def test_one_bad_file_does_not_pollute_others(self) -> None:
        paths = discover_wavs(self.root)
        self.assertEqual(len(paths), 5)
        analyses = run_batch(paths, DetectionConfig())
        ok = [a for a in analyses if a.status == "ok"]
        bad = [a for a in analyses if a.status != "ok"]
        self.assertEqual(len(ok), 3)
        self.assertEqual(len(bad), 2)
        self.assertGreater(sum(len(a.result.segments) for a in ok), 0)
        for analysis in bad:
            self.assertIsNone(analysis.result)
            self.assertTrue(analysis.error_code)

    def test_persist_is_idempotent_by_hash(self) -> None:
        db_path = self.path("b.sqlite")
        with Store(db_path) as store:
            report = execute_batch(self.root, DetectionConfig(), store=store)
            self.assertIsNotNone(report.batch_id)
            first = store.stats()
            execute_batch(self.root, DetectionConfig(), store=store)
            second = store.stats()
        self.assertEqual(first, second, "re-running the same files must not duplicate rows")

    def test_failed_batch_rolls_back(self) -> None:
        db_path = self.path("rb.sqlite")
        with Store(db_path) as store:
            paths = discover_wavs(self.root)
            analyses = run_batch(paths, DetectionConfig())
            original = store.save_analysis_within
            calls = {"n": 0}

            def flaky(conn, analysis):
                calls["n"] += 1
                if calls["n"] == 3:
                    raise RuntimeError("simulated write failure")
                return original(conn, analysis)

            store.save_analysis_within = flaky  # type: ignore[assignment]
            with self.assertRaises(RuntimeError):
                from audio_segmenter.pipeline import persist_batch

                persist_batch(store, analyses, DetectionConfig(), self.root)
            store.save_analysis_within = original  # type: ignore[assignment]
            self.assertEqual(store.stats()["files"], 0, "partial writes must be rolled back")
            batch = store.batch_summary(1)
            self.assertEqual(batch["status"], "failed")

    def test_segment_rows_are_replaced_not_appended(self) -> None:
        db_path = self.path("rep.sqlite")
        with Store(db_path) as store:
            report = execute_batch(self.root, DetectionConfig(), store=store)
            ok = report.ok[0]
            file_row = store.get_file_by_hash(ok.sha256)
            before = len(store.list_segments(file_row["id"]))
            store.save_analysis(ok, report.batch_id)
            after = len(store.list_segments(file_row["id"]))
        self.assertEqual(before, after)


class ExportTests(TempWorkspace):
    def setUp(self) -> None:
        super().setUp()
        generate_fixtures(self.root)
        self.analyses = run_batch(discover_wavs(self.root), DetectionConfig())

    def test_jsonl_has_segment_and_silence_records(self) -> None:
        path = self.path("out.jsonl")
        export_all(self.analyses, jsonl_path=path)
        records = list(read_jsonl(path))
        kinds = {r["kind"] for r in records}
        self.assertEqual(kinds, {"segment", "silence"})
        segments = [r for r in records if r["kind"] == "segment"]
        self.assertGreaterEqual(len(segments), 6)
        for record in segments:
            self.assertEqual(record["sample_count"], record["end_sample"] - record["start_sample"])

    def test_csv_written_with_both_sections(self) -> None:
        path = self.path("out.csv")
        export_all(self.analyses, csv_path=path)
        with open(path, newline="", encoding="utf-8") as fh:
            rows = list(csv.reader(fh))
        self.assertIn("start_sample", rows[0])
        self.assertGreater(len(rows), 3)

    def test_sqlite_export_is_queryable(self) -> None:
        path = self.path("out.sqlite")
        export_all(self.analyses, sqlite_path=path)
        conn = sqlite3.connect(path)
        try:
            seg_count = conn.execute("SELECT COUNT(*) FROM segments").fetchone()[0]
            file_count = conn.execute("SELECT COUNT(*) FROM files").fetchone()[0]
        finally:
            conn.close()
        self.assertEqual(file_count, 3)
        self.assertGreaterEqual(seg_count, 6)

    def test_sqlite_export_refuses_silent_overwrite(self) -> None:
        path = self.path("keep.sqlite")
        export_all(self.analyses, sqlite_path=path)
        before = os.path.getmtime(path)
        with self.assertRaises(ExportError) as ctx:
            export_all(self.analyses, sqlite_path=path)
        self.assertEqual(ctx.exception.code, "export_exists")
        self.assertEqual(os.path.getmtime(path), before)
        export_all(self.analyses, sqlite_path=path, overwrite=True)

    def test_export_to_missing_directory_fails_cleanly(self) -> None:
        with self.assertRaises(ExportError) as ctx:
            export_all(self.analyses, jsonl_path=self.path("nope/out.jsonl"))
        self.assertEqual(ctx.exception.code, "export_dir_missing")

    def test_export_uses_same_results_as_detector(self) -> None:
        path = self.path("same.jsonl")
        export_all(self.analyses, jsonl_path=path)
        exported = {(r["path"], r["start_sample"]) for r in read_jsonl(path) if r["kind"] == "segment"}
        expected = {
            (a.path, s.start_sample) for a in self.analyses if a.result for s in a.result.segments
        }
        self.assertTrue(expected.issubset(exported))


class CliExitCodeTests(TempWorkspace):
    def setUp(self) -> None:
        super().setUp()
        self._out = contextlib.redirect_stdout(io.StringIO())
        self._err = contextlib.redirect_stderr(io.StringIO())
        self.extra = []
        self.extra.append(self._out.__enter__())
        self.extra.append(self._err.__enter__())

    def tearDown(self) -> None:
        for ctx in reversed(self.extra):
            ctx.__exit__(None, None, None)
        super().tearDown()

    def test_generate_then_batch_then_verify(self) -> None:
        data_dir = self.path("demo_data")
        self.assertEqual(cli.main(["generate", "--out-dir", data_dir]), 0)
        db = self.path("demo.sqlite")
        jsonl = self.path("demo.jsonl")
        code = cli.main(
            [
                "batch",
                "--input",
                data_dir,
                "--db",
                db,
                "--jsonl",
                jsonl,
                "--sample-rate",
                "48000",
                "--threshold-db",
                "-42",
                "--min-silence-ms",
                "120",
            ]
        )
        self.assertEqual(code, 0)
        self.assertTrue(os.path.exists(jsonl))
        self.assertEqual(cli.main(["verify", "--db", db]), 0)
        self.assertEqual(cli.main(["verify", "--input", data_dir]), 0)

    def test_missing_input_exits_2(self) -> None:
        self.assertEqual(cli.main(["detect", "--input", self.path("absent")]), 2)

    def test_only_invalid_files_exits_2(self) -> None:
        bad_dir = self.path("bad")
        os.makedirs(bad_dir)
        write_float_wav(os.path.join(bad_dir, "f.wav"))
        self.assertEqual(cli.main(["detect", "--input", bad_dir]), 2)

    def test_invalid_threshold_configuration_exits_2(self) -> None:
        data_dir = self.path("cfg")
        os.makedirs(data_dir)
        write_wav(os.path.join(data_dir, "a.wav"), tone(SR, 0.2), SR, 2)
        self.assertEqual(
            cli.main(
                ["detect", "--input", data_dir, "--hop-ms", "40", "--frame-ms", "10"]
            ),
            2,
        )

    def test_database_in_unwritable_place_exits_3(self) -> None:
        data_dir = self.path("db")
        os.makedirs(data_dir)
        write_wav(os.path.join(data_dir, "a.wav"), tone(SR, 0.2), SR, 2)
        code = cli.main(
            ["batch", "--input", data_dir, "--db", self.path("absent-dir/x.sqlite")]
        )
        self.assertEqual(code, 3)


if __name__ == "__main__":
    unittest.main()
