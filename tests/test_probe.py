"""Tests for probe-level demuxing and error classification."""

import pytest

from archive_media.errors import ErrorLog, FatalMediaError
from archive_media.probe import AUDIO, VIDEO, Probe
from fixtures import build_fixtures


@pytest.fixture(scope="module")
def fixture_set(tmp_path_factory):
    return build_fixtures(str(tmp_path_factory.mktemp("probe")), target_kbps=900)


def test_probe_yields_ordered_samples(fixture_set):
    case = fixture_set.by_name("normal")
    log = ErrorLog()
    with Probe(case.path, log) as probe:
        assert VIDEO in probe.streams
        assert AUDIO in probe.streams
        samples = list(probe.samples())
    assert any(s.track == VIDEO for s in samples)
    assert any(s.track == AUDIO for s in samples)
    assert log.regions == []


def test_missing_video_track_is_fatal(tmp_path):
    # A text file with an mp4 extension cannot provide a video track.
    bogus = tmp_path / "broken.mp4"
    bogus.write_text("not a real container")
    with pytest.raises(FatalMediaError):
        Probe(str(bogus), ErrorLog())


def test_corrupt_packet_classified_recoverable(fixture_set):
    case = fixture_set.by_name("corrupted")
    log = ErrorLog()
    with Probe(case.path, log) as probe:
        list(probe.samples())
    codes = {region.code for region in log.regions}
    assert "corrupt_packet" in codes
    region = next(r for r in log.regions if r.code == "corrupt_packet")
    assert region.start_s >= 0  # location is recorded for auditing


def test_truncation_records_region(fixture_set):
    case = fixture_set.by_name("truncated")
    log = ErrorLog()
    with Probe(case.path, log) as probe:
        list(probe.samples())
    assert log.has_errors()
    # Truncation (or the corrupt packet that precedes it) must be reported.
    assert {r.code for r in log.regions} & {"truncated_input", "corrupt_packet"}
