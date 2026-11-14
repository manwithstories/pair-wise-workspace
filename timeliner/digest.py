"""Frame digests, Merkle root, and manifest read/write/recompute."""

import hashlib
import json
import os

from .errors import DigestMismatchError, TimelineError

MANIFEST_NAME = "manifest.json"
FRAME_FMT = "frame_%06d.rgb"


def frame_digest(rgb24):
    """SHA-256 over fixed-layout RGB24 bytes (row-major, R-G-B order)."""
    return hashlib.sha256(rgb24).hexdigest()


def merkle_root(digests):
    """Merkle root over hex digests; odd levels duplicate the last node."""
    if not digests:
        raise TimelineError("cannot build a Merkle root over zero frames")
    level = [bytes.fromhex(d) for d in digests]
    while len(level) > 1:
        if len(level) % 2 == 1:
            level.append(level[-1])
        level = [hashlib.sha256(level[i] + level[i + 1]).digest()
                 for i in range(0, len(level), 2)]
    return level[0].hex()


def build_manifest(timeline, fps, n_frames, digests, av_max_ms, av_mean_ms,
                   blend_calls):
    return {
        "version": 1,
        "width": timeline.width,
        "height": timeline.height,
        "fps": fps,
        "frames": n_frames,
        "pixfmt": "rgb24",
        "digest_alg": "sha256",
        "first_frame": digests[0],
        "last_frame": digests[-1],
        "merkle_root": merkle_root(digests),
        "frame_digests": list(digests),
        "av_max_err_ms": round(av_max_ms, 3),
        "av_mean_err_ms": round(av_mean_ms, 3),
    }


def write_manifest(path, manifest):
    """Deterministic manifest serialization (byte-identical across runs)."""
    text = json.dumps(manifest, sort_keys=True, separators=(",", ":"),
                      ensure_ascii=True)
    with open(path, "w", encoding="utf-8", newline="") as fh:
        fh.write(text + "\n")
    return path


def load_manifest(path):
    try:
        with open(path, "r", encoding="utf-8") as fh:
            data = json.load(fh)
    except FileNotFoundError:
        raise TimelineError("manifest not found: %s" % path)
    except json.JSONDecodeError as exc:
        raise TimelineError("manifest is not valid JSON: %s" % exc)
    required = ("frames", "frame_digests", "first_frame", "last_frame",
                "merkle_root", "width", "height")
    for name in required:
        if name not in data:
            raise TimelineError("manifest missing field %r" % name)
    return data


def check_manifest_consistency(manifest):
    """Internal consistency: counts, first/last digests, Merkle root."""
    digests = manifest["frame_digests"]
    problems = []
    if len(digests) != manifest["frames"]:
        problems.append("frame_digests has %d entries, manifest says %d"
                        % (len(digests), manifest["frames"]))
    if digests:
        if digests[0] != manifest["first_frame"]:
            problems.append("first_frame digest mismatch")
        if digests[-1] != manifest["last_frame"]:
            problems.append("last_frame digest mismatch")
        if merkle_root(digests) != manifest["merkle_root"]:
            problems.append("merkle_root mismatch")
    return problems


def verify_frames(manifest, frames_dir):
    """Recompute digests from frame files and compare with the manifest."""
    n = manifest["frames"]
    digests = manifest["frame_digests"]
    matched = 0
    mismatches = []
    expected_size = manifest["width"] * manifest["height"] * 3
    for i in range(n):
        path = os.path.join(frames_dir, FRAME_FMT % i)
        try:
            with open(path, "rb") as fh:
                data = fh.read()
        except FileNotFoundError:
            mismatches.append((i, "missing"))
            continue
        if len(data) != expected_size:
            mismatches.append((i, "bad size %d" % len(data)))
            continue
        if frame_digest(data) == digests[i]:
            matched += 1
        else:
            mismatches.append((i, "digest mismatch"))
    return matched, mismatches


def require_consistent(problems):
    if problems:
        raise DigestMismatchError("; ".join(problems))
