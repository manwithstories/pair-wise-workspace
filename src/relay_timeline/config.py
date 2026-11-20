"""Hard safety limits and tunable defaults.

All limits are deliberately conservative so memory usage scales with the
bounded queue capacity rather than with total presentation duration.
"""

from __future__ import annotations

import os
from dataclasses import dataclass

# 33-bit MPEG-2 PTS modulo (2**33), in 90 kHz ticks.
MPEG_PTS_MOD = 1 << 33
MPEG_PTS_TICKS_PER_SECOND = 90_000

# Safety ceilings applied to every accepted manifest / input set.
MAX_SEGMENTS = 2_000
MAX_TRACKS = 16
MAX_QUEUE_SIZE = 256
MAX_SEGMENT_BYTES = 512 * 1024 * 1024  # 512 MiB per segment file
MAX_OUTPUT_FRAMES = 200_000
MAX_MANIFEST_BYTES = 16 * 1024 * 1024
MAX_PATH_DEPTH = 64

# Memory allowance above the configured queue bound (acceptance target).
MEMORY_HEADROOM_MB = 256

# Default pipeline configuration.
DEFAULT_QUEUE_SIZE = 8
DEFAULT_DRIFT_PPM_LIMIT = 100.0
DEFAULT_TARGET_BITRATE = 1_800_000


POLICY_FAIL = "fail"
POLICY_GAP_FILL = "gap-fill"
POLICY_SKIP = "skip"
VALID_POLICIES = frozenset({POLICY_FAIL, POLICY_GAP_FILL, POLICY_SKIP})


@dataclass(frozen=True, slots=True)
class Limits:
    """Runtime-enforced limits; constructed once per run."""

    max_segments: int = MAX_SEGMENTS
    max_tracks: int = MAX_TRACKS
    max_queue_size: int = MAX_QUEUE_SIZE
    max_segment_bytes: int = MAX_SEGMENT_BYTES
    max_output_frames: int = MAX_OUTPUT_FRAMES
    max_path_depth: int = MAX_PATH_DEPTH
    memory_headroom_mb: int = MEMORY_HEADROOM_MB

    @staticmethod
    def from_env() -> "Limits":
        """Allow CI to tighten limits via environment variables."""
        def get(name: str, default: int) -> int:
            raw = os.environ.get(name)
            return int(raw) if raw else default

        return Limits(
            max_segments=get("RT_MAX_SEGMENTS", MAX_SEGMENTS),
            max_tracks=get("RT_MAX_TRACKS", MAX_TRACKS),
            max_queue_size=get("RT_MAX_QUEUE", MAX_QUEUE_SIZE),
            max_segment_bytes=get("RT_MAX_SEGMENT_BYTES", MAX_SEGMENT_BYTES),
            max_output_frames=get("RT_MAX_OUTPUT_FRAMES", MAX_OUTPUT_FRAMES),
        )
