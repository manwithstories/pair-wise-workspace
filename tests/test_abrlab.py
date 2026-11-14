from __future__ import annotations

import contextlib
import io
import json
import tempfile
import unittest
from pathlib import Path

import numpy as np

import abrlab.scheduler as scheduler_module
from abrlab.cli import main
from abrlab.codec import decode, encode
from abrlab.metrics import evaluate
from abrlab.plan import PlanError, generate_demo, make_entry, normalize_entries
from abrlab.scheduler import Scheduler
from abrlab.source import generate_reference, generate_sequence
from abrlab.store import Store


def run_cli(arguments: list[str]) -> tuple[int, str, str]:
    stdout = io.StringIO()
    stderr = io.StringIO()
    with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
        code = main(arguments)
    return code, stdout.getvalue(), stderr.getvalue()


class PlanTests(unittest.TestCase):
    def test_demo_deduplicates_by_task_key(self) -> None:
        plan = generate_demo(3)
        self.assertEqual(plan["stats"], {"entries": 51, "unique": 48, "duplicates_removed": 3})
        self.assertEqual(len({entry["task_key"] for entry in plan["entries"]}), 48)

    def test_same_name_with_different_parameters_is_rejected(self) -> None:
        source = {
            "seed": 1, "frames": 1, "fps": 24.0, "width": 16, "height": 16,
            "base_width": 16, "base_height": 16, "pattern": "mixed",
        }
        first = make_entry(source, {"qp": 20, "block_size": 4, "chroma": "420"}, "same")
        second = make_entry(source, {"qp": 28, "block_size": 4, "chroma": "420"}, "same")
        with self.assertRaises(PlanError):
            normalize_entries([first, second])

    def test_task_key_is_stable_across_duplicate_names(self) -> None:
        plan = generate_demo(0)
        entry = plan["entries"][0]
        duplicate = json.loads(json.dumps(entry))
        duplicate["name"] = "renamed-copy"
        normalized, duplicates = normalize_entries([entry, duplicate])
        self.assertEqual(duplicates, 1)
        self.assertEqual(normalized[0]["task_key"], entry["task_key"])


class CodecMetricTests(unittest.TestCase):
    def test_all_modes_round_trip_deterministically(self) -> None:
        source = {
            "seed": 10291, "frames": 1, "fps": 24.0, "width": 19, "height": 11,
            "base_width": 19, "base_height": 11, "pattern": "mixed",
        }
        for mode in ("420", "444", "gray"):
            frames = generate_sequence(source, mode)
            first = encode(frames, 24, 4, mode)
            second = encode(frames, 24, 4, mode)
            self.assertEqual(first, second)
            self.assertEqual(decode(first).shape, frames.shape)
            self.assertGreater(evaluate(frames, decode(first))["ssim"], 0.7)

    def test_constant_tiny_and_grayscale_are_stable(self) -> None:
        for pattern, mode in (("constant", "420"), ("mixed", "gray")):
            source = {
                "seed": 9, "frames": 1, "fps": 24.0, "width": 1, "height": 1,
                "base_width": 1, "base_height": 1, "pattern": pattern,
            }
            frames = generate_sequence(source, mode)
            payload = encode(frames, 20, 4, mode)
            metrics = evaluate(generate_reference(source, mode), decode(payload))
            self.assertTrue(np.isfinite(metrics["psnr_db"]))
            self.assertTrue(0.0 <= metrics["ssim"] <= 1.0)
            self.assertEqual(metrics["ssim"], 1.0)


class StoreSchedulerTests(unittest.TestCase):
    def test_stale_running_is_recovered_and_temp_is_cleaned(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            db = Path(directory) / "scan.db"
            entry = generate_demo(0)["entries"][0]
            with Store(db) as store:
                store.add_plan_entries([entry])
                self.assertEqual(store.claim_task(entry["task_key"]), 1)
                temp_dir = db.parent / f".{db.name}.work"
                temp_dir.mkdir()
                (temp_dir / f"{entry['task_key']}.part").write_bytes(b"partial")
                self.assertEqual(store.recover_stale(), 1)
                self.assertEqual(store.status_counts()["pending"], 1)
                stats = Scheduler(store).run()
                self.assertEqual(stats.done, 1)
                self.assertEqual(stats.cleaned_temps, 1)
                self.assertEqual(list(temp_dir.glob("*.part")), [])

    def test_failure_is_isolated_and_retry_limit_is_recorded(self) -> None:
        entries = generate_demo(0)["entries"][:2]
        original = scheduler_module.encode
        with tempfile.TemporaryDirectory() as directory, Store(Path(directory) / "scan.db") as store:
            store.add_plan_entries(entries)

            def fail(*args, **kwargs):
                raise RuntimeError("forced failure")

            scheduler_module.encode = fail
            try:
                stats = Scheduler(store).run()
            finally:
                scheduler_module.encode = original
            self.assertEqual(stats.failed, 2)
            self.assertEqual(stats.attempts, 6)
            row = store.conn.execute(
                "SELECT attempts,error_code FROM tasks WHERE status='failed' LIMIT 1"
            ).fetchone()
            self.assertEqual(row["attempts"], 3)
            self.assertEqual(row["error_code"], "E_RUNTIMEERROR")


class EndToEndTests(unittest.TestCase):
    def test_acceptance_flow_and_tamper_detection(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            plan = root / "plan.json"
            db = root / "scan.db"
            ladder = root / "ladder.json"
            code, stdout, _ = run_cli(["plan", "--demo", "--inject-duplicates", "3", "--out", str(plan)])
            self.assertEqual(code, 0)
            self.assertEqual(json.loads(stdout), {"entries": 51, "unique": 48})
            code, stdout, _ = run_cli([
                "scan", "--plan", str(plan), "--db", str(db), "--stop-after", "4", "--json",
            ])
            self.assertEqual(code, 4)
            self.assertEqual(json.loads(stdout)["done"], 4)
            code, stdout, _ = run_cli(["scan", "--resume", "--db", str(db), "--json"])
            self.assertEqual(code, 0)
            resumed = json.loads(stdout)
            self.assertEqual(resumed["skipped"], 4)
            self.assertEqual(resumed["done"], 48)
            code, stdout, _ = run_cli([
                "ladder", "--db", str(db), "--out", str(ladder), "--max-rungs", "4",
                "--max-bitrate", "1800", "--min-quality", "0.86", "--json",
            ])
            self.assertEqual(code, 0)
            value = json.loads(stdout)
            self.assertTrue(value["monotone"])
            self.assertGreaterEqual(len(value["levels"]), 1)
            self.assertLessEqual(len(value["levels"]), 4)
            code, stdout, _ = run_cli(["verify", "--db", str(db), "--ladder", str(ladder)])
            self.assertEqual(code, 0)
            verify = json.loads(stdout)
            self.assertTrue(verify["pass"])
            self.assertEqual(verify["violations"], [])

            tampered = json.loads(ladder.read_text(encoding="utf-8"))
            tampered["levels"][1]["bitrate_kbps"] = tampered["levels"][0]["bitrate_kbps"]
            ladder.write_text(json.dumps(tampered), encoding="utf-8")
            code, stdout, _ = run_cli(["verify", "--db", str(db), "--ladder", str(ladder)])
            self.assertEqual(code, 3)
            rejected = json.loads(stdout)
            self.assertFalse(rejected["monotone"])
            self.assertTrue(rejected["violations"])


if __name__ == "__main__":
    unittest.main()
