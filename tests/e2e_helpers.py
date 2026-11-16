"""Shared end-to-end helpers: in-process CLI runner and synthetic broken files."""

from __future__ import annotations

import contextlib
import io
import json
import tempfile
import unittest
from pathlib import Path

from src import cli


class TempDirTestCase(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory(prefix="mkv2mp4-e2e-")
        self.work = Path(self._tmp.name)

    def tearDown(self) -> None:
        self._tmp.cleanup()


def run_cli(argv: list[str]) -> tuple[int, dict]:
    """Run the CLI in-process and capture its JSON envelope."""
    buffer = io.StringIO()
    code = 0
    with contextlib.redirect_stdout(buffer), contextlib.redirect_stderr(io.StringIO()):
        try:
            cli.main(argv)
        except SystemExit as exc:  # the CLI always exits through _emit
            code = int(exc.code or 0)
    text = buffer.getvalue()
    payload = json.loads(text) if text.strip() else {}
    return code, payload


def _build_laced_document() -> bytes:
    """A minimal Matroska whose single video block carries two fixed-laced frames."""
    from src import ebml_schema as ids
    from src._fixture_ebml import binary_elem, elem, string_elem, uint_elem
    from src.ebml_schema import encode_vint

    entry = (
        uint_elem(ids.TRACK_NUMBER, 1)
        + uint_elem(ids.TRACK_UID, 1)
        + uint_elem(ids.TRACK_TYPE, 1)
        + string_elem(ids.CODEC_ID, "V_MPEG4/ISO/AVC")
        + binary_elem(ids.CODEC_PRIVATE, bytes.fromhex("010064001fffe1000567640001ffe1000568ee3c80"))
        + elem(ids.VIDEO, uint_elem(ids.PIXEL_WIDTH, 64) + uint_elem(ids.PIXEL_HEIGHT, 64))
    )
    # SimpleBlock header: track 1, relative 0, flags with fixed lacing (0x04).
    frames = b"abcde" + b"fghij"
    block_payload = encode_vint(1) + (0).to_bytes(2, "big", signed=True) + b"\x04" + b"\x01" + frames
    cluster = elem(
        ids.CLUSTER,
        uint_elem(ids.TIMESTAMP, 0) + binary_elem(ids.SIMPLE_BLOCK, block_payload),
    )
    segment = elem(
        ids.SEGMENT,
        elem(ids.INFO, uint_elem(ids.TIMESTAMP_SCALE, 1_000_000))
        + elem(ids.TRACKS, elem(ids.TRACK_ENTRY, entry))
        + cluster,
    )
    header = elem(ids.EBML_HEADER, string_elem(ids.DOC_TYPE, "matroska"))
    return header + segment
