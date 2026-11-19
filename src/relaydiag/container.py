"""Deterministic on-disk container used by the fixture generator and probe.

The relay diagnosis pipeline needs the *source byte offset* of every packet so
that damage can be mapped back to a reviewable byte interval.  A generic mux
hides those offsets, so the fixture generator writes a small explicit
container instead.  The format is local-only and fully deterministic.

Layout (all multi-byte integers are big-endian)::

    magic  b"RLYD"
    u8     version
    u32    header_length
    bytes  JSON header (tracks, time bases, anchors, edit points, damage plan)
    repeated packets:
        u32    packet_length
        bytes  packet body = JSON record followed by an optional payload
    u32    sentinel 0

A packet record is JSON with ``track``, ``pts`` (ticks), ``dts``,
``keyframe``, ``pts_wrapped``, ``payload_len`` and optional ``kind``.  The raw
media payload immediately follows the JSON bytes; probes can therefore report
exact byte ranges for both record and payload.
"""

from __future__ import annotations

import json
import struct
from dataclasses import dataclass

MAGIC = b"RLYD"
VERSION = 1
SENTINEL = 0
HEADER_LEN_FMT = ">I"
PACKET_LEN_FMT = ">I"


@dataclass(slots=True)
class PacketRecord:
    track: str
    pts: int
    dts: int
    keyframe: bool = False
    pts_wrapped: bool = False
    kind: str = "media"
    payload: bytes = b""

    def to_json(self) -> bytes:
        return json.dumps(
            {
                "track": self.track,
                "pts": self.pts,
                "dts": self.dts,
                "keyframe": self.keyframe,
                "pts_wrapped": self.pts_wrapped,
                "kind": self.kind,
                "payload_len": len(self.payload),
            },
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")

    @classmethod
    def from_json(cls, raw: bytes, payload: bytes) -> "PacketRecord":
        data = json.loads(raw.decode("utf-8"))
        return cls(
            track=data["track"],
            pts=data["pts"],
            dts=data["dts"],
            keyframe=data.get("keyframe", False),
            pts_wrapped=data.get("pts_wrapped", False),
            kind=data.get("kind", "media"),
            payload=payload,
        )


def encode_packet(record: PacketRecord) -> bytes:
    body = record.to_json()
    return (
        struct.pack(PACKET_LEN_FMT, len(body) + len(record.payload))
        + body
        + record.payload
    )


def write_container(path: str, header: dict, records: list[PacketRecord]) -> None:
    header_bytes = json.dumps(header, sort_keys=True, separators=(",", ":")).encode("utf-8")
    with open(path, "wb") as fh:
        fh.write(MAGIC)
        fh.write(bytes([VERSION]))
        fh.write(struct.pack(HEADER_LEN_FMT, len(header_bytes)))
        fh.write(header_bytes)
        for record in records:
            fh.write(encode_packet(record))
        fh.write(struct.pack(PACKET_LEN_FMT, SENTINEL))


@dataclass(slots=True)
class ParsedPacket:
    record: PacketRecord
    #: Byte range of the whole on-disk packet (length prefix included).
    packet_range: tuple[int, int]
    #: Byte range of the media payload alone.
    payload_range: tuple[int, int]
    raw_json: bytes


def read_header(fh) -> dict:
    magic = fh.read(4)
    if magic != MAGIC:
        raise ValueError("not a relaydiag container: bad magic")
    version = fh.read(1)
    if version != bytes([VERSION]):
        raise ValueError(f"unsupported container version: {version!r}")
    (hlen,) = struct.unpack(HEADER_LEN_FMT, fh.read(4))
    return json.loads(fh.read(hlen).decode("utf-8"))


def iter_packets(path: str):
    """Yield :class:`ParsedPacket` objects with exact source byte ranges."""
    with open(path, "rb") as fh:
        header = read_header(fh)
        while True:
            prefix_pos = fh.tell()
            raw_len = fh.read(4)
            if len(raw_len) < 4:
                raise EOFError("truncated packet length prefix")
            (length,) = struct.unpack(PACKET_LEN_FMT, raw_len)
            if length == SENTINEL:
                break
            body_start = fh.tell()
            body = fh.read(length)
            if len(body) < length:
                raise EOFError("truncated packet body")
            # The JSON record is the prefix up to its closing brace; payload
            # follows immediately and its length is recorded in the JSON.
            brace = body.index(b"}") + 1
            raw_json, payload = body[:brace], body[brace:]
            record = PacketRecord.from_json(raw_json, payload)
            if record.payload and len(payload) != len(record.payload):
                raise ValueError("payload length mismatch")
            payload_range = (body_start + brace, body_start + length)
            yield header, ParsedPacket(
                record,
                (prefix_pos, body_start + length),
                payload_range,
                raw_json,
            )
