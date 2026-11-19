"""Shared pytest fixtures.

The suite never checks in binary assets: every MP4/MOV/MKV used by the tests is
encoded on demand into a per-session temporary directory and reused, so the
first test that needs a corpus pays the encode cost once.
"""

from __future__ import annotations

import shutil
import tempfile
from pathlib import Path

import pytest

from aircheck.fixtures import EncodeSpec, encode_asset


@pytest.fixture(scope="session")
def workdir() -> Path:
    directory = Path(tempfile.mkdtemp(prefix="aircheck-tests-"))
    yield directory
    shutil.rmtree(directory, ignore_errors=True)


@pytest.fixture(scope="session")
def media_dir(workdir: Path) -> Path:
    path = workdir / "media"
    path.mkdir(parents=True, exist_ok=True)
    return path


@pytest.fixture(scope="session")
def multi_mp4(media_dir: Path):
    return encode_asset(
        media_dir / "multi.mp4",
        EncodeSpec(
            format_key="mp4",
            video_streams=2,
            audio_streams=2,
            sample_rates=(48000, 44100),
            duration_ms=1000,
            segment_ms=250,
            gop=12,
        ),
    )


@pytest.fixture(scope="session")
def multi_mov(media_dir: Path):
    return encode_asset(
        media_dir / "multi.mov",
        EncodeSpec(
            format_key="mov",
            video_streams=1,
            audio_streams=2,
            sample_rates=(48000, 44100),
            duration_ms=1000,
            segment_ms=250,
        ),
    )


@pytest.fixture(scope="session")
def multi_mkv(media_dir: Path):
    return encode_asset(
        media_dir / "multi.mkv",
        EncodeSpec(
            format_key="mkv",
            video_streams=1,
            audio_streams=2,
            sample_rates=(48000, 44100),
            duration_ms=1000,
            segment_ms=250,
        ),
    )


@pytest.fixture(scope="session")
def silent_mov(media_dir: Path):
    return encode_asset(
        media_dir / "silent.mov",
        EncodeSpec(
            format_key="mov",
            video_streams=1,
            audio_streams=1,
            duration_ms=1500,
            silent_audio=True,
        ),
    )
