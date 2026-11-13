"""Read-only mmap access to the custom raw evidence image."""

from __future__ import annotations

import mmap
from dataclasses import dataclass
from pathlib import Path

from .format import BLOCK_SIZE, Partition, SuperBlock, parse_superblock


class ImageError(ValueError):
    """Base class for image access errors."""


class ImageBoundsError(ImageError):
    """Requested bytes fall outside the underlying file."""


@dataclass(frozen=True)
class AssetLocation:
    asset: str
    physical_offset: int
    physical_length: int
    logical_offset: int
    content_length: int
    truncated: bool = False


@dataclass(frozen=True)
class BlockInfo:
    block: int
    physical_offset: int
    expected_length: int
    actual_length: int
    asset: str
    logical_offset: int
    truncated: bool


class ImageIO:
    def __init__(self, path: str | Path, file_obj) -> None:
        self.path = Path(path)
        self._file = file_obj
        self.size = self.path.stat().st_size
        header_data = self._map_range(0, min(self.size, 1024 * 1024))
        self.superblock = parse_superblock(header_data)
        self._assets = {part.name: part for part in self.superblock.partitions}

    @classmethod
    def open(cls, path: str | Path) -> "ImageIO":
        image_path = Path(path)
        file_obj = open(image_path, "rb")
        try:
            if image_path.stat().st_size == 0:
                raise ImageError(f"empty image: {image_path}")
            return cls(image_path, file_obj)
        except Exception:
            file_obj.close()
            raise

    def __enter__(self) -> "ImageIO":
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        self.close()

    def close(self) -> None:
        if not self._file.closed:
            self._file.close()

    def _map_range(self, offset: int, length: int) -> bytes:
        if length <= 0:
            return b""
        alignment = mmap.ALLOCATIONGRANULARITY
        aligned_offset = offset - (offset % alignment)
        prefix = offset - aligned_offset
        mapping_length = prefix + length
        mapping = mmap.mmap(
            self._file.fileno(),
            mapping_length,
            access=mmap.ACCESS_READ,
            offset=aligned_offset,
        )
        try:
            return bytes(mapping[prefix : prefix + length])
        finally:
            mapping.close()

    @property
    def declared_size(self) -> int:
        return self.superblock.total_blocks * self.superblock.block_size

    @property
    def truncated(self) -> bool:
        return self.size < self.declared_size

    def asset(self, name: str) -> Partition:
        try:
            return self._assets[name]
        except KeyError as exc:
            raise ImageError(f"asset not present: {name}") from exc

    def find_asset(self, physical_offset: int, length: int = 1) -> Partition | None:
        best: Partition | None = None
        best_overlap = 0
        end = physical_offset + max(length, 1)
        for partition in self.superblock.partitions:
            part_start = partition.physical_offset
            part_end = part_start + partition.physical_length
            overlap = max(0, min(end, part_end) - max(physical_offset, part_start))
            if overlap > best_overlap:
                best = partition
                best_overlap = overlap
        return best

    def locate(self, physical_offset: int, length: int = 1) -> AssetLocation:
        if physical_offset < 0:
            raise ImageBoundsError("negative physical offset")
        partition = self.find_asset(physical_offset, length)
        if partition is None:
            return AssetLocation(
                asset="UNALLOCATED",
                physical_offset=physical_offset,
                physical_length=length,
                logical_offset=physical_offset,
                content_length=0,
                truncated=physical_offset >= self.size,
            )
        relative = physical_offset - partition.physical_offset
        return AssetLocation(
            asset=partition.name,
            physical_offset=physical_offset,
            physical_length=length,
            logical_offset=partition.logical_start + relative,
            content_length=partition.length,
            truncated=physical_offset >= self.size,
        )

    def read_physical(
        self,
        offset: int,
        length: int,
        *,
        zero_fill: bool = False,
    ) -> bytes:
        if offset < 0 or length < 0:
            raise ImageBoundsError("physical offset and length must be non-negative")
        if offset >= self.size:
            if zero_fill:
                return b"\0" * length
            raise ImageBoundsError(
                f"read at {offset} is beyond image size {self.size}"
            )
        available = min(length, self.size - offset)
        data = self._map_range(offset, available)
        if available < length:
            if not zero_fill:
                raise ImageBoundsError(
                    f"read at {offset}+{length} exceeds image size {self.size}"
                )
            data += b"\0" * (length - available)
        return data

    def read_asset(
        self,
        name: str,
        offset: int = 0,
        length: int | None = None,
        *,
        zero_fill: bool = False,
    ) -> bytes:
        partition = self.asset(name)
        if offset < 0:
            raise ImageBoundsError("negative asset offset")
        if length is None:
            length = max(0, partition.length - offset)
        if offset + length > partition.physical_length:
            raise ImageBoundsError(
                f"asset {name} read extends beyond its declared blocks"
            )
        return self.read_physical(
            partition.physical_offset + offset, length, zero_fill=zero_fill
        )

    def block_info(self, block: int, expected_length: int | None = None) -> BlockInfo:
        if block < 0 or block >= self.superblock.total_blocks:
            raise ImageBoundsError(f"block {block} is outside the declared image")
        physical_offset = block * self.superblock.block_size
        if expected_length is None:
            expected_length = self.superblock.block_size
        actual_length = max(0, min(expected_length, self.size - physical_offset))
        location = self.locate(physical_offset, expected_length)
        return BlockInfo(
            block=block,
            physical_offset=physical_offset,
            expected_length=expected_length,
            actual_length=actual_length,
            asset=location.asset,
            logical_offset=location.logical_offset,
            truncated=actual_length < expected_length,
        )

    def read_block(self, block: int) -> bytes:
        info = self.block_info(block, self.superblock.block_size)
        return self.read_physical(info.physical_offset, info.actual_length)

    def iter_blocks(self):
        for block in range(self.superblock.total_blocks):
            info = self.block_info(block, self.superblock.block_size)
            yield info, self.read_physical(
                info.physical_offset, info.actual_length
            )
