#!/usr/bin/env python3
"""Generate a deterministic self-contained forensic image fixture."""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import random
import sqlite3
import sys
import tempfile
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[1]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from forensictl.format import (  # noqa: E402
    ASSET_FILE_RECORDS,
    ASSET_MAINTENANCE,
    ASSET_NTP_LOG,
    ASSET_SQLITE_MAIN,
    ASSET_SQLITE_WAL,
    ASSET_TZ_RULES,
    BLOCK_SIZE,
    DEFAULT_TOTAL_BLOCKS,
    FORMAT_VERSION,
    GENESIS_HASH,
    PARTITION_ENTRY_SIZE,
    PARTITION_TABLE_OFFSET,
    Partition,
    SuperBlock,
    frame_superblock,
    unix_100ns,
)
from forensictl.filemeta import FILE_RECORD, FILE_TABLE_HEADER, FILE_TABLE_MAGIC  # noqa: E402
from forensictl.timebase import TimeZoneRules, Transition, build_zone  # noqa: E402


UTC = timezone.utc
ACQUISITION = datetime(2026, 3, 8, 22, 31, tzinfo=UTC)
ACTIVE_SEGMENTS = [
    (datetime(2026, 3, 7, 12, 0, tzinfo=UTC), datetime(2026, 3, 8, 0, 0, tzinfo=UTC)),
    (datetime(2026, 3, 8, 1, 0, tzinfo=UTC), datetime(2026, 3, 8, 6, 40, tzinfo=UTC)),
    (datetime(2026, 3, 8, 7, 10, tzinfo=UTC), datetime(2026, 3, 8, 9, 0, tzinfo=UTC)),
    (datetime(2026, 3, 8, 10, 0, tzinfo=UTC), datetime(2026, 3, 8, 14, 0, tzinfo=UTC)),
    (datetime(2026, 3, 8, 17, 0, tzinfo=UTC), datetime(2026, 3, 8, 22, 30, tzinfo=UTC)),
]
MAINTENANCE = [
    (datetime(2026, 3, 8, 14, 0, tzinfo=UTC), datetime(2026, 3, 8, 17, 0, tzinfo=UTC))
]
ANOMALY_RECORDS = {17: "inversion", 101: "future", 211: "inversion", 287: "future"}
NAIVE_EPOCH = datetime(1970, 1, 1)


def _rules() -> TimeZoneRules:
    return TimeZoneRules(
        zone_name="Forensic/Lab",
        base_offset=-5 * 3600,
        base_abbreviation="FST",
        transitions=(
            Transition(
                utc=datetime(2026, 3, 8, 7, 0, tzinfo=UTC),
                offset_before=-5 * 3600,
                offset_after=-4 * 3600,
                abbreviation_before="FST",
                abbreviation_after="FDT",
                is_dst_after=True,
            ),
            Transition(
                utc=datetime(2026, 11, 1, 6, 0, tzinfo=UTC),
                offset_before=-4 * 3600,
                offset_after=-5 * 3600,
                abbreviation_before="FDT",
                abbreviation_after="FST",
                is_dst_after=False,
            ),
        ),
    )


def _segment_counts(total: int) -> list[int]:
    durations = [(end - start).total_seconds() for start, end in ACTIVE_SEGMENTS]
    raw = [total * duration / sum(durations) for duration in durations]
    counts = [max(2, math.floor(value)) for value in raw]
    difference = total - sum(counts)
    while difference > 0:
        fractions = [raw[index] - math.floor(raw[index]) for index in range(len(raw))]
        index = max(range(len(raw)), key=lambda item: (fractions[item], -item))
        counts[index] += 1
        raw[index] += 1
        difference -= 1
    while difference < 0:
        index = max(
            (item for item in range(len(counts)) if counts[item] > 2),
            key=lambda item: counts[item],
        )
        counts[index] -= 1
        difference += 1
    return counts


def _base_times(total: int = 1280) -> list[datetime]:
    values: list[datetime] = []
    for (start, end), count in zip(ACTIVE_SEGMENTS, _segment_counts(total), strict=True):
        span = (end - start).total_seconds()
        for index in range(count):
            offset = span * index / (count - 1)
            values.append(start + timedelta(seconds=offset))
    return values


def _naive_100ns(value: datetime) -> int:
    delta = value - NAIVE_EPOCH
    return (delta.days * 86_400 + delta.seconds) * 10_000_000 + delta.microseconds * 10


def _local_naive(value: datetime, zone) -> datetime:
    return value.astimezone(zone).replace(tzinfo=None)


def _marker(value: dict) -> str:
    return "EVENTROW|" + json.dumps(value, separators=(",", ":"), ensure_ascii=False)


def _append_uncommitted_wal_frame(wal_bytes: bytes) -> bytes:
    from forensictl.sqlitecarve import WAL_MAGIC_LITTLE, _wal_checksum

    if len(wal_bytes) < 32 + 24 + 4096:
        return wal_bytes
    magic = int.from_bytes(wal_bytes[:4], "big")
    little = magic == WAL_MAGIC_LITTLE
    page_size = int.from_bytes(wal_bytes[8:12], "big")
    seed = (
        int.from_bytes(wal_bytes[24:28], "big"),
        int.from_bytes(wal_bytes[28:32], "big"),
    )
    frame_count = (len(wal_bytes) - 32) // (24 + page_size)
    last_offset = 32 + (frame_count - 1) * (24 + page_size)
    last_header = wal_bytes[last_offset : last_offset + 24]
    last_page = bytearray(
        wal_bytes[last_offset + 24 : last_offset + 24 + page_size]
    )
    for frame_index in range(frame_count):
        offset = 32 + frame_index * (24 + page_size)
        header = wal_bytes[offset : offset + 24]
        page = wal_bytes[offset + 24 : offset + 24 + page_size]
        seed = _wal_checksum(header[:8] + page, seed, little)
    ghost = {
        "rowid": 9999,
        "entity_key": "ghost-uncommitted",
        "event_type": "GHOST",
        "description": "must never be recovered",
        "wall_time": "2026-03-08T22:00:00",
        "utc_time": None,
        "tz_offset": None,
        "source": "application",
        "sequence": 999999,
        "payload": "",
    }
    ghost_payload = _marker(ghost).encode("utf-8")
    position = page_size - len(ghost_payload) - 8
    last_page[position : position + len(ghost_payload)] = ghost_payload
    page_number = int.from_bytes(last_header[0:4], "big")
    frame_header = page_number.to_bytes(4, "big") + (0).to_bytes(4, "big") + wal_bytes[16:24]
    checksum = _wal_checksum(frame_header[:8] + bytes(last_page), seed, little)
    return (
        wal_bytes
        + frame_header
        + checksum[0].to_bytes(4, "big")
        + checksum[1].to_bytes(4, "big")
        + bytes(last_page)
    )


def _normalize_wal(wal_bytes: bytes) -> bytes:
    from forensictl.sqlitecarve import WAL_MAGIC_BIG, WAL_MAGIC_LITTLE, _wal_checksum

    if len(wal_bytes) < 32:
        return wal_bytes
    magic = int.from_bytes(wal_bytes[:4], "big")
    if magic not in (WAL_MAGIC_LITTLE, WAL_MAGIC_BIG):
        return wal_bytes
    little = magic == WAL_MAGIC_LITTLE
    page_size = int.from_bytes(wal_bytes[8:12], "big")
    frame_count = (len(wal_bytes) - 32) // (24 + page_size)
    output = bytearray(wal_bytes[: 32 + frame_count * (24 + page_size)])
    output[16:24] = bytes.fromhex("123456789abcdef0")
    seed = _wal_checksum(bytes(output[:24]), (0, 0), little)
    output[24:28] = seed[0].to_bytes(4, "big")
    output[28:32] = seed[1].to_bytes(4, "big")
    for index in range(frame_count):
        offset = 32 + index * (24 + page_size)
        output[offset + 8 : offset + 16] = output[16:24]
        seed = _wal_checksum(
            bytes(output[offset : offset + 8])
            + bytes(output[offset + 24 : offset + 24 + page_size]),
            seed,
            little,
        )
        output[offset + 16 : offset + 20] = seed[0].to_bytes(4, "big")
        output[offset + 20 : offset + 24] = seed[1].to_bytes(4, "big")
    return bytes(output)


@dataclass(frozen=True)
class AppRowSpec:
    rowid: int
    entity_key: str
    event_type: str
    description: str
    wall_time: str
    utc_time: str | None
    tz_offset: str | None
    source: str
    sequence: int
    payload: str

    def as_record(self) -> dict:
        return {
            "rowid": self.rowid,
            "entity_key": self.entity_key,
            "event_type": self.event_type,
            "description": self.description,
            "wall_time": self.wall_time,
            "utc_time": self.utc_time,
            "tz_offset": self.tz_offset,
            "source": self.source,
            "sequence": self.sequence,
            "payload": self.payload,
        }


def _app_rows(
    base_times: list[datetime],
    record_times: list[tuple[datetime, datetime, str]],
    zone,
    seed: int,
) -> list[AppRowSpec]:
    rng = random.Random(seed)
    specs: list[AppRowSpec] = []
    for index in range(20, 40):
        rowid = index + 1
        _, modified, entity_key = record_times[index]
        value = modified + timedelta(milliseconds=500)
        local = _local_naive(value, zone)
        specs.append(
            AppRowSpec(
                rowid=rowid - 20,
                entity_key=entity_key,
                event_type="FILE_MODIFIED",
                description=f"application observed modification of {entity_key}",
                wall_time=local.isoformat(timespec="microseconds"),
                utc_time=value.isoformat(timespec="microseconds"),
                tz_offset=None,
                source="application",
                sequence=200_000 + rowid - 20,
                payload="",
            )
        )
    unique_times = base_times[600:]
    for offset, value in enumerate(unique_times):
        rowid = 21 + offset
        local_aware = value.astimezone(zone)
        local = local_aware.replace(tzinfo=None)
        wall_time = local.isoformat(timespec="microseconds")
        utc_time = None
        tz_offset = None
        if rowid % 11 == 0:
            utc_time = value.isoformat(timespec="microseconds")
        elif rowid % 7 == 0:
            offset_seconds = int(local_aware.utcoffset().total_seconds())
            sign = "+" if offset_seconds >= 0 else "-"
            absolute = abs(offset_seconds)
            wall_time = f"{wall_time}{sign}{absolute // 3600:02d}:{absolute % 3600 // 60:02d}"
        elif rowid % 13 == 0:
            offset_seconds = int(local_aware.utcoffset().total_seconds())
            sign = "+" if offset_seconds >= 0 else "-"
            absolute = abs(offset_seconds)
            tz_offset = f"{sign}{absolute // 3600:02d}:{absolute % 3600 // 60:02d}"
        event_types = ["APP_ACTIVITY", "AUTH", "NETWORK", "PROCESS"]
        event_type = event_types[rowid % len(event_types)]
        specs.append(
            AppRowSpec(
                rowid=rowid,
                entity_key=f"app-{rowid:04d}",
                event_type=event_type,
                description=rng.choice(
                    ["session opened", "authentication succeeded", "network connect", "process started"]
                ),
                wall_time=wall_time,
                utc_time=utc_time,
                tz_offset=tz_offset,
                source="application",
                sequence=100_000 + rowid,
                payload="",
            )
        )
    for spec in specs:
        payload = _marker(spec.as_record())
        if spec.rowid in (333, 444):
            payload += "|" + "X" * 6000
        specs[spec.rowid - 1] = AppRowSpec(**{**spec.as_record(), "payload": payload})
    return specs


def _build_sqlite(
    path: Path,
    specs: list[AppRowSpec],
    wal_path: Path,
) -> tuple[bytes, bytes]:
    if wal_path.exists():
        wal_path.unlink()
    connection = sqlite3.connect(path)
    try:
        connection.execute("PRAGMA page_size=4096")
        connection.execute("PRAGMA auto_vacuum=NONE")
        connection.execute("PRAGMA secure_delete=OFF")
        connection.execute("PRAGMA journal_mode=WAL")
        connection.execute("PRAGMA wal_autocheckpoint=0")
        connection.execute("PRAGMA synchronous=OFF")
        connection.execute(
            """
            CREATE TABLE events(
                id INTEGER PRIMARY KEY,
                entity_key TEXT NOT NULL,
                event_type TEXT NOT NULL,
                description TEXT NOT NULL,
                wall_time TEXT NOT NULL,
                utc_time TEXT,
                tz_offset TEXT,
                source TEXT NOT NULL,
                sequence INTEGER NOT NULL,
                payload TEXT NOT NULL
            )
            """
        )
        connection.execute("CREATE TABLE padding(id INTEGER PRIMARY KEY, body TEXT NOT NULL)")
        connection.executemany(
            "INSERT INTO events VALUES(?,?,?,?,?,?,?,?,?,?)",
            [
                (
                    spec.rowid,
                    spec.entity_key,
                    spec.event_type,
                    spec.description,
                    spec.wall_time,
                    spec.utc_time,
                    spec.tz_offset,
                    spec.source,
                    spec.sequence,
                    spec.payload,
                )
                for spec in specs
                if spec.rowid <= 690
            ],
        )
        padding_rows = []
        by_id = {spec.rowid: spec for spec in specs}
        for index in range(1, 81):
            if index == 1:
                body = _marker(by_id[7].as_record()) + "|" + "P" * 3000
            elif index == 2:
                body = _marker(by_id[99].as_record()) + "|" + "P" * 3000
            else:
                body = f"padding-{index}-" + "P" * 3300
            padding_rows.append((index, body))
        connection.executemany("INSERT INTO padding VALUES(?,?)", padding_rows)
        connection.commit()
        connection.execute("DELETE FROM padding")
        connection.execute("DELETE FROM events WHERE id IN (7,99)")
        connection.commit()
        connection.execute("PRAGMA wal_checkpoint(TRUNCATE)")
        connection.execute("DELETE FROM events WHERE id IN (233,456)")
        connection.executemany(
            "INSERT INTO events VALUES(?,?,?,?,?,?,?,?,?,?)",
            [
                (
                    spec.rowid,
                    spec.entity_key,
                    spec.event_type,
                    spec.description,
                    spec.wall_time,
                    spec.utc_time,
                    spec.tz_offset,
                    spec.source,
                    spec.sequence,
                    spec.payload,
                )
                for spec in specs
                if spec.rowid >= 691
            ],
        )
        connection.commit()
        main_bytes = path.read_bytes()
        wal_bytes = _append_uncommitted_wal_frame(
            _normalize_wal(wal_path.read_bytes() if wal_path.exists() else b"")
        )
        return main_bytes, wal_bytes
    finally:
        connection.close()


def _timezone_json(rules: TimeZoneRules) -> bytes:
    value = {
        "zone": rules.zone_name,
        "base_offset": rules.base_offset,
        "base_abbreviation": rules.base_abbreviation,
        "transitions": [
            {
                "utc": transition.utc.isoformat().replace("+00:00", "Z"),
                "offset_before": transition.offset_before,
                "offset_after": transition.offset_after,
                "abbreviation_before": transition.abbreviation_before,
                "abbreviation_after": transition.abbreviation_after,
                "is_dst_after": transition.is_dst_after,
            }
            for transition in rules.transitions
        ],
    }
    return json.dumps(value, indent=2, sort_keys=True).encode("utf-8")


def _ntp_jsonl(zone) -> bytes:
    samples = [
        (1, datetime(2026, 3, 7, 12, 0, tzinfo=UTC), "step", -3.0),
        (2, datetime(2026, 3, 8, 6, 0, tzinfo=UTC), "drift_start", -3.0),
        (3, datetime(2026, 3, 8, 6, 40, tzinfo=UTC), "drift_end", -6.0),
        (4, datetime(2026, 3, 8, 8, 0, tzinfo=UTC), "step", 1.2),
        (5, datetime(2026, 3, 8, 22, 0, tzinfo=UTC), "sample", 1.2),
    ]
    lines = []
    for sequence, reference, mode, offset_seconds in samples:
        observed = reference + timedelta(seconds=offset_seconds)
        observed_local = observed.astimezone(zone).replace(tzinfo=None)
        lines.append(
            json.dumps(
                {
                    "seq": sequence,
                    "reference_utc": reference.isoformat().replace("+00:00", "Z"),
                    "observed_local": observed_local.isoformat(timespec="microseconds"),
                    "mode": mode,
                    "offset_s": offset_seconds,
                },
                sort_keys=True,
            )
        )
    return ("\n".join(lines) + "\n").encode("utf-8")


def _file_records_table(
    record_times: list[tuple[datetime, datetime, str]],
    acquisition: datetime,
    zone,
) -> bytes:
    output = bytearray(FILE_TABLE_HEADER.pack(FILE_TABLE_MAGIC, 1, len(record_times), FILE_RECORD.size))
    for index, (created, modified, entity_key) in enumerate(record_times):
        if index in ANOMALY_RECORDS:
            if ANOMALY_RECORDS[index] == "inversion":
                modified = created - timedelta(seconds=120 if index == 17 else 30)
            elif index in (101, 287):
                modified = acquisition + timedelta(seconds=60 if index == 101 else 90)
        created_ticks = unix_100ns(created) + 37
        modified_ticks = unix_100ns(modified) + 73
        created_local = _naive_100ns(_local_naive(created, zone)) + 37
        modified_local = _naive_100ns(_local_naive(modified, zone)) + 73
        output.extend(
            FILE_RECORD.pack(
                index + 1,
                entity_key.encode("utf-8"),
                created_ticks,
                created_local,
                modified_ticks,
                modified_local,
                1 if index in ANOMALY_RECORDS else 0,
            )
        )
    return bytes(output)


def _write_image(
    image_path: Path,
    total_blocks: int,
    assets: dict[str, bytes],
    layout: list[tuple[str, int, int]],
    acquisition: datetime,
) -> dict[str, Partition]:
    image_path.write_bytes(b"")
    with image_path.open("r+b") as handle:
        handle.truncate(total_blocks * BLOCK_SIZE)
        partitions = []
        for name, start_block, length in layout:
            data = assets[name]
            block_count = max(1, math.ceil(len(data) / BLOCK_SIZE))
            partitions.append(
                Partition(
                    name=name,
                    start_block=start_block,
                    block_count=block_count,
                    length=length,
                )
            )
            handle.seek(start_block * BLOCK_SIZE)
            handle.write(data)
        superblock = SuperBlock(
            version=FORMAT_VERSION,
            block_size=BLOCK_SIZE,
            total_blocks=total_blocks,
            partition_count=len(partitions),
            partition_entry_size=PARTITION_ENTRY_SIZE,
            flags=1,
            acquired_unix_100ns=unix_100ns(acquisition),
            partitions=tuple(partitions),
        )
        handle.seek(0)
        handle.write(frame_superblock(superblock))
    return {partition.name: partition for partition in partitions}


def _write_manifest(image_path: Path, manifest_path: Path, total_blocks: int) -> list[dict]:
    entries = []
    previous = GENESIS_HASH
    with image_path.open("rb") as handle, manifest_path.open("w", encoding="utf-8") as manifest:
        for block in range(total_blocks):
            data = handle.read(BLOCK_SIZE)
            digest = hashlib.sha256(data).hexdigest()
            entry = {
                "block": block,
                "offset": block * BLOCK_SIZE,
                "length": BLOCK_SIZE,
                "sha256": digest,
                "prev_hash": previous,
            }
            entries.append(entry)
            manifest.write(json.dumps(entry, sort_keys=True, separators=(",", ":")) + "\n")
            previous = digest
    return entries


def _inject_rewrites(image_path: Path, targets: dict[str, int]) -> dict[str, int]:
    offsets = {}
    with image_path.open("r+b") as handle:
        for name, block in targets.items():
            offset = block * BLOCK_SIZE + BLOCK_SIZE - 1
            handle.seek(offset)
            original = handle.read(1)
            handle.seek(offset)
            handle.write(bytes([original[0] ^ 0x5A]))
            offsets[name] = offset
    return offsets


def generate(out_dir: Path, seed: int, total_blocks: int) -> dict:
    if total_blocks < 10:
        raise SystemExit("--blocks must be at least 10")
    out_dir.mkdir(parents=True, exist_ok=True)
    rules = _rules()
    zone = build_zone(rules)
    base_times = _base_times(1280)
    record_times = []
    for index in range(300):
        record_times.append(
            (base_times[index * 2], base_times[index * 2 + 1], f"asset-{index + 1:04d}.dat")
        )
    specs = _app_rows(base_times, record_times, zone, seed)
    image_path = out_dir / "evidence.img"
    manifest_path = out_dir / "manifest.jsonl"
    ground_truth_path = out_dir / "ground_truth.json"
    with tempfile.TemporaryDirectory(prefix=".fixture-", dir=out_dir) as temporary:
        db_path = Path(temporary) / "main.db"
        main_bytes, wal_bytes = _build_sqlite(db_path, specs, Path(temporary) / "main.db-wal")

    file_records = _file_records_table(record_times, ACQUISITION, zone)
    ntp_log = _ntp_jsonl(zone)
    timezone_rules = _timezone_json(rules)
    maintenance = json.dumps(
        {
            "windows": [
                {
                    "start": start.isoformat().replace("+00:00", "Z"),
                    "end": end.isoformat().replace("+00:00", "Z"),
                }
                for start, end in MAINTENANCE
            ]
        },
        sort_keys=True,
    ).encode("utf-8")

    assets = {
        ASSET_SQLITE_MAIN: main_bytes,
        ASSET_SQLITE_WAL: wal_bytes,
        ASSET_FILE_RECORDS: file_records + b"\0" * (BLOCK_SIZE - len(file_records)),
        ASSET_NTP_LOG: ntp_log + b"\0" * (BLOCK_SIZE - len(ntp_log)),
        ASSET_TZ_RULES: timezone_rules + b"\0" * (BLOCK_SIZE - len(timezone_rules)),
        ASSET_MAINTENANCE: maintenance + b"\0" * (BLOCK_SIZE - len(maintenance)),
    }
    layout: list[tuple[str, int, int]] = []
    cursor = 1
    for name in (
        ASSET_SQLITE_MAIN,
        ASSET_SQLITE_WAL,
        ASSET_FILE_RECORDS,
        ASSET_NTP_LOG,
        ASSET_TZ_RULES,
        ASSET_MAINTENANCE,
    ):
        data = assets[name]
        blocks = max(1, math.ceil(len(data) / BLOCK_SIZE))
        if cursor + blocks > total_blocks:
            raise SystemExit(
                f"image needs at least {cursor + blocks} blocks for {name}"
            )
        layout.append((name, cursor, len(data)))
        cursor += blocks

    partitions = _write_image(image_path, total_blocks, assets, layout, ACQUISITION)
    manifest = _write_manifest(image_path, manifest_path, total_blocks)
    tamper_blocks = {
        ASSET_FILE_RECORDS: partitions[ASSET_FILE_RECORDS].start_block,
        ASSET_NTP_LOG: partitions[ASSET_NTP_LOG].start_block,
        ASSET_MAINTENANCE: partitions[ASSET_MAINTENANCE].start_block,
    }
    tamper_offsets = _inject_rewrites(image_path, tamper_blocks)
    ground_truth = {
        "seed": seed,
        "block_size": BLOCK_SIZE,
        "total_blocks": total_blocks,
        "image_size": total_blocks * BLOCK_SIZE,
        "acquired_utc": ACQUISITION.isoformat().replace("+00:00", "Z"),
        "manifest_blocks": len(manifest),
        "expected_events": 1284,
        "expected_hash_mismatches": 3,
        "expected_missing_ranges": 2,
        "expected_carved_rows": 700,
        "expected_recovered_rows": 4,
        "expected_file_records": 300,
        "expected_anomalies": 4,
        "expected_bucket_seconds": 300,
        "partitions": {
            name: {
                "start_block": partition.start_block,
                "block_count": partition.block_count,
                "length": partition.length,
            }
            for name, partition in partitions.items()
        },
        "tampered": [
            {
                "asset": name,
                "block": block,
                "offset": block * BLOCK_SIZE,
                "changed_byte_offset": tamper_offsets[name],
            }
            for name, block in tamper_blocks.items()
        ],
        "deleted_rowids": [7, 99, 233, 456],
        "uncommitted_wal_rowid": 9999,
        "active_segments": [
            {
                "start": start.isoformat().replace("+00:00", "Z"),
                "end": end.isoformat().replace("+00:00", "Z"),
            }
            for start, end in ACTIVE_SEGMENTS
        ],
        "maintenance_windows": [
            {
                "start": start.isoformat().replace("+00:00", "Z"),
                "end": end.isoformat().replace("+00:00", "Z"),
            }
            for start, end in MAINTENANCE
        ],
    }
    ground_truth_path.write_text(
        json.dumps(ground_truth, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    work_dir = out_dir / "work"
    work_dir.mkdir(exist_ok=True)
    return ground_truth


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", default="evidence", help="output directory")
    parser.add_argument("--seed", type=int, default=20260308, help="fixture seed")
    parser.add_argument(
        "--blocks",
        type=int,
        default=DEFAULT_TOTAL_BLOCKS,
        help="image size in 4MiB blocks (default: 64, 256MiB)",
    )
    args = parser.parse_args()
    truth = generate(Path(args.out), args.seed, args.blocks)
    print(
        f"image={Path(args.out) / 'evidence.img'} size={truth['image_size']} "
        f"blocks={truth['total_blocks']} tampered={truth['expected_hash_mismatches']}"
    )
    print(f"manifest={Path(args.out) / 'manifest.jsonl'}")
    print(f"ground_truth={Path(args.out) / 'ground_truth.json'}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
