from __future__ import annotations

import io
import json
import os
import tempfile
import unittest
from pathlib import Path

from PIL import Image

from fixtures.generate_corpus import generate, image_negative_scene
from mediaidx.indexer import Indexer


def cluster_signature(report: dict[str, object]) -> list[tuple[str, str, tuple[str, ...]]]:
    return sorted(
        (
            cluster["cluster_id"],
            cluster["family"],
            tuple(sorted(member["path"] for member in cluster["members"])),
        )
        for cluster in report["clusters"]
    )


def cluster_for_path(report: dict[str, object], path: str) -> tuple[str, ...]:
    for cluster in report["clusters"]:
        members = tuple(sorted(member["path"] for member in cluster["members"]))
        if path in members:
            return members
    return ()


class IntegrationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory(prefix="mediaidx-test-")
        base = Path(self.temporary.name)
        self.root = base / "corpus"
        self.manifest = base / "corpus.labels.json"
        generate(self.root, self.manifest, force=True)

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def test_jobs_are_deterministic_and_rerun_decodes_nothing(self) -> None:
        base = Path(self.temporary.name)
        report_one = Indexer(self.root, base / "one.db", jobs=1).run()
        report_four = Indexer(self.root, base / "four.db", jobs=4).run()

        self.assertEqual(report_one["summary"]["conflict_rate"], 0.0)
        self.assertEqual(report_four["summary"]["conflict_rate"], 0.0)
        self.assertEqual(report_one["summary"]["success"], 27)
        self.assertEqual(report_one["summary"]["failed"], 3)
        self.assertEqual(report_one["summary"]["unique_assets"], 10)
        self.assertEqual(cluster_signature(report_one), cluster_signature(report_four))
        self.assertEqual(report_one["summary"]["candidate_pairs"], report_four["summary"]["candidate_pairs"])
        self.assertLess(
            report_one["summary"]["candidate_pairs"],
            27 * 26 // 2,
        )

        labels = {
            path: asset_id
            for asset_id, paths in json.loads(self.manifest.read_text(encoding="utf-8"))["assets"].items()
            for path in paths
        }
        for cluster in report_one["clusters"]:
            labeled = {
                labels[member["path"]]
                for member in cluster["members"]
                if member["path"] in labels
            }
            self.assertLessEqual(len(labeled), 1, cluster)
        for asset_id, asset_paths in json.loads(self.manifest.read_text(encoding="utf-8"))["assets"].items():
            clusters = {cluster_for_path(report_one, path) for path in asset_paths}
            self.assertEqual(len(clusters), 1, asset_id)

        report_rerun = Indexer(self.root, base / "four.db", jobs=4).run()
        self.assertEqual(report_rerun["index"]["decode_calls"], 0)
        self.assertEqual(report_rerun["index"]["unchanged_files"], 27)
        self.assertEqual(cluster_signature(report_rerun), cluster_signature(report_four))
        self.assertEqual(report_rerun["summary"]["conflict_rate"], 0.0)

    def test_same_size_same_mtime_replacement_is_detected(self) -> None:
        base = Path(self.temporary.name)
        db_path = base / "replace.db"
        first = Indexer(self.root, db_path, jobs=2).run()
        target = self.root / "delivery/images/a_half.jpg"
        negative_peer = "delivery/images/negative_01.jpg"
        self.assertNotEqual(cluster_for_path(first, target.relative_to(self.root).as_posix()), cluster_for_path(first, negative_peer))

        original = target.read_bytes()
        original_stat = target.stat()
        replacement = image_negative_scene(4).resize((320, 240), Image.Resampling.LANCZOS)
        encoded = b""
        for quality in range(85, 9, -5):
            buffer = io.BytesIO()
            replacement.save(buffer, format="JPEG", quality=quality)
            encoded = buffer.getvalue()
            if len(encoded) <= len(original):
                break
        self.assertLessEqual(len(encoded), len(original))
        target.write_bytes(encoded + b"\x00" * (len(original) - len(encoded)))
        os.utime(target, ns=(original_stat.st_atime_ns, original_stat.st_mtime_ns))
        replaced_stat = target.stat()
        self.assertEqual(replaced_stat.st_size, original_stat.st_size)
        self.assertEqual(replaced_stat.st_mtime_ns, original_stat.st_mtime_ns)

        # The file's content now belongs to the negative asset's truth group.
        # Update the benchmark manifest to reflect that replacement; conflict
        # accounting must then return to zero for this synthetic corpus.
        manifest_data = json.loads(self.manifest.read_text(encoding="utf-8"))
        relative_target = target.relative_to(self.root).as_posix()
        for paths in manifest_data["assets"].values():
            if relative_target in paths:
                paths.remove(relative_target)
        manifest_data["assets"]["image_negative_01"].append(relative_target)
        self.manifest.write_text(json.dumps(manifest_data, ensure_ascii=False), encoding="utf-8")

        second = Indexer(self.root, db_path, jobs=2).run()
        self.assertEqual(second["index"]["decode_calls"], 1)
        replaced_cluster = cluster_for_path(second, target.relative_to(self.root).as_posix())
        negative_cluster = cluster_for_path(second, negative_peer)
        self.assertEqual(replaced_cluster, negative_cluster)
        self.assertNotIn("delivery/images/a_master.png", replaced_cluster)
        self.assertEqual(second["summary"]["conflict_rate"], 0.0)

        target.unlink()
        third = Indexer(self.root, db_path, jobs=2).run()
        self.assertEqual(third["index"]["deleted_files"], 1)
        self.assertEqual(cluster_for_path(third, negative_peer), (negative_peer,))
        self.assertEqual(third["summary"]["success"], 26)


if __name__ == "__main__":
    unittest.main()
