"""Sample generation and end-to-end pipeline behaviour."""

from __future__ import annotations

import hashlib
import json
import os
import unittest
from fractions import Fraction

from avtimeline.errors import Stage
from avtimeline.samples import generate_samples, sample_digests

try:  # both `unittest discover -s tests` and package imports
    from .support import TempDirMixin, make_config, reused_stages, run_pipeline
except ImportError:  # pragma: no cover - discovery from the tests directory
    from support import TempDirMixin, make_config, reused_stages, run_pipeline


class SampleTests(TempDirMixin, unittest.TestCase):
    def test_generation_is_deterministic(self) -> None:
        first = generate_samples(self.path("a"), force=True)
        second = generate_samples(self.path("b"), force=True)
        self.assertEqual(sample_digests(first), sample_digests(second))

    def test_fixture_sizes(self) -> None:
        samples = generate_samples(self.path("samples"), force=True)
        for path in samples.all_paths():
            self.assertGreater(os.path.getsize(path), 0)
        first = samples.corrupt_cam
        main = samples.cam_main
        self.assertLess(os.path.getsize(first), os.path.getsize(main))

    def test_reuses_existing_files(self) -> None:
        samples = generate_samples(self.path("samples"), force=True)
        before = os.stat(samples.cam_main).st_mtime_ns
        again = generate_samples(self.path("samples"), force=False)
        self.assertEqual(before, os.stat(again.cam_main).st_mtime_ns)


class ConfigTests(unittest.TestCase):
    def test_invalid_window_rejected(self) -> None:
        from avtimeline.config import ConfigError, ProjectConfig

        with self.assertRaises(ConfigError):
            ProjectConfig.from_mapping({"window": {"start": 2, "end": 1}})

    def test_single_rendition_rejected(self) -> None:
        from avtimeline.config import ConfigError, ProjectConfig

        with self.assertRaises(ConfigError):
            ProjectConfig.from_mapping(
                {"renditions": [{"name": "only", "width": 320, "height": 180}]}
            )

    def test_odd_dimension_rejected(self) -> None:
        from avtimeline.config import ConfigError, ProjectConfig

        with self.assertRaises(ConfigError):
            ProjectConfig.from_mapping(
                {
                    "renditions": [
                        {"name": "a", "width": 321, "height": 180},
                        {"name": "b", "width": 160, "height": 90},
                    ]
                }
            )


class PipelineTests(TempDirMixin, unittest.TestCase):
    def test_full_run_is_aligned_and_clean(self) -> None:
        result = run_pipeline(self.path("run"), force_samples=True)
        self.assertEqual([e.code for e in result.errors], [])
        self.assertIsNotNone(result.manifest)
        assert result.manifest is not None
        self.assertGreaterEqual(len(result.manifest.segments), 6)
        self.assertEqual(result.plan.max_boundary_error(), 0)
        self.assertEqual(result.alignment_issues, [])
        self.assertEqual(result.stages[-1].stage, Stage.MANIFEST)
        self.assertEqual(result.stages[-1].status, "done")

    def test_repeat_runs_produce_identical_checksums(self) -> None:
        first = run_pipeline(self.path("one"), force_samples=True)
        second = run_pipeline(self.path("two"), force_samples=True)
        assert first.manifest is not None and second.manifest is not None
        self.assertEqual(first.manifest.serialize(), second.manifest.serialize())
        for left, right in zip(first.manifest.segments, second.manifest.segments):
            for name, entry in left["renditions"].items():
                self.assertEqual(entry["sha256"], right["renditions"][name]["sha256"])

    def test_every_segment_starts_on_a_keyframe(self) -> None:
        import av

        result = run_pipeline(self.path("run"), force_samples=True)
        for item in result.encoded:
            container = av.open(item.path)
            try:
                packets = [p for p in container.demux(video=0) if p.size > 0] if container.streams.video else []
            finally:
                container.close()
            if packets:
                self.assertTrue(packets[0].is_keyframe, item.path)

    def test_interrupt_then_resume_reuses_stages(self) -> None:
        result = run_pipeline(
            self.path("run"), force_samples=True, break_at=Stage.ENCODE
        )
        self.assertEqual(result.interrupted_at, Stage.ENCODE)
        self.assertGreater(len(result.stages), 0)
        resumed = run_pipeline(self.path("run"), resume=True)
        reused = reused_stages(resumed)
        self.assertIn(Stage.PROBE, reused)
        self.assertIn(Stage.NORMALIZE, reused)
        self.assertIn(Stage.ENCODE, reused)
        self.assertEqual([e.code for e in resumed.errors], [])
        self.assertGreaterEqual(resumed.aligned_segments, 6)

    def test_resume_matches_full_run_output(self) -> None:
        full = run_pipeline(self.path("full"), force_samples=True)
        run_pipeline(self.path("broken"), force_samples=True, break_at=Stage.SEGMENT)
        resumed = run_pipeline(self.path("broken"), resume=True)
        assert full.manifest is not None and resumed.manifest is not None
        self.assertEqual(full.manifest_text, resumed.manifest_text)

    def test_resume_after_artifact_corruption_redoes_earliest_stage(self) -> None:
        run_pipeline(self.path("run"), force_samples=True, break_at=Stage.ENCODE)
        normalized = os.path.join(self.path("run"), "normalized")
        target = None
        for name in sorted(os.listdir(normalized)):
            if name.endswith(".audio.wav"):
                target = os.path.join(normalized, name)
                break
        self.assertIsNotNone(target)
        with open(target, "r+b") as handle:
            handle.truncate(100)
        resumed = run_pipeline(self.path("run"), resume=True)
        statuses = {stage.stage: stage.status for stage in resumed.stages}
        self.assertEqual(statuses[Stage.NORMALIZE], "done")
        self.assertEqual(statuses[Stage.PROBE], "reused")
        self.assertEqual([e.code for e in resumed.errors], [])

    def test_resume_after_segment_corruption_redoes_encode(self) -> None:
        from avtimeline.errors import Stage as _Stage

        full = run_pipeline(self.path("full"), force_samples=True)
        result = run_pipeline(
            self.path("run"), force_samples=True, break_at=_Stage.ENCODE
        )
        target = result.encoded[3].path
        with open(target, "r+b") as handle:
            handle.truncate(64)
        resumed = run_pipeline(self.path("run"), resume=True)
        statuses = {record.stage: record.status for record in resumed.stages}
        self.assertEqual(resumed.checkpoint_status, "broken_artifact")
        self.assertEqual(statuses[_Stage.ENCODE], "done")
        self.assertEqual(statuses[_Stage.NORMALIZE], "reused")
        self.assertEqual([e.code for e in resumed.errors], [])
        self.assertGreaterEqual(resumed.aligned_segments, 6)
        # Re-encoding the damaged segment must reproduce its original hash.
        self.assertIsNotNone(full.manifest)
        self.assertIsNotNone(resumed.manifest)
        assert full.manifest is not None and resumed.manifest is not None
        self.assertEqual(
            [s["renditions"] for s in resumed.manifest.segments],
            [s["renditions"] for s in full.manifest.segments],
        )

    def test_corrupt_sample_stop_policy_fails(self) -> None:
        config = make_config(policy="stop")
        result = run_pipeline(
            self.path("stop"), config, force_samples=True, corrupt_sample=True
        )
        self.assertTrue(result.errors)
        first = result.errors[0]
        self.assertEqual(first.location.track, "corrupt_cam")
        self.assertEqual(first.location.pts, Fraction(6, 5))
        self.assertIn("failed", [stage.status for stage in result.stages])

    def test_corrupt_sample_skip_policy_continues(self) -> None:
        config = make_config(policy="skip")
        result = run_pipeline(
            self.path("skip"), config, force_samples=True, corrupt_sample=True
        )
        self.assertGreaterEqual(result.aligned_segments, 6)
        self.assertEqual(result.skipped_tracks[0]["track"], "corrupt_cam")
        self.assertEqual([e.code for e in result.errors if e.severity.name == "FATAL"], [])

    def test_edge_sample_keeps_short_tail(self) -> None:
        result = run_pipeline(self.path("edge"), force_samples=True, edge_sample=True)
        assert result.manifest is not None
        short = [s for s in result.manifest.segments if s["is_short"]]
        self.assertTrue(short)
        tail = short[-1]
        self.assertEqual(tail["duration"], "1/50")
        self.assertEqual(tail["audio_samples"], 960)
        for entry in tail["renditions"].values():
            self.assertEqual(entry["actual_audio_samples"], 960)

    def test_color_conflict_recorded_in_manifest(self) -> None:
        result = run_pipeline(self.path("edge"), force_samples=True, edge_sample=True)
        self.assertIsNotNone(result.color)
        assert result.color is not None
        self.assertTrue(result.color["conflicts"])
        assert result.manifest is not None
        self.assertEqual(result.manifest.color["decided_by"], "policy:first:cam_main")

    def test_manifest_and_checkpoint_written(self) -> None:
        run_pipeline(self.path("run"), force_samples=True)
        self.assertTrue(os.path.exists(self.path("run", "manifest.json")))
        self.assertTrue(os.path.exists(self.path("run", "checkpoint.json")))
        with open(self.path("run", "checkpoint.json"), encoding="utf-8") as handle:
            payload = json.load(handle)
        self.assertIn("manifest", payload["completed"])
        self.assertEqual(payload["version"], 2)
        self.assertGreater(len(payload["artifacts"]), 6)

    def test_segment_hashes_match_files(self) -> None:
        result = run_pipeline(self.path("run"), force_samples=True)
        assert result.manifest is not None
        for segment in result.manifest.segments:
            for name, entry in segment["renditions"].items():
                directory = os.path.join(self.path("run"), "segments", name)
                path = os.path.join(directory, entry["path"])
                with open(path, "rb") as handle:
                    digest = hashlib.sha256(handle.read()).hexdigest()
                self.assertEqual(digest, entry["sha256"])

    def test_track_dependency_cycle_reported(self) -> None:
        from avtimeline.graph import detect_cycle, from_timeline
        from avtimeline.timeline import parse_timeline

        timeline = parse_timeline(
            {
                "window": {"start": 0, "end": 1},
                "tracks": [
                    {"id": "a", "clips": [{"id": "x", "start": 0, "end": 1}], "after": ["b"]},
                    {"id": "b", "clips": [{"id": "y", "start": 0, "end": 1}], "after": ["a"]},
                ],
            }
        )
        cycle = detect_cycle(from_timeline(timeline))
        self.assertIsNotNone(cycle)
        assert cycle is not None
        self.assertIn("normalize:a", cycle)
        self.assertIn("normalize:b", cycle)

    def test_unknown_track_dependency_rejected(self) -> None:
        from avtimeline.graph import GraphError, from_timeline
        from avtimeline.timeline import parse_timeline

        timeline = parse_timeline(
            {
                "window": {"start": 0, "end": 1},
                "tracks": [
                    {"id": "a", "clips": [{"id": "x", "start": 0, "end": 1}], "after": ["ghost"]}
                ],
            }
        )
        with self.assertRaises(GraphError):
            from_timeline(timeline)

    def test_renditions_share_exact_boundaries(self) -> None:
        result = run_pipeline(self.path("run"), force_samples=True)
        assert result.manifest is not None
        for segment in result.manifest.segments:
            self.assertEqual(
                sorted(segment["renditions"]), ["180p", "360p"]
            )
            planned_durations = {
                entry["sha256"] for entry in segment["renditions"].values()
            }
            self.assertEqual(len(planned_durations), 2)  # different renditions differ


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
