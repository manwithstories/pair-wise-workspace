from __future__ import annotations

import io
import os
import tempfile
import unittest
from pathlib import Path

from PIL import Image

from mediaidx.canonical import (
    MediaFamily,
    PathValidationError,
    chunked_sha256,
    sniff_kind,
    validate_media_path,
)


def png_bytes(color: tuple[int, int, int] = (10, 20, 30)) -> bytes:
    buffer = io.BytesIO()
    Image.new("RGB", (16, 16), color).save(buffer, format="PNG")
    return buffer.getvalue()


class CanonicalTests(unittest.TestCase):
    def test_magic_beats_filename_and_identifies_families(self) -> None:
        self.assertEqual(sniff_kind(png_bytes()).family, MediaFamily.IMAGE)
        self.assertEqual(sniff_kind(png_bytes()).format, "png")
        self.assertIsNone(sniff_kind(b"not media bytes at all"))
        self.assertEqual(
            sniff_kind(b"\x00\x00\x00\x18ftypisom\x00\x00\x02\x00isomiso2").format,
            "mp4",
        )
        self.assertEqual(sniff_kind(b"RIFF\x00\x00\x00\x00WAVEfmt ").format, "wav")
        self.assertEqual(sniff_kind(b"fLaC\x00\x00\x00\x00").family, MediaFamily.AUDIO)
        self.assertEqual(sniff_kind(b"OggS\x00\x00\x00\x00").format, "ogg")
        self.assertEqual(sniff_kind(b"\x1aE\xdf\xa3").format, "mkv")

    def test_path_validation_rejects_symlink_and_escape(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / "corpus"
            root.mkdir()
            target = root / "image.mov"
            target.write_bytes(png_bytes())
            resolved, relative = validate_media_path(target, root)
            self.assertEqual(relative, "image.mov")
            self.assertFalse(resolved.is_symlink())

            outside = Path(temporary) / "outside.png"
            outside.write_bytes(png_bytes())
            link = root / "link.png"
            if hasattr(os, "symlink"):
                os.symlink(outside, link)
                with self.assertRaises(PathValidationError):
                    validate_media_path(link, root)

    def test_chunk_hash_changes_when_size_and_mtime_do_not(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "asset.bin"
            path.write_bytes(b"A" * 4096)
            original_hash = chunked_sha256(path, chunk_size=700)
            original_stat = path.stat()
            path.write_bytes(b"B" * 4096)
            os.utime(path, ns=(original_stat.st_atime_ns, original_stat.st_mtime_ns))
            changed_stat = path.stat()
            self.assertEqual(changed_stat.st_size, original_stat.st_size)
            self.assertEqual(changed_stat.st_mtime_ns, original_stat.st_mtime_ns)
            self.assertNotEqual(chunked_sha256(path, chunk_size=700), original_hash)


if __name__ == "__main__":
    unittest.main()
