from fractions import Fraction

import pytest

from media_probe import fixtures_gen, probe
from media_probe.probe import ProbeError


def test_probe_drift_stream_facts(drift_path):
    result = probe.probe(drift_path)
    assert result.codec == "h264"
    assert (result.width, result.height) == (320, 240)
    assert result.time_base == Fraction(1, 90000)
    assert result.nominal_fps == pytest.approx(30.0, abs=0.5)
    assert result.duration_sec == pytest.approx(10.0, abs=0.5)


def test_probe_matroska_time_base(fixtures_dir):
    import os

    result = probe.probe(os.path.join(fixtures_dir, "timebase_1k.mkv"))
    assert result.time_base == Fraction(1, 1000)
    assert result.codec == "h264"


def test_probe_hevc_supported(fixtures_dir):
    import os

    result = probe.probe(os.path.join(fixtures_dir, "hevc_clip.mp4"))
    assert result.codec == "hevc"


def test_probe_rejects_missing_file():
    with pytest.raises(ProbeError):
        probe.probe("/nonexistent/clip.mp4")


def test_probe_rejects_garbage(tmp_path):
    bad = tmp_path / "garbage.bin"
    bad.write_bytes(b"\x00\x11" * 512)
    with pytest.raises(ProbeError):
        probe.probe(str(bad))


def test_probe_rejects_unsupported_codec(tmp_path):
    clip = tmp_path / "mpeg4.mp4"
    fixtures_gen.encode_clean(str(clip), frames=5, gop=30, bframes=0, codec="mpeg4")
    with pytest.raises(ProbeError, match="unsupported"):
        probe.probe(str(clip))
