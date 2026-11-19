"""Deterministic result manifest with per-segment checksums.

The manifest is the durable contract between a run and the ``verify`` command:

* every error record keeps track, byte interval, time interval and category,
* each logical segment (track / damage category) gets a SHA-256 checksum of
  its canonical JSON encoding, so a single changed record is easy to spot,
* the whole document is serialised with sorted keys and fixed separators,
  making repeated runs byte-identical and giving the output a stable
  ``output_sha256`` fingerprint.
"""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass, field

CANONICAL_SEPARATORS = (",", ":")


def canonical_json(obj) -> bytes:
    """Stable byte encoding used for every checksum in the manifest."""
    return json.dumps(obj, sort_keys=True, ensure_ascii=False,
                      separators=CANONICAL_SEPARATORS, default=_default).encode("utf-8")


def _default(value):
    # Fractions and other rational types appear inside time ranges.
    from fractions import Fraction
    if isinstance(value, Fraction):
        return float(value)
    raise TypeError(f"cannot canonically encode {type(value)!r}")


@dataclass(slots=True)
class ManifestBuilder:
    run_params: dict
    input_fingerprint: dict
    clock_fit: dict = field(default_factory=dict)
    probe: dict = field(default_factory=dict)
    keyframes: dict = field(default_factory=dict)
    errors: list[dict] = field(default_factory=list)
    metrics: dict = field(default_factory=dict)
    resume: dict = field(default_factory=dict)

    def add_error(self, record: dict) -> None:
        # Keep the error list ordered by byte offset for stable output.
        self.errors.append(record)
        self.errors.sort(key=lambda r: (r.get("byte_offset_start", 0),
                                        r.get("category", "")))

    def segment_checksums(self) -> dict[str, str]:
        segments: dict[str, list] = {}
        for err in self.errors:
            segments.setdefault(err["category"], []).append(err)
        for track, info in self.probe.get("tracks", {}).items():
            segments[f"track:{track}"] = info
        checksums = {
            name: hashlib.sha256(canonical_json(payload)).hexdigest()
            for name, payload in sorted(segments.items())
        }
        checksums["__all_errors__"] = hashlib.sha256(
            canonical_json(self.errors)).hexdigest()
        return checksums

    def build(self) -> dict:
        document = {
            "schema": "relaydiag-result/1",
            "run_params": self.run_params,
            "input": self.input_fingerprint,
            "clock_fit": self.clock_fit,
            "probe": self.probe,
            "keyframes": self.keyframes,
            "errors": self.errors,
            "segment_checksums": self.segment_checksums(),
            "metrics": self.metrics,
            "resume": self.resume,
        }
        document["output_sha256"] = hashlib.sha256(
            canonical_json(document)).hexdigest()
        return document

    def write(self, path: str) -> dict:
        document = self.build()
        # Pretty form for humans; the fingerprint covers the canonical form,
        # so formatting does not affect verifiability.
        with open(path, "w", encoding="utf-8") as fh:
            json.dump(document, fh, sort_keys=True, indent=2, ensure_ascii=False)
            fh.write("\n")
        return document


def verify_document(document: dict) -> tuple[bool, list[str]]:
    """Recompute every checksum; return ``(ok, mismatched_names)``."""
    expected = document.get("output_sha256", "")
    embedded = dict(document)
    embedded.pop("output_sha256", None)
    actual = hashlib.sha256(canonical_json(embedded)).hexdigest()
    problems = []
    if actual != expected:
        problems.append("output_sha256 mismatch")
    return not problems, problems
