"""Streaming block verification against a hash-chain manifest."""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass, field
from pathlib import Path

from .format import GENESIS_HASH
from .imageio import ImageIO


class ManifestError(ValueError):
    """Raised when the manifest cannot be interpreted."""


@dataclass(frozen=True)
class ManifestEntry:
    block: int
    offset: int
    length: int
    sha256: str
    prev_hash: str


@dataclass(frozen=True)
class Mismatch:
    block: int
    offset: int
    asset: str
    logical_offset: int
    expected_sha256: str
    actual_sha256: str
    reason: str


@dataclass
class VerifyResult:
    block_size: int
    expected_blocks: int
    manifest_blocks: int
    checked_blocks: int
    mismatches: list[Mismatch] = field(default_factory=list)
    chain_breaks: list[int] = field(default_factory=list)
    structural_errors: list[str] = field(default_factory=list)
    input_errors: list[str] = field(default_factory=list)

    @property
    def clean(self) -> bool:
        return not (
            self.mismatches
            or self.chain_breaks
            or self.structural_errors
            or self.input_errors
        )

    @property
    def exit_code(self) -> int:
        if self.input_errors:
            return 2
        if self.mismatches or self.chain_breaks or self.structural_errors:
            return 1
        return 0


def _load_manifest(path: str | Path) -> list[ManifestEntry]:
    entries = []
    with open(path, "r", encoding="utf-8") as handle:
        for line_number, line in enumerate(handle, 1):
            line = line.strip()
            if not line:
                continue
            try:
                value = json.loads(line)
                entry = ManifestEntry(
                    block=int(value["block"]),
                    offset=int(value["offset"]),
                    length=int(value["length"]),
                    sha256=str(value["sha256"]).lower(),
                    prev_hash=str(value["prev_hash"]).lower(),
                )
            except (KeyError, TypeError, ValueError, json.JSONDecodeError) as exc:
                raise ManifestError(f"invalid manifest line {line_number}: {exc}") from exc
            if entry.length != 4 * 1024 * 1024:
                raise ManifestError(
                    f"manifest block {entry.block} has unsupported length {entry.length}"
                )
            if not all(
                len(digest) == 64 and all(c in "0123456789abcdef" for c in digest)
                for digest in (entry.sha256, entry.prev_hash)
            ):
                raise ManifestError(f"invalid hash on manifest line {line_number}")
            entries.append(entry)
    return entries


def verify_image(image_path: str | Path, manifest_path: str | Path) -> VerifyResult:
    try:
        with ImageIO.open(image_path) as image:
            manifest = _load_manifest(manifest_path)
            result = VerifyResult(
                block_size=image.superblock.block_size,
                expected_blocks=image.superblock.total_blocks,
                manifest_blocks=len(manifest),
                checked_blocks=0,
            )
            if len(manifest) != image.superblock.total_blocks:
                result.structural_errors.append(
                    "manifest block count mismatch: "
                    f"expected={image.superblock.total_blocks} manifest={len(manifest)}"
                )
            if image.size > image.declared_size:
                result.structural_errors.append(
                    "image has data beyond declared block count"
                )

            previous_hash = GENESIS_HASH
            for ordinal, entry in enumerate(manifest):
                if entry.block != ordinal:
                    result.structural_errors.append(
                        f"manifest block order mismatch at line {ordinal + 1}: "
                        f"block={entry.block}"
                    )
                if entry.offset != ordinal * 4 * 1024 * 1024:
                    result.structural_errors.append(
                        f"manifest offset mismatch for block {entry.block}"
                    )
                if entry.prev_hash != previous_hash:
                    result.chain_breaks.append(entry.block)
                    result.structural_errors.append(
                        f"hash chain break at block {entry.block}"
                    )
                previous_hash = entry.sha256

                if entry.block >= image.superblock.total_blocks:
                    result.structural_errors.append(
                        f"manifest references block beyond image: {entry.block}"
                    )
                    continue
                info = image.block_info(entry.block, entry.length)
                if info.actual_length != entry.length:
                    result.structural_errors.append(
                        "short block "
                        f"{entry.block}: expected={entry.length} actual={info.actual_length}"
                    )
                if info.actual_length:
                    digest = hashlib.sha256()
                    start = info.physical_offset
                    end = start + info.actual_length
                    position = start
                    while position < end:
                        chunk_end = min(end, position + 1024 * 1024)
                        digest.update(image.read_physical(position, chunk_end - position))
                        position = chunk_end
                    actual_hash = digest.hexdigest()
                else:
                    actual_hash = hashlib.sha256(b"").hexdigest()
                result.checked_blocks += 1
                if actual_hash != entry.sha256:
                    result.mismatches.append(
                        Mismatch(
                            block=entry.block,
                            offset=info.physical_offset,
                            asset=info.asset,
                            logical_offset=info.logical_offset,
                            expected_sha256=entry.sha256,
                            actual_sha256=actual_hash,
                            reason="short block" if info.truncated else "hash mismatch",
                        )
                    )
            if not manifest:
                result.structural_errors.append("manifest is empty")
            return result
    except (OSError, ValueError) as exc:
        raise ManifestError(str(exc)) from exc
