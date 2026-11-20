"""archiveflow: RTP/RTCP archive recovery pipeline.

Recovers incomplete program material from captured RTP/RTCP streams,
produces aligned multi-bitrate segment ladders and a JSON manifest for
downstream transcode and retrieval stages.
"""

__version__ = "0.1.0"

CLOCK_RATE = 90_000  # unified media clock, 90 kHz
