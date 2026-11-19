"""mediamix — frame-sequence alignment, compositing and validation pipeline.

Data flow: probe -> clock -> timeline -> render -> validate, with resume
checkpoints written at every stage boundary. All modules share the time base,
record types and offset-carrying errors defined in :mod:`mediamix.model`.
"""

__version__ = "0.1.0"
