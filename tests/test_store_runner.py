import tempfile
import unittest
from pathlib import Path

from support import ROOT  # noqa: F401
from abrl.scheduler.grid import (
    ENCODER_VERSION,
    GridConflictError,
    TaskSpec,
    expand,
    make_key,
)
from abrl.store import db


def _spec(key: str = "k", qp: int = 20) -> TaskSpec:
    return TaskSpec(key, "src.yuv", "d" * 64, 64, 32, 30, qp, 16, ENCODER_VERSION)


class TaskKeyTests(unittest.TestCase):
    def test_key_deterministic_and_distinct(self):
        common = ("a" * 64, 640, 360, 30, 20, 30)
        first = make_key(*common)
        self.assertEqual(first, make_key(*common))
        self.assertNotEqual(first, make_key("b" * 64, 640, 360, 30, 20, 30))
        self.assertNotEqual(first, make_key("a" * 64, 512, 288, 30, 20, 30))
        self.assertNotEqual(first, make_key("a" * 64, 640, 360, 24, 20, 30))

    def test_grid_dedup_counts_duplicates(self):
        grid = {
            "resolutions": ["64x32", "64x32"],
            "fps": [30],
            "qp": [12],
            "gop": [16],
        }
        tasks, duplicates = expand(grid, "src.yuv", "d" * 64)
        self.assertEqual(len(tasks), 1)
        self.assertEqual(duplicates, 1)

    def test_conflict_on_same_key_different_params(self):
        conn = db.connect(":memory:")
        db.initialize(conn)
        db.upsert_pending(conn, [_spec(qp=20)])
        conflicting = TaskSpec("k", "src.yuv", "d" * 64, 64, 32, 30, 21, 16, ENCODER_VERSION)
        with self.assertRaises(GridConflictError):
            from abrl.scheduler.grid import detect_conflicts
            detect_conflicts(db.existing_params(conn), [conflicting])
        conn.close()


class StoreTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.path = Path(self.tmp.name) / "s.db"
        self.conn = db.connect(self.path)
        db.initialize(self.conn)

    def tearDown(self):
        self.conn.close()
        self.tmp.cleanup()

    def test_wal_mode_and_schema(self):
        mode = self.conn.execute("PRAGMA journal_mode").fetchone()[0]
        self.assertEqual(mode.lower(), "wal")
        db.validate_schema(self.conn)

    def test_atomic_claim_and_orphan_reclaim(self):
        db.upsert_pending(self.conn, [_spec("one"), _spec("two")])
        claimed = db.claim_next(self.conn)
        self.assertEqual(claimed["status"], "pending")
        stored = db.get(self.conn, claimed["key"])
        self.assertEqual(stored["status"], "running")
        reclaimed = db.reclaim_orphans(self.conn)
        self.assertEqual(reclaimed, 1)
        again = db.claim_next(self.conn)
        self.assertEqual(again["key"], claimed["key"])
        self.assertEqual(again["attempts"], 1)  # reclaim keeps history, re-claim increments

    def test_retry_then_failed_terminal_state(self):
        spec = _spec("retry")
        db.upsert_pending(self.conn, [spec])
        for expected_attempt in range(1, 4):
            row = db.claim_next(self.conn)
            stored = db.get(self.conn, row["key"])
            self.assertEqual(stored["attempts"], expected_attempt)
            if expected_attempt < 3:
                db.requeue_failed_attempt(self.conn, row["key"], "boom")
            else:
                db.mark_failed(self.conn, row["key"], "boom")
        self.assertEqual(db.counts(self.conn).get("failed"), 1)
        self.assertIsNone(db.claim_next(self.conn))

    def test_done_row_persists_metrics(self):
        db.upsert_pending(self.conn, [_spec("done")])
        row = db.claim_next(self.conn)
        db.mark_done(self.conn, row["key"], {
            "bytes": 4096, "duration_s": 1.0, "actual_kbps": 32.768,
            "psnr_db": 35.5, "psnr_min_db": 33.2, "ssim": 0.97,
            "ssim_min": 0.95, "bitstream_path": "x.abr",
        })
        done = db.completed_rows(self.conn)
        self.assertEqual(len(done), 1)
        self.assertEqual(done[0]["actual_kbps"], 32.768)
        self.assertIsNone(db.claim_next(self.conn))


if __name__ == "__main__":
    unittest.main()


class RunnerIntegrationTests(unittest.TestCase):
    def test_sweep_encodes_and_persists_metrics(self):
        import tempfile
        from pathlib import Path
        from abrl.scheduler.grid import TaskSpec
        from abrl.scheduler.runner import run_sweep

        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        source = Path(tmp.name) / "clip.yuv"
        from abrl.media.yuv import Frame, write_frames
        import numpy as np
        rng = np.random.default_rng(11)
        frames = [
            Frame(rng.integers(0, 256, (32, 32), dtype=np.uint8),
                  rng.integers(0, 256, (16, 16), dtype=np.uint8),
                  rng.integers(0, 256, (16, 16), dtype=np.uint8))
            for _ in range(6)
        ]
        write_frames(source, frames)
        specs = [
            TaskSpec("integ1", str(source), "9" * 64, 32, 32, 24, 20, 8, ENCODER_VERSION),
            TaskSpec("integ2", str(source), "9" * 64, 32, 32, 24, 32, 8, ENCODER_VERSION),
        ]
        db_path = str(Path(tmp.name) / "sweep.db")
        conn = db.connect(db_path)
        db.initialize(conn)
        stats = run_sweep(conn, specs, 0, 1, db_path, 32, 32)
        self.assertEqual(stats.done, 2)
        self.assertEqual(stats.failed, 0)
        rows = {row["key"]: row for row in db.completed_rows(conn)}
        self.assertEqual(set(rows), {"integ1", "integ2"})
        for row in rows.values():
            self.assertGreater(row["bytes"], 0)
            self.assertGreater(row["actual_kbps"], 0)
            self.assertTrue(Path(row["bitstream_path"]).is_file())
        conn.close()
