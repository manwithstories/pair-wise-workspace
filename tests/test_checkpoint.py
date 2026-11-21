"""Dependency graph, checkpoint persistence and manifest alignment."""

from __future__ import annotations

import json
import os
import unittest
from fractions import Fraction

from avtimeline.checkpoint import (
    Artifact,
    Checkpoint,
    CheckpointStatus,
    input_digest,
    load_checkpoint,
    reusable_stages,
    save_checkpoint,
)
from avtimeline.errors import Stage
from avtimeline.graph import DependencyGraph, GraphError, detect_cycle

try:  # both `unittest discover -s tests` and package imports
    from .support import TempDirMixin
except ImportError:  # pragma: no cover - discovery from the tests directory
    from support import TempDirMixin


class GraphTests(unittest.TestCase):
    def test_default_order(self) -> None:
        graph = DependencyGraph.default(["cam", "mic"])
        order = graph.topological_order()
        self.assertEqual(order[0], "timeline")
        self.assertEqual(order[-1], "manifest")
        self.assertLess(order.index("probe:cam"), order.index("normalize:cam"))
        self.assertLess(order.index("normalize:cam"), order.index("transition"))

    def test_cycle_detected_with_stages_and_tracks(self) -> None:
        graph = DependencyGraph.default(["cam"])
        graph.add_edge("manifest", "probe:cam")
        cycle = detect_cycle(graph)
        self.assertIsNotNone(cycle)
        assert cycle is not None
        self.assertIn("probe", cycle)
        self.assertIn("cam", cycle)
        with self.assertRaises(GraphError):
            graph.topological_order()

    def test_unknown_edge_rejected(self) -> None:
        graph = DependencyGraph.default([])
        with self.assertRaises(GraphError):
            graph.add_edge("nope", "manifest")

    def test_cycle_stage_labels(self) -> None:
        graph = DependencyGraph.default([])
        graph.add_edge("manifest", "timeline")
        labels = graph.stages_on_cycle(graph.find_cycle() or [])
        self.assertIn("timeline", labels)
        self.assertIn("manifest", labels)


class CheckpointTests(TempDirMixin, unittest.TestCase):
    def _artifact(self, name: str, content: bytes = b"data") -> str:
        path = self.path(name)
        with open(path, "wb") as handle:
            handle.write(content)
        return path

    def test_roundtrip_and_status(self) -> None:
        artifact = self._artifact("a.bin")
        checkpoint = Checkpoint(input_digest="abc")
        checkpoint.mark_complete(Stage.PROBE, [Artifact.from_path(Stage.PROBE, artifact)])
        save_checkpoint(checkpoint, self._dir)
        result = load_checkpoint(self._dir, "abc")
        self.assertEqual(result.status, CheckpointStatus.VALID)
        assert result.checkpoint is not None
        self.assertEqual(result.checkpoint.completed, [Stage.PROBE])
        self.assertEqual(result.checkpoint.resume_stage, Stage.NORMALIZE)

    def test_stale_input_rejects(self) -> None:
        checkpoint = Checkpoint(input_digest="abc")
        save_checkpoint(checkpoint, self._dir)
        result = load_checkpoint(self._dir, "different")
        self.assertEqual(result.status, CheckpointStatus.STALE_INPUT)
        self.assertEqual(result.first_invalid_stage, Stage.TIMELINE)

    def test_missing_artifact_reports_stage(self) -> None:
        probe = self._artifact("probe.json")
        normalized = self._artifact("norm.wav")
        checkpoint = Checkpoint(input_digest="abc")
        checkpoint.mark_complete(Stage.PROBE, [Artifact.from_path(Stage.PROBE, probe)])
        checkpoint.mark_complete(
            Stage.NORMALIZE, [Artifact.from_path(Stage.NORMALIZE, normalized)]
        )
        save_checkpoint(checkpoint, self._dir)
        os.unlink(normalized)
        result = load_checkpoint(self._dir, "abc")
        self.assertEqual(result.status, CheckpointStatus.BROKEN_ARTIFACT)
        self.assertEqual(result.first_invalid_stage, Stage.NORMALIZE)
        reusable = reusable_stages(result.checkpoint, result.status, result.first_invalid_stage)
        self.assertEqual(reusable, [Stage.PROBE])

    def test_partial_write_rejected(self) -> None:
        artifact = self._artifact("norm.wav", b"0123456789")
        checkpoint = Checkpoint(input_digest="abc")
        checkpoint.mark_complete(
            Stage.NORMALIZE, [Artifact.from_path(Stage.NORMALIZE, artifact)]
        )
        save_checkpoint(checkpoint, self._dir)
        with open(artifact, "wb") as handle:
            handle.write(b"0123")
        result = load_checkpoint(self._dir, "abc")
        self.assertEqual(result.status, CheckpointStatus.BROKEN_ARTIFACT)
        self.assertEqual(result.first_invalid_stage, Stage.NORMALIZE)

    def test_corrupt_document_rejected(self) -> None:
        with open(self.path("checkpoint.json"), "w", encoding="utf-8") as handle:
            handle.write("{not json")
        result = load_checkpoint(self._dir)
        self.assertEqual(result.status, CheckpointStatus.INVALID_FORMAT)

    def test_missing_checkpoint(self) -> None:
        result = load_checkpoint(self._dir)
        self.assertEqual(result.status, CheckpointStatus.MISSING)

    def test_input_digest_changes_with_files(self) -> None:
        first = self._artifact("first.bin", b"one")
        second = self._artifact("second.bin", b"two")
        a = input_digest({"x": 1}, [first])
        b = input_digest({"x": 1}, [first, second])
        c = input_digest({"x": 2}, [first])
        self.assertNotEqual(a, b)
        self.assertNotEqual(a, c)

    def test_artifact_paths_are_relative_in_document(self) -> None:
        artifact = self._artifact("nested.bin")
        checkpoint = Checkpoint(input_digest="abc")
        checkpoint.mark_complete(Stage.PROBE, [Artifact.from_path(Stage.PROBE, artifact, self._dir)])
        save_checkpoint(checkpoint, self._dir)
        with open(self.path("checkpoint.json"), "r", encoding="utf-8") as handle:
            payload = json.load(handle)
        self.assertFalse(os.path.isabs(payload["artifacts"][0]["path"]))


class ManifestTests(TempDirMixin, unittest.TestCase):
    def test_alignment_detects_missing_rendition(self) -> None:
        from avtimeline.encode import EncodedSegment
        from avtimeline.manifest import build_manifest, check_alignment
        from avtimeline.model import Rate, Span
        from avtimeline.segment import plan_segments

        rate = Rate(25)
        plan = plan_segments(Span.of(0, Rate(25).frame_duration * 10), rate, 48000, Rate(25).frame_duration * 10)
        segment = EncodedSegment(
            rendition="360p",
            index=0,
            path=self.path("x.mp4"),
            sha256="0" * 64,
            size=10,
            duration=plan.segments[0].duration,
            video_frames=plan.segments[0].video_frames,
            audio_samples=plan.segments[0].audio_samples,
            actual_video_frames=plan.segments[0].video_frames,
            actual_audio_samples=plan.segments[0].audio_samples,
        )
        from avtimeline.encode import Rendition

        manifest = build_manifest(
            plan,
            {"360p": [segment]},
            [Rendition("360p", 320, 180, 1, 1), Rendition("180p", 160, 90, 1, 1)],
            {"start": "0", "end": "1"},
            Rate(25).frame_duration * 10,
        )
        issues = check_alignment(manifest, ["360p", "180p"])
        self.assertIn("missing_rendition", [i.code for i in issues])

    def test_ntsc_audio_rounding_is_accepted(self) -> None:
        from avtimeline.encode import EncodedSegment, Rendition
        from avtimeline.manifest import build_manifest, check_alignment
        from avtimeline.model import Rate, Span
        from avtimeline.segment import plan_segments

        rate = Rate.parse("30000/1001")
        plan = plan_segments(Span.of(0, Fraction(101, 50)), rate, 48000, Fraction(2, 5))
        encoded = [
            EncodedSegment(
                rendition="a",
                index=segment.index,
                path=self.path("a.mp4"),
                sha256="0" * 64,
                size=1,
                duration=segment.duration,
                video_frames=segment.video_frames,
                audio_samples=segment.audio_samples,
                actual_video_frames=segment.video_frames,
                actual_audio_samples=segment.audio_samples,
            )
            for segment in plan.segments
        ]
        manifest = build_manifest(
            plan,
            {"a": encoded},
            [Rendition("a", 320, 180, 1, 1)],
            {"start": "0", "end": "101/50"},
            Fraction(2, 5),
        )
        self.assertGreater(plan.max_boundary_error(), 0)
        self.assertEqual(check_alignment(manifest), [])

    def test_serialization_is_deterministic(self) -> None:
        from avtimeline.encode import EncodedSegment, Rendition
        from avtimeline.manifest import build_manifest
        from avtimeline.model import Rate, Span
        from avtimeline.segment import plan_segments

        rate = Rate(25)
        plan = plan_segments(Span.of(0, Rate(25).frame_duration * 10), rate, 48000, Rate(25).frame_duration * 10)
        segment = EncodedSegment(
            rendition="360p",
            index=0,
            path=self.path("y.mp4"),
            sha256="f" * 64,
            size=10,
            duration=plan.segments[0].duration,
            video_frames=plan.segments[0].video_frames,
            audio_samples=plan.segments[0].audio_samples,
            actual_video_frames=plan.segments[0].video_frames,
            actual_audio_samples=plan.segments[0].audio_samples,
        )
        kwargs = dict(
            plan=plan,
            segments_by_rendition={"360p": [segment]},
            renditions=[Rendition("360p", 320, 180, 1, 1)],
            window={"start": "0", "end": "1"},
            segment_duration=Rate(25).frame_duration * 10,
        )
        first = build_manifest(**kwargs).serialize()
        second = build_manifest(**kwargs).serialize()
        self.assertEqual(first, second)


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
