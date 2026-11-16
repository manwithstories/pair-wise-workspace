"""Deterministic per-track sample payloads for generated fixtures."""

from __future__ import annotations

import hashlib


#: Minimal, structurally valid decoder configurations so fixtures can omit
#: CodecPrivate without producing unreadable sample entries.
DEFAULT_CODEC_PRIVATE = {
    # AVC: configurationVersion 1, AVCProfileIndication 0x64 (High), level 3.0,
    # one SPS/PPS pair with placeholder bytes.
    "V_MPEG4/ISO/AVC": bytes.fromhex("010064001fffe1000567640001ffe1000568ee3c80"),
    # HEVC: configurationVersion 1 with no arrays (valid empty hvcC).
    "V_MPEGH/ISO/HEVC": bytes.fromhex("01016000000000000000"),
    # AAC: AAC-LC, 48 kHz, stereo AudioSpecificConfig.
    "A_AAC": bytes.fromhex("1190"),
}


def default_codec_private(codec_id: str) -> bytes:
    return DEFAULT_CODEC_PRIVATE.get(codec_id, b"")


def sample_bytes(track_number: int, index: int, codec_id: str, size: int) -> bytes:
    """A repeatable pseudo-payload for one sample (no external media needed)."""
    seed = f"{track_number}:{index}:{codec_id}".encode("ascii")
    digest = hashlib.sha256(seed).digest()
    return (digest * ((size // len(digest)) + 1))[:size]
