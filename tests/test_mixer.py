"""集成层测试:块引擎、配置解析、端到端离线/流式一致性与错误链路。

覆盖:任意块大小位级一致、偏移/尾部补零、故障媒体错误码、
best-effort 跳过、输出失败、verify 容差。
"""

import json
import tempfile
import unittest
from pathlib import Path

import numpy as np
import soundfile as sf

from mixer.config import load_config
from mixer.offline import run_mix
from mixer.sources import FileSource, MemorySource, PrefetchSource
from mixer.stream import BlockEngine
from mixer.types import (
    AutomationPoint,
    ConfigError,
    ErrorPolicy,
    MediaError,
    ProjectConfig,
    TrackSpec,
)



def _engine_output(config, sources, block_frames):
    engine = BlockEngine(config, sources)
    result = engine.run(block_frames)
    out = np.concatenate([mb.buffer for mb in result.blocks], axis=0)
    engine.close()
    return out, result


class EngineTest(unittest.TestCase):
    def setUp(self):
        self.tracks = (
            TrackSpec(name="a", path="a.wav",
                      automation=(AutomationPoint(0, 1.0),
                                  AutomationPoint(10, 2.0))),
            TrackSpec(name="b", path="b.wav", offset=10, gain=0.5),
        )
        self.cfg = ProjectConfig(
            sample_rate=48000, duration_frames=100, tracks=self.tracks
        )
        self.a = np.arange(50, dtype=np.float64)
        self.b = np.ones(30, dtype=np.float64) * 10.0

    def test_block_size_bit_exact(self):
        ref, _ = _engine_output(
            self.cfg,
            {"a": MemorySource(self.a, 48000), "b": MemorySource(self.b, 48000)},
            1,
        )
        for bs in (7, 13, 32, 100, 128):
            out, _ = _engine_output(
                self.cfg,
                {"a": MemorySource(self.a, 48000), "b": MemorySource(self.b, 48000)},
                bs,
            )
            np.testing.assert_array_equal(out, ref)

    def test_offset_gain_and_tail_padding(self):
        out, _ = _engine_output(
            self.cfg,
            {"a": MemorySource(self.a, 48000), "b": MemorySource(self.b, 48000)},
            7,
        )
        self.assertEqual(out.shape, (100, 2))
        self.assertAlmostEqual(out[5, 0], 5 * 1.5)           # 自动化插值
        self.assertAlmostEqual(out[10, 0], 10 * 2.0 + 5.0)   # b 轨进入
        self.assertAlmostEqual(out[40, 0], 40 * 2.0)         # b 已结束
        np.testing.assert_array_equal(out[50:], 0.0)         # 尾部补零
        np.testing.assert_array_equal(out[:, 0], out[:, 1])  # mono 复制

    def test_resampled_track_length_and_silence_after(self):
        data = np.ones(100, dtype=np.float64)
        cfg = ProjectConfig(
            sample_rate=48000,
            duration_frames=120,
            tracks=(TrackSpec(name="c", path="c.wav"),),
        )
        out, result = _engine_output(cfg, {"c": MemorySource(data, 44100)}, 16)
        expected = int(np.ceil(100 * 48000 / 44100))
        self.assertEqual(result.track_stats[0].project_frames, expected)
        np.testing.assert_array_equal(out[expected:], 0.0)

    def test_prefetch_preserves_samples(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "stereo.wav"
            data = np.stack([np.linspace(-0.5, 0.5, 2000),
                             np.linspace(0.5, -0.5, 2000)], axis=1)
            sf.write(str(p), data, 44100, subtype="FLOAT")
            source = PrefetchSource(FileSource(p))
            chunks = []
            while True:
                part = source.read(333)
                if part.shape[0] == 0:
                    break
                chunks.append(part)
            source.close()
            got = np.concatenate(chunks)
            np.testing.assert_allclose(got, data, atol=1e-7)


class ConfigTest(unittest.TestCase):
    def test_missing_field_rejected(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "c.json"
            p.write_text(json.dumps({"sample_rate": 48000}))
            with self.assertRaises(ConfigError):
                load_config(p)

    def test_bad_sample_rate_and_policy(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "c.json"
            p.write_text(json.dumps({
                "sample_rate": 0, "duration_frames": 10,
                "tracks": [{"name": "a", "path": "a.wav"}],
            }))
            with self.assertRaises(ConfigError):
                load_config(p)
            p.write_text(json.dumps({
                "sample_rate": 48000, "duration_frames": 10,
                "error_policy": "nope",
                "tracks": [{"name": "a", "path": "a.wav"}],
            }))
            with self.assertRaises(ConfigError):
                load_config(p)

    def test_routes_parsed(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "c.json"
            p.write_text(json.dumps({
                "sample_rate": 48000, "duration_frames": 10,
                "tracks": [{
                    "name": "a", "path": "a.wav",
                    "routes": {"0": "L", "1": "R", "2": "L+R"},
                }],
            }))
            cfg = load_config(p)
            self.assertEqual(cfg.tracks[0].routes, {0: "L", 1: "R", 2: "L+R"})

    def test_duplicate_track_names_rejected(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "c.json"
            p.write_text(json.dumps({
                "sample_rate": 48000, "duration_frames": 10,
                "tracks": [
                    {"name": "a", "path": "a.wav"},
                    {"name": "a", "path": "b.wav"},
                ],
            }))
            with self.assertRaises(ConfigError):
                load_config(p)


class MediaErrorTest(unittest.TestCase):
    def test_empty_and_corrupt_wav(self):
        with tempfile.TemporaryDirectory() as d:
            empty = Path(d) / "e.wav"
            empty.write_bytes(b"")
            with self.assertRaises(MediaError) as ctx:
                FileSource(empty)
            self.assertEqual(ctx.exception.details["reason"], "empty_file")
            corrupt = Path(d) / "c.wav"
            corrupt.write_bytes(b"RIFF0000WAVEgarbage")
            with self.assertRaises(MediaError) as ctx:
                FileSource(corrupt)
            self.assertEqual(ctx.exception.details["reason"], "corrupt_wav")

    def test_missing_file(self):
        with self.assertRaises(MediaError) as ctx:
            FileSource("/nonexistent/path/xyz.wav")
        self.assertEqual(ctx.exception.details["reason"], "not_found")


class EndToEndTest(unittest.TestCase):
    def test_offline_stream_bit_exact_and_report(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            rate = 48000
            mono = np.sin(np.linspace(0, 20, 1000))
            stereo = np.stack([np.linspace(0, 1, 2000),
                               np.linspace(1, 0, 2000)], axis=1) * 0.4
            sf.write(str(root / "m.wav"), mono, 44100, subtype="FLOAT")
            sf.write(str(root / "s.wav"), stereo, rate, subtype="FLOAT")
            config = ProjectConfig(
                sample_rate=rate,
                duration_frames=3000,
                tracks=(
                    TrackSpec(name="m", path="m.wav", offset=0),
                    TrackSpec(name="s", path="s.wav", offset=500, gain=0.5),
                ),
            )
            cfg_path = root / "p.json"
            cfg_path.write_text(json.dumps({
                "sample_rate": rate, "duration_frames": 3000,
                "tracks": [
                    {"name": "m", "path": "m.wav", "offset": 0},
                    {"name": "s", "path": "s.wav", "offset": 500, "gain": 0.5},
                ],
            }))
            _, stats_off = run_mix(config, cfg_path, root / "o.wav",
                                   root / "o.json", "offline", block_frames=4096)
            _, stats_st = run_mix(config, cfg_path, root / "s.wav",
                                  root / "s.json", "stream", block_frames=37)
            with sf.SoundFile(str(root / "o.wav")) as f:
                off = f.read(dtype="float64", always_2d=True)
            with sf.SoundFile(str(root / "s.wav")) as f:
                stm = f.read(dtype="float64", always_2d=True)
            np.testing.assert_array_equal(off, stm)
            self.assertEqual(stats_off.frames, 3000)
            self.assertEqual(stats_off.channels, 2)
            self.assertTrue(np.isfinite(stats_off.peak_dbfs))

    def test_best_effort_skips_bad_track(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            sf.write(str(root / "good.wav"), np.ones(100), 48000, subtype="FLOAT")
            (root / "bad.wav").write_bytes(b"")
            cfg_path = root / "p.json"
            cfg_path.write_text(json.dumps({
                "sample_rate": 48000, "duration_frames": 200,
                "error_policy": ErrorPolicy.BEST_EFFORT.value,
                "tracks": [
                    {"name": "good", "path": "good.wav"},
                    {"name": "bad", "path": "bad.wav"},
                ],
            }))
            cfg = load_config(cfg_path)
            _, stats = run_mix(cfg, cfg_path, root / "o.wav",
                               root / "o.json", "offline")
            self.assertEqual(len(stats.skipped_tracks), 1)
            self.assertEqual(stats.skipped_tracks[0].reason, "empty_file")

    def test_fail_fast_raises_media_error(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            (root / "bad.wav").write_bytes(b"")
            cfg_path = root / "p.json"
            cfg_path.write_text(json.dumps({
                "sample_rate": 48000, "duration_frames": 200,
                "tracks": [{"name": "bad", "path": "bad.wav"}],
            }))
            cfg = load_config(cfg_path)
            with self.assertRaises(MediaError):
                run_mix(cfg, cfg_path, root / "o.wav",
                        root / "o.json", "offline")


if __name__ == "__main__":
    unittest.main()
