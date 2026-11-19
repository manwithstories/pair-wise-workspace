"""bcastqc -- lossless broadcast remux QC for MP4/MOV delivery batches."""

from .errors import (
    EXIT_MEDIA,
    EXIT_OK,
    EXIT_THRESHOLD,
    EXIT_USAGE,
    QCError,
)

__version__ = "0.1.0"

__all__ = [
    "__version__",
    "QCError",
    "EXIT_OK",
    "EXIT_THRESHOLD",
    "EXIT_USAGE",
    "EXIT_MEDIA",
]
