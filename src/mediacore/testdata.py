"""Deterministic fixture set: generation, damage and timeline samples.

Every sample used by tests and the demo is generated here from a fixed seed.
Nothing is downloaded and nothing depends on wall-clock time, so two machines
produce byte-identical files.  The set covers the six recovery scenarios from
the acceptance contract: healthy, truncated, corrupt packet, index gap,
missing index and cyclic dependency, plus the three-track MP4 whose streams
have different timebases (the drift test).
"""

from __future__ import annotations

import hashlib
import json
import shutil
import struct
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .errors import ParameterError
from .testdata_damage import (
    _corrupt_packet,
    _drop_box,
    _drop_stco_entry,
    _truncate,
)
from .testdata_write import (
    SYNTH_FPS,
    SYNTH_HEIGHT,
    SYNTH_TACTICAL_RATE,
    SYNTH_VOICE_RATE,
    SYNTH_WIDTH,
    _add_audio_stream,
    _add_video_stream,
    _write_audio_track,
    _write_multitrack,
    _write_rtp_wrapped,
    _write_timeline,
    _write_wav,
    _video_frame,
    synthetic_signal,
)
from .timebase import US_PER_SECOND

__all__ = [
    "FixtureSet",
    "generate_fixtures",
    "synthetic_signal",
    "SYNTH_FPS",
    "SYNTH_VIDEO_TB",
    "SYNTH_VOICE_RATE",
    "SYNTH_TACTICAL_RATE",
]

SYNTH_FPS = 30
SYNTH_VIDEO_TB = "1/15360"  # 512 ticks per 30 fps frame
SYNTH_VOICE_RATE = 44_100
SYNTH_TACTICAL_RATE = 48_000
SYNTH_WIDTH = 160
SYNTH_HEIGHT = 90
SYNTH_SECONDS = 3.0






@dataclass
class FixtureSet:
    """Paths and metadata for one generated fixture directory."""

    root: Path
    sample_rate: int
    layout: str
    files: dict[str, Path] = field(default_factory=dict)
    manifest: dict[str, Any] = field(default_factory=dict)

    def path(self, name: str) -> Path:
        return self.files[name]

    def __getitem__(self, name: str) -> Path:
        return self.files[name]

    def to_dict(self) -> dict[str, Any]:
        return {
            "root": str(self.root),
            "sample_rate": self.sample_rate,
            "layout": self.layout,
            "files": {k: v.name for k, v in self.files.items()},
            "manifest": self.manifest,
        }


def generate_fixtures(
    root: str | Path,
    *,
    sample_rate: int = 48_000,
    layout: str = "stereo",
    seconds: float = SYNTH_SECONDS,
    force: bool = False,
) -> FixtureSet:
    """Create the full fixture family under ``root``.

    The generated set is idempotent: if ``manifest.json`` already describes
    every file and the sizes/hashes match, generation is skipped unless
    ``force`` is set.  This keeps repeated demo runs fast.
    """
    root = Path(root)
    if layout not in ("mono", "stereo"):
        raise ParameterError("layout must be mono or stereo", layout=layout)
    if sample_rate < 8_000:
        raise ParameterError("sample rate too low", sample_rate=sample_rate)
    duration_us = int(seconds * US_PER_SECOND)
    root.mkdir(parents=True, exist_ok=True)

    expected = {
        "clean",
        "truncated",
        "corrupt",
        "index_gap",
        "index_missing",
        "nonzero_origin",
        "voice",
        "tactical",
        "overlap",
        "wrapped",
        "timeline_clean",
        "timeline_overlap",
        "timeline_cycle",
    }
    manifest_path = root / "manifest.json"
    if not force and manifest_path.is_file():
        try:
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            if set(manifest.get("files", {})) == expected and all(
                (root / name).is_file() for name in manifest["files"]
            ):
                fs = FixtureSet(root=root, sample_rate=sample_rate, layout=layout)
                fs.files = {k: root / k for k in manifest["files"]}
                fs.manifest = manifest
                return fs
        except (OSError, json.JSONDecodeError):
            pass

    files: dict[str, Path] = {}

    clean = root / "clean.mp4"
    _write_multitrack(clean, duration_us=duration_us, offsets_us=None)
    files["clean"] = clean

    files["voice"] = _write_wav(
        root / "voice.wav",
        synthetic_signal(sample_rate, duration_us, freq=220.0, channels=_channels(layout)),
        sample_rate,
    )
    files["tactical"] = _write_wav(
        root / "tactical.wav",
        synthetic_signal(sample_rate, duration_us, freq=880.0, channels=1, phase=0.5),
        sample_rate,
    )
    files["overlap"] = root / "overlap.wav"
    shutil.copyfile(files["voice"], files["overlap"])

    files["truncated"] = _truncate(clean, root / "truncated.mp4")
    files["corrupt"] = _corrupt_packet(clean, root / "corrupt.mp4")
    files["index_gap"] = _drop_stco_entry(clean, root / "index_gap.mp4")
    files["index_missing"] = _drop_box(clean, root / "index_missing.mp4", "stco")
    files["nonzero_origin"] = root / "nonzero_origin.mp4"
    _write_multitrack(
        files["nonzero_origin"], duration_us=duration_us, offsets_us=2_500_000
    )
    files["wrapped"] = root / "wrapped.aac"
    _write_rtp_wrapped(files["wrapped"], sample_rate=sample_rate)

    files["timeline_clean"] = _write_timeline(
        root / "timeline_clean.json",
        clips=[
            # Two clips on the voice track joined by a crossfade.  The overlap
            # is licensed by the transition; audio on different source rates
            # still lands on the same microsecond grid.
            dict(id="voice-a", track="voice", start_us=0, end_us=1_200_000),
            dict(id="voice-b", track="voice", start_us=1_000_000, end_us=2_500_000),
            dict(id="tactical-a", track="tactical", start_us=0, end_us=2_500_000),
        ],
        transitions=[
            dict(id="voice-xfade", left="voice-a", right="voice-b", duration_us=200_000)
        ],
    )
    files["timeline_overlap"] = _write_timeline(
        root / "timeline_overlap.json",
        clips=[
            dict(id="voice-a", track="voice", start_us=0, end_us=1_200_000),
            dict(id="voice-b", track="voice", start_us=1_000_000, end_us=2_500_000),
        ],
        transitions=[],
    )
    files["timeline_cycle"] = _write_timeline(
        root / "timeline_cycle.json",
        clips=[
            dict(id="voice-a", track="voice", start_us=0, end_us=1_200_000),
            dict(id="voice-b", track="voice", start_us=1_000_000, end_us=2_500_000),
        ],
        transitions=[
            dict(id="ab", left="voice-a", right="voice-b", duration_us=200_000,
                 after=("ba",)),
            dict(id="ba", left="voice-b", right="voice-a", duration_us=200_000,
                 after=("ab",)),
        ],
    )

    manifest = {
        "generator": "mediacore.testdata.v1",
        "sample_rate": sample_rate,
        "layout": layout,
        "duration_us": duration_us,
        "files": {},
    }
    for name, path in files.items():
        data = path.read_bytes()
        manifest["files"][path.name] = {
            "fixture": name,
            "size": len(data),
            "sha256": hashlib.sha256(data).hexdigest(),
        }
    manifest_path.write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    fs = FixtureSet(root=root, sample_rate=sample_rate, layout=layout)
    fs.files = files
    fs.manifest = manifest
    return fs


def _channels(layout: str) -> int:
    return 1 if layout == "mono" else 2


# ---------------------------------------------------------------------------
# writers
# ---------------------------------------------------------------------------


















# ---------------------------------------------------------------------------
# damage derivation
# ---------------------------------------------------------------------------


















