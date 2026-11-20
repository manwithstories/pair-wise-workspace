"""Rate-invariant content fingerprints for cross-node de-duplication.

A fingerprint is a sequence of 64-bit hashes over *fixed time windows*
(default 20 ms), not fixed sample counts. The same audio arriving at 44.1 kHz
and at 48 kHz therefore produces the same hash sequence — comparison never
needs a shared sample rate. Each window is described by its signed RMS band
energy, which ignores level differences but distinguishes content.

Only content whose windows genuinely overlap in normalised time is treated as
duplicate. Two segments that are merely adjacent (one ends exactly when the
next begins) share no windows, so they can never be de-duplicated. Every
duplicate points back to the first source (lowest source id, then earliest
time) that already carried the same content.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field

import numpy as np

# A practical floor: windows quieter than this are acoustically empty and all
# hash to the same silence token regardless of source, which is intended.
_FLOOR = 1e-9
# Fixed absolute amplitude bands: identical audio on any sample rate lands in
# the same bands (per-window relative bands would drift with window length).
_AMP_BANDS = np.array([1e-4, 1e-3, 1e-2, 3e-2, 1e-1, 3e-1, 7e-1], dtype=np.float64)
# Zero-crossing-rate quantiles, likewise absolute (crossings per second).
_ZCR_BANDS = np.array([200.0, 800.0, 2000.0, 5000.0], dtype=np.float64)


@dataclass(frozen=True, slots=True)
class Fingerprint:
    source: str
    sample_rate: int
    window_ms: float
    hashes: tuple[int, ...]
    window_seconds: float  # nominal, identical for every window
    # Position on the unified timeline; defaults to a local 0-based range.
    timeline_start: float = 0.0

    @property
    def timeline_end(self) -> float:
        return self.timeline_start + len(self.hashes) * self.window_seconds


@dataclass(frozen=True, slots=True)
class DuplicateMatch:
    source: str
    start_seconds: float
    end_seconds: float
    duplicate_of_source: str
    duplicate_of_start_seconds: float
    duplicate_of_end_seconds: float
    overlap_seconds: float


@dataclass(slots=True)
class FingerprintIndex:
    window_ms: float
    fingerprints: dict[str, Fingerprint] = field(default_factory=dict)
    matches: list[DuplicateMatch] = field(default_factory=list)

    @property
    def duplicate_count(self) -> int:
        return len(self.matches)


def _window_hashes(pcm: np.ndarray, sample_rate: int, window_ms: float) -> tuple[int, ...]:
    if pcm.ndim == 1:
        pcm = pcm[:, None]
    win = max(1, int(round(sample_rate * window_ms / 1000.0)))
    mono = pcm.mean(axis=1)
    hashes: list[int] = []
    for start in range(0, len(mono), win):
        chunk = mono[start:start + win]
        if len(chunk) == 0:
            break
        rms = float(np.sqrt(np.mean(np.square(chunk, dtype=np.float64))))
        if rms < _FLOOR:
            hashes.append(0)  # canonical silence token, rate independent
            continue
        amp_code = int(np.digitize(np.abs(chunk).mean(), _AMP_BANDS))
        zcr = float(np.count_nonzero(np.signbit(chunk[1:]) != np.signbit(chunk[:-1])))
        zcr_per_sec = zcr * sample_rate / len(chunk)
        zcr_code = int(np.digitize(zcr_per_sec, _ZCR_BANDS))
        payload = np.array([amp_code, zcr_code], dtype=np.uint8)
        digest = hashlib.blake2b(payload.tobytes(), digest_size=8).digest()
        hashes.append(int.from_bytes(digest, "big"))
    return tuple(hashes)


def fingerprint_pcm(
    pcm: np.ndarray,
    sample_rate: int,
    source: str,
    *,
    window_ms: float = 20.0,
    timeline_start: float = 0.0,
) -> Fingerprint:
    return Fingerprint(
        source=source,
        sample_rate=sample_rate,
        window_ms=window_ms,
        hashes=_window_hashes(pcm, sample_rate, window_ms),
        window_seconds=window_ms / 1000.0,
        timeline_start=timeline_start,
    )


def _longest_common_run(a: tuple[int, ...], b: tuple[int, ...], min_windows: int) -> tuple[int, int, int] | None:
    """Longest contiguous equal hash run via an LCS length table."""
    if not a or not b:
        return None
    table = np.zeros((len(a) + 1, len(b) + 1), dtype=np.int32)
    best_len = 0
    best_i = 0
    best_j = 0
    for i in range(1, len(a) + 1):
        for j in range(1, len(b) + 1):
            if a[i - 1] == b[j - 1]:
                table[i, j] = table[i - 1, j - 1] + 1
                if table[i, j] > best_len:
                    best_len = int(table[i, j])
                    best_i, best_j = i, j
    if best_len < min_windows:
        return None
    return best_i - best_len, best_j - best_len, best_len


def register_and_match(
    index: FingerprintIndex,
    fp: Fingerprint,
    *,
    min_overlap_ms: float = 80.0,
) -> list[DuplicateMatch]:
    """Add ``fp`` to the index and match it against previously registered sources."""
    min_windows = max(2, int(round(min_overlap_ms / fp.window_ms)))
    new_matches: list[DuplicateMatch] = []
    # Earlier sources only: the first source to carry content is the authority.
    for prior_source, prior in index.fingerprints.items():
        run = _longest_common_run(fp.hashes, prior.hashes, min_windows)
        if run is None:
            continue
        i0, j0, length = run
        w = fp.window_seconds
        # The equal hash run must also overlap on the unified timeline. Two
        # ranges that merely abut (end == start) share no instant and are
        # therefore never de-duplicated even when the content is identical.
        new_start = fp.timeline_start + i0 * w
        new_end = fp.timeline_start + (i0 + length) * w
        prior_start = prior.timeline_start + j0 * w
        prior_end = prior.timeline_start + (j0 + length) * w
        overlap = min(new_end, prior_end) - max(new_start, prior_start)
        if overlap < min_overlap_ms / 1000.0:
            continue
        new_matches.append(
            DuplicateMatch(
                source=fp.source,
                start_seconds=round(new_start, 6),
                end_seconds=round(new_end, 6),
                duplicate_of_source=prior_source,
                duplicate_of_start_seconds=round(prior_start, 6),
                duplicate_of_end_seconds=round(prior_end, 6),
                overlap_seconds=round(overlap, 6),
            )
        )
    index.fingerprints[fp.source] = fp
    index.matches.extend(new_matches)
    return new_matches
