"""Magic-byte dispatch and the unified Track model."""

import os
from dataclasses import dataclass, field

from . import ebml, isobmff, riff
from .io_bounds import BoundedReader, MediaError

MEDIA_SUFFIXES = {".mp4", ".mov", ".m4a", ".m4v", ".wav", ".webm", ".mkv"}


@dataclass
class Track:
    path: str
    container: str
    track_index: int
    kind: str
    codec: str
    duration_us: int
    start_us: int = 0
    media_duration_us: int = 0
    timescale: int = 0
    sample_count: int = 0
    sample_duration_us: int = 0
    width: int = 0
    height: int = 0
    rotation: int = 0
    display_width: int = 0
    display_height: int = 0
    sample_rate: int = 0
    channels: int = 0
    bits_per_sample: int = 0
    language: str = None
    duration_source: str = "container"
    extra: dict = field(default_factory=dict)

    def summary(self):
        if self.kind == "video":
            text = "%s %dx%d" % (self.codec, self.display_width,
                                 self.display_height)
            return text + (" rot%d" % self.rotation if self.rotation else "")
        if self.kind == "audio":
            return "%s %dHz/%dch/%dbit" % (
                self.codec, self.sample_rate, self.channels,
                self.bits_per_sample)
        return self.codec or self.kind

    def to_dict(self):
        d = dict(self.__dict__)
        d["summary"] = self.summary()
        return d


@dataclass
class FileResult:
    path: str
    container: str = ""
    tracks: list = field(default_factory=list)
    warnings: list = field(default_factory=list)
    error: str = None

    @property
    def ok(self):
        return self.error is None

    @property
    def duration_us(self):
        return max((t.duration_us for t in self.tracks), default=0)

    def codecs(self):
        return [t.summary() for t in self.tracks]


def sniff_format(path):
    with open(path, "rb") as fh:
        head = fh.read(12)
    if len(head) >= 8 and head[4:8] == b"ftyp":
        return "isobmff"
    if len(head) >= 12 and head[0:4] == b"RIFF" and head[8:12] == b"WAVE":
        return "wav"
    if head[0:4] == b"\x1a\x45\xdf\xa3":
        return "ebml"
    return None


def extract_file(path):
    """Extract all tracks from one media file; errors are captured, not raised."""
    try:
        fmt = sniff_format(path)
        with BoundedReader.open(path) as reader:
            if fmt == "isobmff":
                container, _brands, raw, warnings = isobmff.parse(path, reader)
            elif fmt == "wav":
                container, raw, warnings = riff.parse(path, reader)
            elif fmt == "ebml":
                container, raw, warnings = ebml.parse(path, reader)
            else:
                raise MediaError("unrecognized magic bytes", 0, path)
        tracks = [Track(path=path, container=container, track_index=i, **t)
                  for i, t in enumerate(raw)]
        return FileResult(path=path, container=container, tracks=tracks,
                          warnings=warnings)
    except MediaError as exc:
        return FileResult(path=path, error=str(exc))
    except OSError as exc:
        return FileResult(path=path, error="io error: %s" % exc)


def collect(paths):
    """Expand files/directories into a sorted list of candidate media files."""
    out = []
    for p in paths:
        if os.path.isdir(p):
            for root, _dirs, names in os.walk(p):
                for name in names:
                    if os.path.splitext(name)[1].lower() in MEDIA_SUFFIXES:
                        out.append(os.path.join(root, name))
        else:
            out.append(p)
    return sorted(out)
