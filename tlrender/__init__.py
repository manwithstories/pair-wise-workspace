"""tlrender: offline subtitle timeline resolution and first-frame rendering."""

__all__ = [
    "timebase",
    "tl_parse",
    "overlap",
    "transition",
    "compose",
    "digest",
    "report",
    "cli",
]

MAX_CUE_MS = 10 * 60 * 1000
MIN_READABLE_MS = 400
MAX_FILE_BYTES = 20 * 1024 * 1024
