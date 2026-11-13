from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

import numpy as np

from mediaidx.canonical import MediaFamily, MediaKind
from mediaidx.dedup import Deduper, DisjointSet
from mediaidx.fingerprint import FINGERPRINT_VERSION, pack_fingerprint
from mediaidx.store import Store


def fake_image_payload(phash: int) -> tuple[bytes, str]:
    collapsed = f"{phash:016x}"
    detail = np.zeros(144, dtype=np.float32)
    detail[1] = 1.0
    thumbnail = np.arange(1024, dtype=np.uint8)
    fingerprint = {
        "type": "image",
        "version": FINGERPRINT_VERSION,
        "phash": collapsed,
        "dhash": "0000000000000000",
        "candidate_hashes": [collapsed],
        "detail": detail,
        "thumbnail": thumbnail,
    }
    return pack_fingerprint(fingerprint), collapsed


class StoreDedupTests(unittest.TestCase):
    def test_dsu_and_cluster_split_after_bridge_deletion(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            store = Store(Path(temporary) / "db.sqlite")
            try:
                kind = MediaKind(MediaFamily.IMAGE, "png")
                # A-B share bands 2/3, B-C share bands 0/1, while A-C share no
                # band.  LSH therefore forms a chain only through B.
                values = [
                    (0x0000000000000000, "a" * 64, "a.png"),
                    (0x0001000200000000, "b" * 64, "b.png"),
                    (0x0001000200030004, "c" * 64, "c.png"),
                ]
                for phash, content_hash, rel_path in values:
                    payload, hassh = fake_image_payload(phash)
                    store.upsert_success(
                        rel_path=rel_path,
                        kind=kind,
                        content_hash=content_hash,
                        size=4,
                        mtime_ns=1,
                        payload=payload,
                        hashes=[hassh],
                        metadata={},
                        loader_name="test",
                        loader_version="test",
                    )
                stats = Deduper(store).run()
                self.assertEqual(stats.candidate_pairs, 2)
                self.assertEqual(stats.verified_pairs, 2)
                self.assertEqual(stats.clusters, 1)
                self.assertEqual(stats.duplicate_clusters, 1)

                store.remove_path("b.png")
                stats = Deduper(store).run()
                self.assertEqual(stats.candidate_pairs, 0)
                self.assertEqual(stats.clusters, 2)
                self.assertEqual(stats.duplicate_clusters, 0)
                self.assertEqual(len(store.cluster_members()), 2)
            finally:
                store.close()

    def test_candidate_pairs_are_not_all_pairs(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            store = Store(Path(temporary) / "db.sqlite")
            try:
                kind = MediaKind(MediaFamily.IMAGE, "png")
                # 40 unrelated hashes produce few shared bands, while full
                # comparison would produce 780 pairs.
                for index in range(40):
                    value = (index * 0x9E3779B97F4A7C15) & ((1 << 64) - 1)
                    payload, hash_text = fake_image_payload(value)
                    store.upsert_success(
                        rel_path=f"file_{index:03d}.png",
                        kind=kind,
                        content_hash=f"{index:064x}",
                        size=4,
                        mtime_ns=1,
                        payload=payload,
                        hashes=[hash_text],
                        metadata={},
                        loader_name="test",
                        loader_version="test",
                    )
                counts = store.rebuild_candidate_tables()
                self.assertLess(counts["candidate_pairs"], 780)
                self.assertEqual(counts["candidate_pairs"], len(store.candidate_pairs()))
            finally:
                store.close()

    def test_wal_and_transaction_rollback(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            store = Store(Path(temporary) / "db.sqlite")
            try:
                mode = store.connection.execute("PRAGMA journal_mode").fetchone()[0]
                self.assertEqual(mode.lower(), "wal")
                with self.assertRaises(RuntimeError):
                    with store.transaction() as cursor:
                        cursor.execute("INSERT INTO quarantine(rel_path, content_hash, error_type, error_message, updated_at) VALUES ('x', 'h', 'e', 'm', 0)")
                        raise RuntimeError("interrupt")
                self.assertIsNone(store.connection.execute("SELECT * FROM quarantine WHERE rel_path='x'").fetchone())
            finally:
                store.close()

    def test_disjoint_set(self) -> None:
        dsu = DisjointSet([1, 2, 3, 4])
        dsu.union(1, 2)
        dsu.union(2, 3)
        self.assertEqual(dsu.find(1), dsu.find(3))
        self.assertNotEqual(dsu.find(1), dsu.find(4))


if __name__ == "__main__":
    unittest.main()
