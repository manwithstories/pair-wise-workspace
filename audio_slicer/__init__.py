"""audio_slicer: silence-based SFX event slicing, QC and export."""

from .detector import (Segment, Silence, check_invariants, compute_silences,
                       detect_events, estimate_noise_floor)
from .framing import frame_dbfs
from .refine import refine_segments
from .store import DatabaseError, close_db, load_run, open_db, save_run
from .wavio import WavFormatError, file_sha256, read_wav, safe_resolve

__version__ = "0.1.0"

__all__ = [
    "Segment", "Silence", "check_invariants", "compute_silences",
    "detect_events", "estimate_noise_floor", "frame_dbfs", "refine_segments",
    "DatabaseError", "close_db", "load_run", "open_db", "save_run",
    "WavFormatError", "file_sha256", "read_wav", "safe_resolve",
]
