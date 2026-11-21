"""Archive media restoration toolkit.

Purely local processing of long-form timestamped MP4/MOV recordings:
shared timebase correction (clock drift, timestamp wraparound), VFR-aware
segmentation, silence detection, content fingerprints with duplicate
detection, a transactional SQLite index with resumable checkpoints and a
deterministic JSONL manifest.
"""

__version__ = "0.1.0"

# Exit codes used by the terminal interface (see cli.py).
EXIT_OK = 0
EXIT_FATAL = 1
EXIT_RECOVERED = 2
