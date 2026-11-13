"""Offline SQLite page and WAL recovery without opening the source image."""

from __future__ import annotations

import json
import sqlite3
import struct
from dataclasses import dataclass, field
from pathlib import Path

from .format import ASSET_SQLITE_MAIN, ASSET_SQLITE_WAL
from .imageio import ImageIO


MARKER = b"EVENTROW|"
WAL_MAGIC_LITTLE = 0x377F0682
WAL_MAGIC_BIG = 0x377F0683


class CarveError(ValueError):
    """Raised when the embedded SQLite assets cannot be interpreted."""


@dataclass(frozen=True)
class CarvedRow:
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
    origins: tuple[str, ...] = ()
    recovered: bool = False

    def as_dict(self) -> dict:
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
            "origins": list(self.origins),
            "recovered": self.recovered,
        }


@dataclass
class CarveResult:
    rows: list[CarvedRow]
    warnings: list[str] = field(default_factory=list)
    freelist_pages: int = 0
    wal_committed_frames: int = 0
    wal_discarded_frames: int = 0
    live_rows: int = 0
    recovered_rows: int = 0


def _read_varint(data: bytes, offset: int) -> tuple[int, int]:
    value = 0
    for index in range(9):
        if offset + index >= len(data):
            raise CarveError("truncated SQLite varint")
        byte = data[offset + index]
        if index == 8:
            value = (value << 8) | byte
            return value, offset + 9
        value = (value << 7) | (byte & 0x7F)
        if not byte & 0x80:
            return value, offset + index + 1
    raise CarveError("invalid SQLite varint")


def _signed(value: int, bits: int) -> int:
    sign = 1 << (bits - 1)
    return (value & (sign - 1)) - (value & sign)


def _parse_record(payload: bytes) -> list[object]:
    header_size, position = _read_varint(payload, 0)
    if header_size > len(payload):
        raise CarveError("SQLite record header exceeds payload")
    serial_types = []
    while position < header_size:
        serial, position = _read_varint(payload, position)
        serial_types.append(serial)
    body_offset = header_size
    values: list[object] = []
    for serial in serial_types:
        if serial == 0:
            values.append(None)
        elif serial in (1, 2, 3, 4, 5, 6):
            width = {1: 1, 2: 2, 3: 3, 4: 4, 5: 6, 6: 8}[serial]
            if body_offset + width > len(payload):
                raise CarveError("truncated SQLite integer")
            raw = int.from_bytes(payload[body_offset : body_offset + width], "big")
            values.append(_signed(raw, width * 8))
            body_offset += width
        elif serial == 7:
            if body_offset + 8 > len(payload):
                raise CarveError("truncated SQLite float")
            values.append(struct.unpack_from(">d", payload, body_offset)[0])
            body_offset += 8
        elif serial in (8, 9):
            values.append(0 if serial == 8 else 1)
        elif serial >= 12:
            length = (serial - 12) // 2 if serial % 2 == 0 else (serial - 13) // 2
            if body_offset + length > len(payload):
                raise CarveError("truncated SQLite text or blob")
            raw = payload[body_offset : body_offset + length]
            values.append(bytes(raw) if serial % 2 == 0 else bytes(raw).decode("utf-8", "replace"))
            body_offset += length
        else:
            raise CarveError(f"unsupported SQLite serial type {serial}")
    return values


def _wal_checksum(data: bytes, seed: tuple[int, int], little_endian: bool) -> tuple[int, int]:
    if len(data) % 8:
        raise CarveError("WAL checksum input is not a multiple of 8 bytes")
    s1, s2 = seed
    for offset in range(0, len(data), 8):
        endian = "little" if little_endian else "big"
        x0 = int.from_bytes(data[offset : offset + 4], endian)
        x1 = int.from_bytes(data[offset + 4 : offset + 8], endian)
        s1 = (s1 + x0 + s2) & 0xFFFFFFFF
        s2 = (s2 + x1 + s1) & 0xFFFFFFFF
    return s1, s2


@dataclass(frozen=True)
class _Page:
    number: int
    data: bytes
    origin: str


@dataclass(frozen=True)
class _Frame:
    page_number: int
    db_size: int
    data: bytes
    index: int


class _PageStore:
    def __init__(self, page_size: int, reserved: int, pages: dict[int, _Page], db_size: int) -> None:
        self.page_size = page_size
        self.reserved = reserved
        self.usable_size = page_size - reserved
        self.pages = pages
        self.db_size = db_size

    def page(self, number: int) -> _Page:
        page = self.pages.get(number)
        if page is None:
            raise CarveError(f"SQLite page {number} is not available")
        if len(page.data) != self.page_size:
            raise CarveError(f"SQLite page {number} has the wrong size")
        return page


class _BTreeReader:
    def __init__(self, store: _PageStore, warnings: list[str]) -> None:
        self.store = store
        self.warnings = warnings
        self.visited: set[int] = set()
        self.overflow_pages: set[int] = set()

    @property
    def page_size(self) -> int:
        return self.store.page_size

    @property
    def usable_size(self) -> int:
        return self.store.usable_size

    def rows(self, root_page: int) -> list[tuple[int, bytes, str]]:
        collected: list[tuple[int, bytes, str]] = []
        self._walk(root_page, collected, 0)
        return collected

    def _walk(self, number: int, collected: list[tuple[int, bytes, str]], depth: int) -> None:
        if depth > 64:
            self.warnings.append(f"page tree depth limit reached at page {number}")
            return
        if number in self.visited:
            self.warnings.append(f"cycle or duplicate page reference at {number}")
            return
        self.visited.add(number)
        try:
            page = self.store.page(number)
        except CarveError as exc:
            self.warnings.append(str(exc))
            return
        header = 100 if number == 1 else 0
        data = page.data
        if header + 8 > len(data):
            self.warnings.append(f"page {number} has no b-tree header")
            return
        page_type = data[header]
        cell_count = int.from_bytes(data[header + 3 : header + 5], "big")
        if page_type == 13:
            header_end = header + 8
            if header_end + cell_count * 2 > len(data):
                self.warnings.append(f"page {number} has an invalid cell array")
                return
            for index in range(cell_count):
                pointer = int.from_bytes(
                    data[header_end + index * 2 : header_end + index * 2 + 2], "big"
                )
                try:
                    rowid, payload = self._leaf_cell(page, pointer)
                    collected.append((rowid, payload, page.origin))
                except CarveError as exc:
                    self.warnings.append(f"page {number} cell {index}: {exc}")
        elif page_type == 5:
            header_end = header + 12
            if header_end + cell_count * 2 + 4 > len(data):
                self.warnings.append(f"page {number} has an invalid interior cell array")
                return
            children = []
            for index in range(cell_count):
                pointer = int.from_bytes(
                    data[header_end + index * 2 : header_end + index * 2 + 2], "big"
                )
                if pointer + 4 > len(data):
                    self.warnings.append(f"page {number} has an invalid child pointer")
                    continue
                children.append(int.from_bytes(data[pointer : pointer + 4], "big"))
            children.append(int.from_bytes(data[header + 8 : header + 12], "big"))
            for child in children:
                self._walk(child, collected, depth + 1)
        else:
            self.warnings.append(f"page {number} has unsupported b-tree type {page_type}")

    def _leaf_cell(self, page: _Page, cell_offset: int) -> tuple[int, bytes]:
        data = page.data
        if cell_offset < 0 or cell_offset >= len(data):
            raise CarveError("cell offset is outside the page")
        payload_size, position = _read_varint(data, cell_offset)
        rowid, position = _read_varint(data, position)
        if payload_size > self.usable_size * self.store.db_size + 1_000_000:
            raise CarveError("implausible payload size")
        max_local = self.usable_size - 35
        min_local = ((self.usable_size - 12) * 32 // 255) - 23
        if payload_size <= max_local:
            local_size = payload_size
        else:
            local_size = min_local + ((payload_size - min_local) % (self.usable_size - 4))
            if local_size > max_local:
                local_size = min_local
        if position + local_size > len(data):
            raise CarveError("local payload extends beyond the page")
        payload = bytearray(data[position : position + local_size])
        remaining = payload_size - local_size
        if remaining:
            if position + local_size + 4 > len(data):
                raise CarveError("overflow pointer is missing")
            overflow = int.from_bytes(data[position + local_size : position + local_size + 4], "big")
            seen: set[int] = set()
            while remaining:
                if overflow in seen:
                    raise CarveError("overflow page cycle")
                seen.add(overflow)
                self.overflow_pages.add(overflow)
                overflow_page = self.store.page(overflow)
                chunk = overflow_page.data[4 : self.usable_size]
                take = min(remaining, len(chunk))
                payload.extend(chunk[:take])
                remaining -= take
                overflow = int.from_bytes(overflow_page.data[0:4], "big")
                if remaining and overflow == 0:
                    raise CarveError("overflow chain ended early")
        return rowid, bytes(payload[:payload_size])


class _Carver:
    def __init__(self, image: ImageIO) -> None:
        self.image = image
        self.warnings: list[str] = []
        self.main = image.read_asset(ASSET_SQLITE_MAIN, zero_fill=True)
        self.wal = image.read_asset(ASSET_SQLITE_WAL, zero_fill=True)
        self.page_size, self.reserved = self._read_page_geometry()
        self.main_page_count = self._read_main_page_count()
        self.frames, self.discarded = self._read_wal()

    def _read_page_geometry(self) -> tuple[int, int]:
        if len(self.main) < 100 or self.main[:16] != b"SQLite format 3\0":
            raise CarveError("embedded SQLite main database header is missing")
        raw_page_size = int.from_bytes(self.main[16:18], "big")
        page_size = 65536 if raw_page_size == 1 else raw_page_size
        if page_size < 512 or page_size & (page_size - 1):
            raise CarveError(f"invalid SQLite page size: {page_size}")
        reserved = self.main[20]
        if reserved >= page_size:
            raise CarveError("invalid SQLite reserved byte count")
        return page_size, reserved

    def _read_main_page_count(self) -> int:
        count = int.from_bytes(self.main[28:32], "big")
        if count == 0:
            count = len(self.main) // self.page_size
        available = min(len(self.main) // self.page_size, count)
        if available < 1:
            raise CarveError("embedded SQLite main database has no pages")
        return available

    def _read_wal(self) -> tuple[list[_Frame], int]:
        if len(self.wal) < 32 or self.wal[:4] not in (
            (WAL_MAGIC_LITTLE).to_bytes(4, "big"),
            (WAL_MAGIC_BIG).to_bytes(4, "big"),
        ):
            return [], 0
        magic = int.from_bytes(self.wal[:4], "big")
        little = magic == WAL_MAGIC_LITTLE
        wal_page_size = int.from_bytes(self.wal[8:12], "big")
        if wal_page_size != self.page_size:
            self.warnings.append("WAL page size differs from the main database")
            return [], 0
        stored_header = (
            int.from_bytes(self.wal[24:28], "big"),
            int.from_bytes(self.wal[28:32], "big"),
        )
        calculated_header = _wal_checksum(self.wal[:24], (0, 0), little)
        if calculated_header != stored_header:
            self.warnings.append("WAL header checksum mismatch; WAL ignored")
            return [], 0
        salt = self.wal[16:24]
        frames: list[_Frame] = []
        seed = stored_header
        position = 32
        trailing = 0
        index = 0
        while position + 24 + self.page_size <= len(self.wal):
            frame_header = self.wal[position : position + 24]
            if frame_header[8:16] != salt:
                self.warnings.append(f"WAL salt mismatch at frame {index}")
                trailing = 1
                break
            page_data = self.wal[position + 24 : position + 24 + self.page_size]
            seed = _wal_checksum(frame_header[:8] + page_data, seed, little)
            stored = (
                int.from_bytes(frame_header[16:20], "big"),
                int.from_bytes(frame_header[20:24], "big"),
            )
            if seed != stored:
                self.warnings.append(f"WAL checksum mismatch at frame {index}")
                trailing = 1
                break
            frames.append(
                _Frame(
                    page_number=int.from_bytes(frame_header[0:4], "big"),
                    db_size=int.from_bytes(frame_header[4:8], "big"),
                    data=page_data,
                    index=index,
                )
            )
            index += 1
            position += 24 + self.page_size
        if position < len(self.wal) and not trailing:
            trailing = 1
        last_commit = -1
        for item in frames:
            if item.db_size:
                last_commit = item.index
        discarded = (len(frames) - last_commit - 1) + trailing
        if last_commit < 0:
            return [], discarded
        return frames[: last_commit + 1], discarded

    def _page_map(self) -> tuple[dict[int, _Page], int]:
        pages: dict[int, _Page] = {}
        for number in range(1, self.main_page_count + 1):
            start = (number - 1) * self.page_size
            pages[number] = _Page(number, self.main[start : start + self.page_size], "main")
        db_size = self.main_page_count
        for frame in self.frames:
            if frame.page_number < 1:
                self.warnings.append(f"invalid WAL page number {frame.page_number}")
                continue
            pages[frame.page_number] = _Page(
                frame.page_number, frame.data, f"wal:{frame.index}"
            )
            db_size = max(db_size, frame.page_number)
            if frame.db_size:
                db_size = frame.db_size
        return pages, db_size

    def run(self) -> CarveResult:
        pages, db_size = self._page_map()
        if db_size > self.main_page_count + self.page_size:
            self.warnings.append(f"implausible WAL database size {db_size}")
        store = _PageStore(self.page_size, self.reserved, pages, db_size)
        reader = _BTreeReader(store, self.warnings)
        schema_rows = reader.rows(1)
        schema = {}
        for _, payload, _ in schema_rows:
            try:
                values = _parse_record(payload)
                if len(values) >= 5 and values[0] == "table":
                    schema[str(values[1])] = int(values[3])
            except (CarveError, TypeError, ValueError):
                continue
        root = schema.get("events")
        if root is None:
            raise CarveError("events table is absent from sqlite_master")

        live_payloads: list[tuple[int, bytes, str]] = reader.rows(root)
        live: dict[int, CarvedRow] = {}
        for rowid, payload, origin in live_payloads:
            try:
                record = self._row_from_record(rowid, payload, origin)
            except CarveError as exc:
                self.warnings.append(f"live row {rowid}: {exc}")
                continue
            live[rowid] = record

        salvage: dict[int, CarvedRow] = {}
        main_pages = {
            number: page
            for number, page in pages.items()
            if page.origin == "main" and number <= self.main_page_count
        }
        self._salvage_scan(main_pages, "main_residual", salvage)
        freelist = self._freelist_pages(store)
        freelist_images = {
            number: pages[number]
            for number in freelist
            if number in pages and pages[number].origin == "main"
        }
        self._salvage_scan(freelist_images, "freelist", salvage)
        wal_origins: dict[int, _Page] = {}
        for frame in self.frames:
            if frame.page_number in pages and pages[frame.page_number].origin == f"wal:{frame.index}":
                wal_origins[frame.page_number] = pages[frame.page_number]
        if wal_origins:
            self._salvage_scan(wal_origins, "wal", salvage)

        merged: dict[int, CarvedRow] = {}
        for rowid, row in live.items():
            if rowid in salvage:
                extra_origins = tuple(
                    sorted(set(row.origins) | set(salvage[rowid].origins))
                )
                merged[rowid] = CarvedRow(**{**row.as_dict(), "origins": extra_origins})
            else:
                merged[rowid] = row
        for rowid, row in salvage.items():
            if rowid not in merged:
                merged[rowid] = row
        ordered = [merged[rowid] for rowid in sorted(merged)]
        recovered = sum(1 for row in ordered if row.recovered)
        return CarveResult(
            rows=ordered,
            warnings=self.warnings,
            freelist_pages=len(freelist),
            wal_committed_frames=len(self.frames),
            wal_discarded_frames=self.discarded,
            live_rows=len(live),
            recovered_rows=recovered,
        )

    def _freelist_pages(self, store: _PageStore) -> set[int]:
        try:
            first_page = store.page(1).data
        except CarveError:
            return set()
        trunk = int.from_bytes(first_page[32:36], "big")
        count = int.from_bytes(first_page[36:40], "big")
        pages: set[int] = set()
        seen: set[int] = set()
        while trunk and trunk not in seen and trunk <= store.db_size:
            seen.add(trunk)
            try:
                data = store.page(trunk).data
            except CarveError:
                break
            pages.add(trunk)
            leaves = int.from_bytes(data[4:8], "big")
            if 8 + leaves * 4 > len(data):
                self.warnings.append(f"freelist trunk {trunk} has an invalid leaf count")
                break
            for index in range(leaves):
                leaf = int.from_bytes(data[8 + index * 4 : 12 + index * 4], "big")
                if 1 <= leaf <= store.db_size:
                    pages.add(leaf)
            trunk = int.from_bytes(data[0:4], "big")
        if count and len(pages) < count:
            self.warnings.append(
                f"freelist count reports {count} pages but {len(pages)} were reachable"
            )
        return pages

    def _salvage_scan(
        self,
        pages: dict[int, _Page],
        origin: str,
        salvage: dict[int, CarvedRow],
    ) -> None:
        decoder = json.JSONDecoder()
        for _, page in sorted(pages.items()):
            start = 0
            while True:
                marker = page.data.find(MARKER, start)
                if marker < 0:
                    break
                text = page.data[marker + len(MARKER) :].decode("utf-8", "ignore")
                try:
                    value, _ = decoder.raw_decode(text)
                except (json.JSONDecodeError, UnicodeError):
                    start = marker + len(MARKER)
                    continue
                if not isinstance(value, dict):
                    start = marker + len(MARKER)
                    continue
                try:
                    rowid = int(value["rowid"])
                    row = self._row_from_dict(
                        value,
                        origins=(origin,),
                        recovered=rowid not in {candidate.rowid for candidate in []},
                    )
                except (CarveError, KeyError, TypeError, ValueError):
                    start = marker + len(MARKER)
                    continue
                vague = salvage.get(rowid)
                if vague is None or row.sequence >= vague.sequence:
                    salvage[rowid] = CarvedRow(**{**row.as_dict(), "recovered": True})
                else:
                    salvage[rowid] = CarvedRow(
                        **{
                            **vague.as_dict(),
                            "origins": tuple(sorted(set(vague.origins) | {origin})),
                        }
                    )
                start = marker + len(MARKER)

    def _row_from_record(self, rowid: int, payload: bytes, origin: str) -> CarvedRow:
        values = _parse_record(payload)
        if len(values) < 10:
            raise CarveError("events record has too few columns")
        return CarvedRow(
            rowid=rowid,
            entity_key=str(values[1]),
            event_type=str(values[2]),
            description=str(values[3]),
            wall_time=str(values[4]),
            utc_time=str(values[5]) if values[5] is not None else None,
            tz_offset=str(values[6]) if values[6] is not None else None,
            source=str(values[7]),
            sequence=int(values[8]),
            payload=str(values[9]),
            origins=(origin,),
            recovered=False,
        )

    def _row_from_dict(
        self,
        value: dict,
        *,
        origins: tuple[str, ...],
        recovered: bool,
    ) -> CarvedRow:
        rowid = int(value["rowid"])
        return CarvedRow(
            rowid=rowid,
            entity_key=str(value["entity_key"]),
            event_type=str(value["event_type"]),
            description=str(value["description"]),
            wall_time=str(value["wall_time"]),
            utc_time=str(value["utc_time"]) if value.get("utc_time") is not None else None,
            tz_offset=str(value["tz_offset"]) if value.get("tz_offset") is not None else None,
            source=str(value["source"]),
            sequence=int(value["sequence"]),
            payload=str(value.get("payload", "")),
            origins=origins,
            recovered=recovered,
        )


def carve_sqlite(image: ImageIO) -> CarveResult:
    return _Carver(image).run()


def write_carved_database(result: CarveResult, path: str | Path) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    connection = sqlite3.connect(target)
    try:
        connection.execute("DROP TABLE IF EXISTS carved_events")
        connection.execute(
            """
            CREATE TABLE carved_events(
                rowid INTEGER PRIMARY KEY,
                entity_key TEXT NOT NULL,
                event_type TEXT NOT NULL,
                description TEXT NOT NULL,
                wall_time TEXT NOT NULL,
                utc_time TEXT,
                tz_offset TEXT,
                source TEXT NOT NULL,
                sequence INTEGER NOT NULL,
                payload TEXT NOT NULL,
                origins TEXT NOT NULL,
                recovered INTEGER NOT NULL
            )
            """
        )
        connection.executemany(
            "INSERT INTO carved_events VALUES(?,?,?,?,?,?,?,?,?,?,?,?)",
            [
                (
                    row.rowid,
                    row.entity_key,
                    row.event_type,
                    row.description,
                    row.wall_time,
                    row.utc_time,
                    row.tz_offset,
                    row.source,
                    row.sequence,
                    row.payload,
                    json.dumps(list(row.origins), sort_keys=True),
                    int(row.recovered),
                )
                for row in result.rows
            ],
        )
        connection.commit()
    finally:
        connection.close()


def load_carved_database(path: str | Path) -> list[CarvedRow]:
    connection = sqlite3.connect(f"file:{Path(path)}?mode=ro", uri=True)
    try:
        rows = connection.execute(
            """
            SELECT rowid, entity_key, event_type, description, wall_time, utc_time,
                   tz_offset, source, sequence, payload, origins, recovered
            FROM carved_events ORDER BY rowid
            """
        ).fetchall()
    finally:
        connection.close()
    return [
        CarvedRow(
            rowid=int(row[0]),
            entity_key=row[1],
            event_type=row[2],
            description=row[3],
            wall_time=row[4],
            utc_time=row[5],
            tz_offset=row[6],
            source=row[7],
            sequence=int(row[8]),
            payload=row[9],
            origins=tuple(json.loads(row[10])),
            recovered=bool(row[11]),
        )
        for row in rows
    ]
