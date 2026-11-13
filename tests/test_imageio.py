from __future__ import annotations

from pathlib import Path

import pytest

from forensictl.format import ASSET_SQLITE_MAIN, BLOCK_SIZE
from forensictl.imageio import ImageBoundsError, ImageIO


def test_asset_slices_bounds_and_sparse_holes(image_path: Path) -> None:
    with ImageIO.open(image_path) as image:
        partition = image.superblock.partition(ASSET_SQLITE_MAIN)
        assert partition.length > 100
        assert image.read_asset(ASSET_SQLITE_MAIN, 0, 16) == b"SQLite format 3\0"
        with pytest.raises(ImageBoundsError):
            image.read_physical(image.size, 1)
        hole_offset = (image.superblock.total_blocks - 1) * BLOCK_SIZE
        assert image.read_physical(hole_offset, 4096) == b"\0" * 4096
        assert image.read_physical(image.size, 1024, zero_fill=True) == b"\0" * 1024
