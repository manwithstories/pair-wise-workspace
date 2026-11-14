from tlcore.convert import convert_etj, model_summary, read_etj
from tlcore.etj_v2 import write_v2
from tlcore.model import Segment, Timeline, Track


def test_v1_to_v2_groups_tracks_and_preserves_extensions(tmp_path):
    source = tmp_path / "in.v1.json"
    output = tmp_path / "out.v2.json"
    source.write_text(
        '''
        {"format":"etj","version":1,"name":"demo","fps":"25",
         "tracks":[{"id":"d1","kind":"audio"},{"id":"v1","kind":"video","z_order":1}],
         "clips":[
           {"id":"a","track":"d1","start_ms":0,"duration_ms":1000,"x_ext":1},
           {"id":"b","track":"v1","start_ms":0,"duration_ms":1000}
         ]}
        ''',
        encoding="utf-8",
    )
    timeline, _ = convert_etj(source, output, 2)
    reread = read_etj(output)
    assert [track.id for track in reread.tracks] == ["d1", "v1"]
    assert reread.track("d1").segments[0].extensions["x_ext"] == 1
    assert model_summary(reread) == model_summary(timeline)


def test_v2_to_v1_quantizes_and_warns(tmp_path):
    timeline = Timeline(
        tracks=[Track("v1", "video", segments=[Segment("a", 24, 72)])],
        declared_duration=96,
    )
    source = tmp_path / "in.v2.json"
    target = tmp_path / "out.v1.json"
    write_v2(timeline, source)
    _, warnings = convert_etj(source, target, 1)
    assert warnings
    assert target.exists()


def test_extensions_survive_v2_v1_v2(tmp_path):
    timeline = Timeline(
        tracks=[
            Track(
                "v1",
                "video",
                extensions={"x_track": "track"},
                segments=[Segment("a", 0, 48_000, extensions={"x_clip": {"n": 3}})],
            )
        ],
        declared_duration=48_000,
        extensions={"x_project": "P2219"},
    )
    source = tmp_path / "source.v2.json"
    downgraded = tmp_path / "down.v1.json"
    restored = tmp_path / "up.v2.json"
    write_v2(timeline, source)
    convert_etj(source, downgraded, 1)
    convert_etj(downgraded, restored, 2)
    reread = read_etj(restored)
    segment = reread.track("v1").segments[0]
    assert reread.extensions["x_project"] == "P2219"
    assert reread.track("v1").extensions["x_track"] == "track"
    assert segment.extensions["x_clip"] == {"n": 3}
