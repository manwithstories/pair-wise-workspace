"""Probe, normalization and colour resolution."""

from __future__ import annotations

import unittest
from fractions import Fraction

import soundfile as sf

from avtimeline.model import Rate
from avtimeline.normalize import (
    ColorMetadata,
    ColorPolicy,
    VideoSpec,
    audio_sample_count,
    normalize_audio,
    normalize_video,
    read_wav_span,
    resolve_color,
)
from avtimeline.probe import probe_media
from avtimeline.samples import generate_samples

try:  # both `unittest discover -s tests` and package imports
    from .support import TempDirMixin
except ImportError:  # pragma: no cover - discovery from the tests directory
    from support import TempDirMixin


class ProbeTests(TempDirMixin, unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        import tempfile

        cls._samples_dir = tempfile.mkdtemp(prefix="avtimeline-samples-")
        cls.samples = generate_samples(cls._samples_dir, force=True)

    @classmethod
    def tearDownClass(cls) -> None:
        import shutil

        shutil.rmtree(cls._samples_dir, ignore_errors=True)

    def test_video_stream_metadata(self) -> None:
        probe = probe_media(self.samples.cam_main, track_id="cam_main")
        video = probe.video()
        self.assertIsNotNone(video)
        assert video is not None
        self.assertEqual(video.width, 640)
        self.assertEqual(video.height, 360)
        self.assertEqual(video.rate, Rate(25))
        self.assertEqual(probe.errors, [])
        self.assertTrue(probe.keyframe_times)

    def test_audio_metadata_after_normalization_inputs(self) -> None:
        audio = probe_media(self.samples.team_voice, track_id="team").audio()
        self.assertIsNotNone(audio)
        assert audio is not None
        self.assertEqual(audio.sample_rate, 48000)
        self.assertEqual(audio.channels, 6)
        self.assertEqual(audio.layout, "5.1")

        mono = probe_media(self.samples.player_voice, track_id="player").audio()
        assert mono is not None
        self.assertEqual(mono.sample_rate, 44100)
        self.assertEqual(mono.channels, 1)

    def test_truncated_video_reports_first_bad_frame(self) -> None:
        probe = probe_media(self.samples.corrupt_cam, track_id="corrupt")
        self.assertTrue(probe.errors)
        first = probe.errors[0]
        self.assertEqual(first.location.track, "corrupt")
        self.assertIsNotNone(first.location.frame)
        self.assertIsNotNone(first.location.pts)
        # The truncation lands inside the 1.2 s-1.24 s frame at 25 fps.
        self.assertEqual(first.location.pts, Fraction(6, 5))
        self.assertEqual(first.location.frame, 30)

    def test_empty_file_is_fatal(self) -> None:
        path = self.path("empty.mp4")
        open(path, "wb").close()
        probe = probe_media(path, track_id="empty")
        self.assertTrue(probe.errors)
        self.assertEqual(probe.errors[0].code, "empty_file")

    def test_garbage_file_is_fatal(self) -> None:
        path = self.path("garbage.mp4")
        with open(path, "wb") as handle:
            handle.write(b"not media" * 64)
        probe = probe_media(path, track_id="garbage")
        self.assertTrue(probe.errors)
        self.assertEqual(probe.errors[0].code, "cannot_open")

    def test_short_tail_video(self) -> None:
        probe = probe_media(self.samples.edge_tail, track_id="edge")
        self.assertEqual(probe.errors, [])
        self.assertGreater(probe.decoded_video_frames or 0, 0)


class NormalizeTests(TempDirMixin, unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        import tempfile

        cls._samples_dir = tempfile.mkdtemp(prefix="avtimeline-samples-")
        cls.samples = generate_samples(cls._samples_dir, force=True)

    @classmethod
    def tearDownClass(cls) -> None:
        import shutil

        shutil.rmtree(cls._samples_dir, ignore_errors=True)

    def test_surround_downmix_is_exact_48k_stereo(self) -> None:
        probe = probe_media(self.samples.team_voice, track_id="team")
        audio, error = normalize_audio(probe, self.path("norm"), "team")
        self.assertIsNone(error)
        assert audio is not None
        self.assertEqual(audio.samples, 105600)
        info = sf.info(audio.path)
        self.assertEqual(info.samplerate, 48000)
        self.assertEqual(info.channels, 2)

    def test_mono_44k_resampled_to_48k(self) -> None:
        probe = probe_media(self.samples.player_voice, track_id="player")
        audio, error = normalize_audio(probe, self.path("norm"), "player")
        self.assertIsNone(error)
        assert audio is not None
        # 105840 samples at 44.1 kHz is exactly 2.4 s -> 115200 at 48 kHz.
        self.assertEqual(audio.samples, 115200)

    def test_video_normalized_frames_and_colour(self) -> None:
        probe = probe_media(self.samples.cam_main, track_id="cam_main")
        decision = resolve_color(
            "cam_main",
            {"cam_main": ColorMetadata.from_stream(probe.video())},
            ColorPolicy(),
        )
        video, error = normalize_video(
            probe, self.path("norm"), "cam_main", VideoSpec(320, 180, Rate(25)), decision
        )
        self.assertIsNone(error)
        assert video is not None
        self.assertEqual(video.frames, 55)
        import av

        container = av.open(video.path)
        try:
            stream = container.streams.video[0]
            self.assertEqual(stream.codec_context.colorspace, 1)
            self.assertEqual(stream.codec_context.color_primaries, 1)
        finally:
            container.close()

    def test_read_wav_span_exact_samples(self) -> None:
        probe = probe_media(self.samples.team_voice, track_id="team")
        audio, _ = normalize_audio(probe, self.path("norm"), "team")
        assert audio is not None
        span = read_wav_span(audio.path, 1000, 1256)
        self.assertEqual(span.shape, (2, 256))
        self.assertEqual(audio_sample_count(audio.path), 105600)


class ColorPolicyTests(unittest.TestCase):
    def setUp(self) -> None:
        self.sources = {
            "cam_main": ColorMetadata(1, 1, 1, 1),
            "cam_alt": ColorMetadata(5, 5, 5, 1),
        }

    def test_first_policy_prefers_declared_order(self) -> None:
        decision = resolve_color(
            "cam_main", self.sources, ColorPolicy(mode="first"), preferred=("cam_main",)
        )
        self.assertEqual(decision.output, ColorMetadata(1, 1, 1, 1))
        self.assertEqual(decision.decided_by, "policy:first:cam_main")
        self.assertTrue(decision.conflicts)

    def test_track_policy(self) -> None:
        decision = resolve_color(
            "cam_main", self.sources, ColorPolicy(mode="track", track="cam_alt")
        )
        self.assertEqual(decision.output, ColorMetadata(5, 5, 5, 1))
        self.assertEqual(decision.decided_by, "policy:track:cam_alt")

    def test_explicit_policy(self) -> None:
        decision = resolve_color(
            "cam_main",
            self.sources,
            ColorPolicy(mode="explicit", values=ColorMetadata(9, 14, 9, 1)),
        )
        self.assertEqual(decision.output.primaries, 9)
        self.assertEqual(decision.decided_by, "policy:explicit")

    def test_unspecified_fallback(self) -> None:
        decision = resolve_color(
            "cam_main", {"cam_main": ColorMetadata()}, ColorPolicy(mode="first")
        )
        self.assertEqual(decision.decided_by, "fallback:bt709")

    def test_conflicts_recorded(self) -> None:
        decision = resolve_color("cam_main", self.sources, ColorPolicy())
        self.assertEqual(len(decision.conflicts), 1)
        self.assertIn("cam_alt", decision.conflicts[0])

    def test_policy_parse_from_json(self) -> None:
        policy = ColorPolicy.parse(
            {"mode": "explicit", "values": {"color_primaries": "bt709", "color_range": "tv"}}
        )
        self.assertEqual(policy.mode, "explicit")
        assert policy.values is not None
        self.assertEqual(policy.values.primaries, 1)
        self.assertEqual(policy.values.range, 1)


if __name__ == "__main__":  # pragma: no cover
    unittest.main()
