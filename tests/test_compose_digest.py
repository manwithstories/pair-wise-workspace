import json
import tempfile
import unittest
from pathlib import Path

from tlrender.compose import HEIGHT, WIDTH, render_first_frame
from tlrender.digest import (
    build_records,
    frame_sha256,
    load_manifest,
    write_manifest,
    write_pngs,
)
from tlrender.tl_parse import Cue


def cue(cid="x", text="hello"):
    c = Cue(id=cid, track=1, start=0, end=1000, text=text,
            source="mem", line=1)
    c.final_start, c.final_end = 100, 1100
    return c


class ComposeDigestTests(unittest.TestCase):
    def test_frame_shape_and_png(self):
        frame = render_first_frame(cue())
        self.assertEqual(frame.rgb.shape, (HEIGHT, WIDTH, 3))
        self.assertTrue(frame.png_bytes.startswith(b"\x89PNG"))
        self.assertEqual(len(frame_sha256(frame)), 16)

    def test_deterministic_png_bytes(self):
        first = render_first_frame(cue()).png_bytes
        second = render_first_frame(cue()).png_bytes
        self.assertEqual(first, second)

    def test_different_text_different_hash(self):
        h1 = frame_sha256(render_first_frame(cue("a", "one")))
        h2 = frame_sha256(render_first_frame(cue("a", "two")))
        self.assertNotEqual(h1, h2)

    def test_manifest_roundtrip_and_stability(self):
        with tempfile.TemporaryDirectory() as tmp:
            c = cue()
            frame = render_first_frame(c)
            digest = frame_sha256(frame)
            write_pngs([frame], tmp)
            records = build_records([c], {c.id: digest})
            summary = {"total": 1, "resolved": 0, "dropped": 0,
                       "frames": 1, "hash_mismatch": 0}
            write_manifest(tmp, records, summary, None)
            payload1 = Path(tmp, "manifest.json").read_bytes()
            write_manifest(tmp, records, summary, None)
            payload2 = Path(tmp, "manifest.json").read_bytes()
            self.assertEqual(payload1, payload2)
            loaded = load_manifest(tmp)
            self.assertEqual(loaded["summary"]["total"], 1)
            self.assertEqual(loaded["cues"][0]["sha256"], digest)
            json.loads(payload1)
            self.assertTrue((Path(tmp) / "x.png").exists())

    def test_timestamp_only_difference(self):
        with tempfile.TemporaryDirectory() as tmp:
            records = build_records([cue()], {"x": "0" * 16})
            summary = {"total": 1}
            write_manifest(tmp, records, summary, None)
            no_ts = Path(tmp, "manifest.json").read_text()
            write_manifest(tmp, records, summary, "2026-01-01T00:00:00+00:00")
            with_ts = Path(tmp, "manifest.json").read_text()
            self.assertNotEqual(no_ts, with_ts)
            self.assertIn("generated_at", with_ts)
            self.assertNotIn("generated_at", no_ts)


if __name__ == "__main__":
    unittest.main()
