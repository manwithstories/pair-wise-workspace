"""Compositor behaviour: intervals, z-order, determinism, dirty regions."""

import json
import os
import shutil
import tempfile
import unittest

import numpy as np
from PIL import Image

from timeliner.compositor import Compositor
from timeliner.errors import AssetError, BlendModeError, TimelineError
from timeliner.model import load_timeline, validate


def _write_png(path, w, h, rgba):
    arr = np.zeros((h, w, 4), dtype=np.uint8)
    arr[..., 0], arr[..., 1], arr[..., 2], arr[..., 3] = rgba
    Image.fromarray(arr, "RGBA").save(path)


class CompositorFixture(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.dir = tempfile.mkdtemp(prefix="timeliner_test_")
        _write_png(os.path.join(cls.dir, "red.png"), 4, 4, (255, 0, 0, 255))
        _write_png(os.path.join(cls.dir, "blue.png"), 4, 4, (0, 0, 255, 255))
        _write_png(os.path.join(cls.dir, "half.png"), 4, 4, (255, 255, 255, 128))

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.dir, ignore_errors=True)

    def make_timeline(self, layers):
        return validate({
            "canvas": {"width": 16, "height": 16},
            "duration": 2.0,
            "layers": layers,
            "audio": [],
        })

    def layer(self, layer_id, asset, start=0.0, duration=2.0, blend="normal",
              keyframes=None):
        return {"id": layer_id, "asset": asset, "start": start,
                "duration": duration, "blend": blend,
                "anchor": [0.0, 0.0], "keyframes": keyframes or []}

    def render(self, timeline, frame, fps=8):
        return Compositor(timeline, self.dir).render_frame(frame, fps)

    def pixel(self, rgb, x, y, w=16):
        off = (y * w + x) * 3
        return tuple(rgb[off:off + 3])


class TestIntervals(CompositorFixture):
    def test_layer_outside_interval_not_composited(self):
        tl = self.make_timeline([
            self.layer("bg", "blue.png"),
            self.layer("fg", "red.png", start=0.5, duration=1.0),
        ])
        self.assertEqual(self.pixel(self.render(tl, 0), 0, 0), (0, 0, 255))
        self.assertEqual(self.pixel(self.render(tl, 8), 0, 0), (255, 0, 0))
        self.assertEqual(self.pixel(self.render(tl, 12), 0, 0), (0, 0, 255))

    def test_z_order_follows_layer_list(self):
        tl = self.make_timeline([
            self.layer("a", "red.png"),
            self.layer("b", "blue.png"),
        ])
        self.assertEqual(self.pixel(self.render(tl, 0), 0, 0), (0, 0, 255))

    def test_deterministic_repeated_render(self):
        tl = self.make_timeline([
            self.layer("a", "red.png"),
            self.layer("b", "half.png", blend="screen",
                       keyframes=[{"t": 0.0, "x": 1.0, "y": 1.0,
                                   "rotate": 30.0, "scale_x": 1.5,
                                   "scale_y": 1.5}]),
        ])
        comp = Compositor(tl, self.dir)
        first = comp.render_frame(3, 8)
        for _ in range(5):
            self.assertEqual(comp.render_frame(3, 8), first)

    def test_keyframed_motion_moves_layer(self):
        tl = self.make_timeline([
            self.layer("bg", "blue.png"),
            self.layer("fg", "red.png", keyframes=[
                {"t": 0.0, "x": 0.0, "y": 0.0},
                {"t": 2.0, "x": 12.0, "y": 0.0},
            ]),
        ])
        self.assertEqual(self.pixel(self.render(tl, 0), 0, 0), (255, 0, 0))
        self.assertEqual(self.pixel(self.render(tl, 16 - 1, fps=8), 13, 0),
                         (255, 0, 0))


class TestValidation(unittest.TestCase):
    def test_zero_duration_located(self):
        with self.assertRaises(TimelineError) as ctx:
            validate({"canvas": {"width": 8, "height": 8}, "duration": 1.0,
                      "layers": [{"id": "x", "asset": "a.png", "start": 0.0,
                                  "duration": 0}], "audio": []})
        self.assertIn("layers[0].duration", str(ctx.exception))

    def test_keyframe_out_of_range_located(self):
        with self.assertRaises(TimelineError) as ctx:
            validate({"canvas": {"width": 8, "height": 8}, "duration": 1.0,
                      "layers": [{"id": "x", "asset": "a.png", "start": 0.0,
                                  "duration": 1.0,
                                  "keyframes": [{"t": 5.0, "x": 1.0}]}],
                      "audio": []})
        self.assertIn("keyframes[0].t", str(ctx.exception))

    def test_unknown_blend_mode_exit_code(self):
        with self.assertRaises(BlendModeError) as ctx:
            validate({"canvas": {"width": 8, "height": 8}, "duration": 1.0,
                      "layers": [{"id": "x", "asset": "a.png", "start": 0.0,
                                  "duration": 1.0, "blend": "bogus"}],
                      "audio": []})
        self.assertEqual(ctx.exception.exit_code, 6)


class TestAssets(CompositorFixture):
    def test_missing_asset(self):
        tl = self.make_timeline([self.layer("a", "ghost.png")])
        with self.assertRaises(AssetError) as ctx:
            Compositor(tl, self.dir)
        self.assertEqual(ctx.exception.exit_code, 3)

    def test_corrupt_asset(self):
        path = os.path.join(self.dir, "broken.png")
        with open(path, "wb") as fh:
            fh.write(b"definitely not a png")
        tl = self.make_timeline([self.layer("a", "broken.png")])
        with self.assertRaises(AssetError):
            Compositor(tl, self.dir)

    def test_size_mismatch(self):
        layer = self.layer("a", "red.png")
        layer["width"] = 99
        tl = self.make_timeline([layer])
        with self.assertRaises(AssetError) as ctx:
            Compositor(tl, self.dir)
        self.assertIn("does not match declared", str(ctx.exception))


if __name__ == "__main__":
    unittest.main()
