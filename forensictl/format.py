"""Binary layout shared by the image generator and forensic parsers."""

from __future__ import annotations

import struct
from dataclasses import dataclass
from datetime import datetime, timezone


BLOCK_SIZE = 4 * 1024 * 1024
DEFAULT_TOTAL_BLOCKS = 64
MAGIC = b"FORNSIMG"
FORMAT_VERSION = 1
PARTITION_TABLE_OFFSET = 4096
PARTITION_ENTRY_SIZE = 64
HEADER_SIZE = 64
GENESIS_HASH = "0" * 64

HEADER_STRUCT = struct.Struct("<8sIIIIIIQ")
ENTRY_STRUCT = struct.Struct("<16sIIQQI20x")

ASSET_SQLITE_MAIN = "SQLITE_MAIN"
ASSET_SQLITE_WAL = "SQLITE_WAL"
ASSET_FILE_RECORDS = "FILE_RECORDS"
ASSET_NTP_LOG = "NTP_LOG"
ASSET_TZ_RULES = "TZ_RULES"
ASSET_MAINTENANCE = "MAINTENANCE"


@dataclass(frozen=True)
class Partition:
    name: str
    start_block: int
    block_count: int
    length: int
    logical_start: int = 0
    flags: int = 0

    @property
    def physical_offset(self) -> int:
        return self.start_block * BLOCK_SIZE

    @property
    def physical_length(self) -> int:
        return self.block_count * BLOCK_SIZE

    @property
    def end_block(self) -> int:
        return self.start_block + self.block_count


@dataclass(frozen=True)
class SuperBlock:
    version: int
    block_size: int
    total_blocks: int
    partition_count: int
    partition_entry_size: int
    flags: int
    acquired_unix_100ns: int
    partitions: tuple[Partition, ...]

    @property
    def acquired_at(self) -> datetime:
        seconds, remainder = divmod(self.acquired_unix_100ns, 10_000_000)
        return datetime.fromtimestamp(seconds, timezone.utc).replace(
            microsecond=remainder // 10
        )

    def partition(self, name: str) -> Partition:
        for partition in self.partitions:
            if partition.name == name:
                return partition
        raise KeyError(f"partition not found: {name}")


def unix_100ns(value: datetime) -> int:
    if value.tzinfo is None:
        value = value.replace(tzinfo=timezone.utc)
    utc = value.astimezone(timezone.utc)
    delta = utc - datetime(1970, 1, 1, tzinfo=timezone.utc)
    return (delta.days * 86_400 + delta.seconds) * 10_000_000 + delta.microseconds * 10


def from_unix_100ns(value: int) -> datetime:
    seconds, remainder = divmod(value, 10_000_000)
    return datetime.fromtimestamp(seconds, timezone.utc).replace(
        microsecond=remainder // 10
    )


def frame_superblock(superblock: SuperBlock) -> bytes:
    header = HEADER_STRUCT.pack(
        MAGIC,
        superblock.version,
        superblock.block_size,
        superblock.total_blocks,
        superblock.partition_count,
        superblock.partition_entry_size,
        superblock.flags,
        superblock.acquired_unix_100ns,
    )
    entries = []
    for partition in superblock.partitions:
        entries.append(
            ENTRY_STRUCT.pack(
                partition.name.encode("ascii"),
                partition.start_block,
                partition.block_count,
                partition.length,
                partition.logical_start,
                partition.flags,
            )
        )
    return header + b"\0" * (PARTITION_TABLE_OFFSET - len(header)) + b"".join(entries)


def parse_superblock(data: bytes | bytearray | memoryview) -> SuperBlock:
    if len(data) < HEADER_SIZE:
        raise ValueError("image is too short for the forensic superblock")
    magic, version, block_size, total_blocks, count, entry_size, flags, acquired = (
        HEADER_STRUCT.unpack_from(data, 0)
    )
    if magic != MAGIC:
        raise ValueError(f"bad image magic: {magic!r}")
    if version != FORMAT_VERSION:
        raise ValueError(f"unsupported image version: {version}")
    if block_size < 4096 or block_size != BLOCK_SIZE:
        raise ValueError(f"unsupported block size: {block_size}")
    if total_blocks < 1:
        raise ValueError("image declares no blocks")
    if entry_size < ENTRY_STRUCT.size:
        raise ValueError(f"partition entry is too small: {entry_size}")
    table_end = PARTITION_TABLE_OFFSET + count * entry_size
    if table_end > len(data):
        raise ValueError("partition table extends beyond available image data")

    partitions = []
    for index in range(count):
        raw = data[
            PARTITION_TABLE_OFFSET + index * entry_size :
            PARTITION_TABLE_OFFSET + index * entry_size + entry_size
        ]
        name, start, block_count, length, logical_start, part_flags = (
            ENTRY_STRUCT.unpack_from(raw, 0)
        )
        name_text = name.decode("ascii", errors="replace").rstrip("\0")
        if not name_text:
            raise ValueError(f"partition {index} has an empty name")
        if block_count < 1:
            raise ValueError(f"partition {name_text} declares no blocks")
        if start + block_count > total_blocks:
            raise ValueError(f"partition {name_text} extends beyond the image")
        if length > block_count * block_size:
            raise ValueError(f"partition {name_text} content exceeds its blocks")
        partitions.append(
            Partition(
                name=name_text,
                start_block=start,
                block_count=block_count,
                length=length,
                logical_start=logical_start,
                flags=part_flags,
            )
        )
    return SuperBlock(
        version=version,
        block_size=block_size,
        total_blocks=total_blocks,
        partition_count=count,
        partition_entry_size=entry_size,
        flags=flags,
        acquired_unix_100ns=acquired,
        partitions=tuple(partitions),
    )
