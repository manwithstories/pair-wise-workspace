"""Parser for the image's 100-nanosecond file metadata table."""

from __future__ import annotations

import struct
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone

from .events import SOURCE_PRIORITY, TimelineEvent
from .format import ASSET_FILE_RECORDS, from_unix_100ns
from .imageio import ImageIO


FILE_TABLE_MAGIC = b"FILETAB1"
FILE_TABLE_HEADER = struct.Struct("<8sIII4x")
FILE_RECORD = struct.Struct("<Q48sqqqqI36x")
NAIVE_EPOCH = datetime(1970, 1, 1)


class FileMetaError(ValueError):
    """Raised when the file metadata table is malformed."""


@dataclass(frozen=True)
class FileRecord:
    rowid: int
    entity_key: str
    created_utc_100ns: int
    created_local_100ns: int
    modified_utc_100ns: int
    modified_local_100ns: int
    flags: int

    @property
    def created_utc(self) -> datetime:
        return from_unix_100ns(self.created_utc_100ns)

    @property
    def modified_utc(self) -> datetime:
        return from_unix_100ns(self.modified_utc_100ns)

    @property
    def created_local(self) -> datetime:
        return _from_naive_100ns(self.created_local_100ns)

    @property
    def modified_local(self) -> datetime:
        return _from_naive_100ns(self.modified_local_100ns)


@dataclass
class FileMetaResult:
    records: list[FileRecord]
    events: list[TimelineEvent]
    anomalies: list[TimelineEvent] = field(default_factory=list)
    warnings: list[str] = field(default_factory=list)


def _from_naive_100ns(value: int) -> datetime:
    seconds, remainder = divmod(value, 10_000_000)
    return NAIVE_EPOCH + timedelta(seconds=seconds, microseconds=remainder // 10)


def _iso_utc(value: datetime) -> str:
    return value.astimezone(timezone.utc).isoformat(timespec="microseconds")


def _raw_record(record: FileRecord) -> dict:
    return {
        "created_utc_100ns": record.created_utc_100ns,
        "created_local_100ns": record.created_local_100ns,
        "modified_utc_100ns": record.modified_utc_100ns,
        "modified_local_100ns": record.modified_local_100ns,
        "created_utc": _iso_utc(record.created_utc),
        "modified_utc": _iso_utc(record.modified_utc),
        "created_local": record.created_local.isoformat(timespec="microseconds"),
        "modified_local": record.modified_local.isoformat(timespec="microseconds"),
    }


def load_file_records(image: ImageIO) -> list[FileRecord]:
    raw = image.read_asset(ASSET_FILE_RECORDS, zero_fill=True)
    if len(raw) < FILE_TABLE_HEADER.size:
        raise FileMetaError("file record table header is truncated")
    magic, version, count, record_size = FILE_TABLE_HEADER.unpack_from(raw, 0)
    if magic != FILE_TABLE_MAGIC:
        raise FileMetaError(f"bad file record table magic: {magic!r}")
    if version != 1:
        raise FileMetaError(f"unsupported file record version: {version}")
    if record_size < FILE_RECORD.size:
        raise FileMetaError(f"file record size is too small: {record_size}")
    needed = FILE_TABLE_HEADER.size + count * record_size
    if needed > len(raw):
        raise FileMetaError(
            f"file record table is truncated: need={needed} available={len(raw)}"
        )
    records = []
    for index in range(count):
        offset = FILE_TABLE_HEADER.size + index * record_size
        rowid, key_raw, created_utc, created_local, modified_utc, modified_local, flags = (
            FILE_RECORD.unpack_from(raw, offset)
        )
        key = key_raw.rstrip(b"\0").decode("utf-8", "replace")
        if not key:
            raise FileMetaError(f"file record {index} has an empty entity key")
        records.append(
            FileRecord(
                rowid=rowid,
                entity_key=key,
                created_utc_100ns=created_utc,
                created_local_100ns=created_local,
                modified_utc_100ns=modified_utc,
                modified_local_100ns=modified_local,
                flags=flags,
            )
        )
    return records


def parse_filemeta(image: ImageIO) -> FileMetaResult:
    records = load_file_records(image)
    acquired = image.superblock.acquired_at
    events: list[TimelineEvent] = []
    anomalies: list[TimelineEvent] = []
    for record in records:
        raw = _raw_record(record)
        events.append(
            TimelineEvent(
                event_id=f"file:{record.rowid}:created",
                utc=record.created_utc,
                entity_key=record.entity_key,
                event_type="FILE_CREATED",
                description=f"file created: {record.entity_key}",
                sources=("file_record",),
                source_priority=SOURCE_PRIORITY["file_record"],
                sequence=record.rowid * 10,
                raw=raw,
            )
        )
        events.append(
            TimelineEvent(
                event_id=f"file:{record.rowid}:modified",
                utc=record.modified_utc,
                entity_key=record.entity_key,
                event_type="FILE_MODIFIED",
                description=f"file modified: {record.entity_key}",
                sources=("file_record",),
                source_priority=SOURCE_PRIORITY["file_record"],
                sequence=record.rowid * 10 + 1,
                raw=raw,
            )
        )
        reasons = []
        if record.modified_utc < record.created_utc:
            reasons.append("modified_utc_before_created_utc")
        if record.modified_local < record.created_local:
            reasons.append("modified_local_before_created_local")
        if record.modified_utc > acquired:
            reasons.append("modified_utc_after_acquisition")
        if record.created_utc > acquired:
            reasons.append("created_utc_after_acquisition")
        if reasons:
            anomaly_raw = dict(raw)
            anomaly_raw["reasons"] = reasons
            anomaly_raw["acquired_utc"] = _iso_utc(acquired)
            anomaly = TimelineEvent(
                event_id=f"file:{record.rowid}:anomaly",
                utc=record.modified_utc,
                entity_key=record.entity_key,
                event_type="METADATA_ANOMALY",
                description="file timestamp anomaly: " + ",".join(reasons),
                sources=("metadata",),
                source_priority=SOURCE_PRIORITY["metadata"],
                sequence=record.rowid * 10 + 2,
                raw=anomaly_raw,
                anomalous=True,
            )
            anomalies.append(anomaly)
            events.append(anomaly)
    return FileMetaResult(records=records, events=events, anomalies=anomalies)
