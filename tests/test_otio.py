import opentimelineio as otio
import pytest

from tlcore.convert import from_otio, model_summary, read_otio, to_otio, write_otio
from tlcore.io_utils import ExchangeError
from tlcore.model import Segment, Timeline, Track


def sample_timeline() -> Timeline:
    return Timeline(
        name="otio",
        tracks=[
            Track(
                "v1",
                "video",
                z_order=1,
                metadata={"camera": "A"},
                segments=[
                    Segment("a", 0, 100, source_start=20, metadata={"note": "one"}),
                    Segment("b", 150, 50, extensions={"vendor": "x"}),
                ],
            ),
            Track("a1", "audio", segments=[Segment("m", 0, 200)]),
        ],
        metadata={"series": "ep01"},
        declared_duration=200,
    )


def test_otio_roundtrip_summary_matches(tmp_path):
    timeline = sample_timeline()
    path = tmp_path / "export.otio"
    write_otio(timeline, path)
    reread = read_otio(path)
    assert model_summary(reread) == model_summary(timeline)
    assert reread.track("v1").segments[1].start == 150
    assert reread.track("v1").metadata["camera"] == "A"


def test_otio_rate_mismatch_names_track():
    value = to_otio(sample_timeline())
    value.tracks[0].metadata["tlcore"]["fps"] = "25"
    with pytest.raises(ExchangeError, match="v1"):
        from_otio(value)


def test_otio_audio_overlap_roundtrips_through_lanes():
    timeline = Timeline(
        tracks=[Track("v1", "video", segments=[Segment("a", 0, 100), Segment("b", 50, 100)])]
    )
    value = to_otio(timeline)
    assert len(value.tracks) == 2
    reread = from_otio(value)
    assert model_summary(reread) == model_summary(timeline)
