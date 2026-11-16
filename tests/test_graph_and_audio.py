"""Graph topology, resampling and limiter invariants."""

from __future__ import annotations

import numpy as np
import pytest

from src.mixer.engine import MixSession, OfflineEngine, StreamingEngine
from src.mixer.graph import (
    MASTER,
    BusNode,
    MasterNode,
    MixGraph,
    TrackNode,
    build_default_graph,
)
from src.mixer.limiter import Limiter, Meter
from src.mixer.resample import (
    LinearResampler,
    pad_tail,
    resample_array,
    resample_duration_frames,
    trim_or_pad,
)


# -- graph ---------------------------------------------------------------
def _graph_with_tracks(project, bus_map=None, bus_gain=None) -> MixGraph:
    return build_default_graph(project, bus_map, bus_gain)


def test_default_graph_is_layered_and_ordered(project):
    graph = _graph_with_tracks(project, {"music": ["drums", "bass"]}, {"music": -3.0})
    summary = graph.validate()
    assert summary.track_nodes == 4
    assert summary.bus_nodes == 1
    assert summary.order[-1] == MASTER
    assert summary.layers[0] == ["bass", "drums", "fx", "pad"]
    assert summary.layers[1] == ["music"]
    assert summary.layers[2] == [MASTER]
    assert "graph: 4 tracks + 1 buses -> master" in summary.lines()[0]


def test_bus_gain_reaches_every_member(project):
    graph = _graph_with_tracks(project, {"music": ["drums", "bass"]}, {"music": -6.0})
    drums = graph.gain_path("drums")
    bass = graph.gain_path("bass")
    assert drums[1][0] == "music"
    assert drums[1][1] == pytest.approx(10 ** (-6.0 / 20.0))
    assert bass[1][1] == pytest.approx(10 ** (-6.0 / 20.0))
    direct = graph.gain_path("fx")
    assert direct[-1][0] == MASTER
    assert len(direct) == 2


def test_cycle_is_detected(project):
    graph = MixGraph()
    graph.add(MasterNode())
    graph.add_bus("a", [])
    graph.add_bus("b", ["a"])
    graph.connect("b", "a")
    graph.connect("a", MASTER)
    with pytest.raises(Exception) as excinfo:
        graph.validate()
    assert excinfo.value.code == "graph_cycle"
    assert set(excinfo.value.details["nodes"]) == {"a", "b"}


def test_self_connection_rejected(project):
    graph = MixGraph()
    graph.add(MasterNode())
    graph.add_bus("a", [])
    with pytest.raises(Exception) as excinfo:
        graph.connect("a", "a")
    assert excinfo.value.code == "graph_cycle"


def test_orphan_node_is_detected(project):
    graph = MixGraph()
    graph.add(MasterNode())
    graph.add_bus("live", [])
    graph.add_bus("dead", [])
    graph.connect("live", MASTER)
    with pytest.raises(Exception) as excinfo:
        graph.validate()
    assert excinfo.value.code == "graph_orphan_node"
    assert excinfo.value.details["orphans"] == ["dead"]


def test_unconnected_output_is_detected(project):
    graph = MixGraph()
    graph.add(MasterNode())
    graph.add(TrackNode(project.tracks[0]))
    graph.add(BusNode("bus", []))
    graph.connect("bus", MASTER)
    with pytest.raises(Exception) as excinfo:
        graph.validate()
    assert excinfo.value.code == "graph_orphan_node"
    assert graph.unconnected_outputs() == [project.tracks[0].name]


def test_master_without_inputs_is_rejected():
    graph = MixGraph()
    graph.add(MasterNode())
    with pytest.raises(Exception) as excinfo:
        graph.validate()
    assert excinfo.value.code == "graph_no_output"


def test_duplicate_node_and_edge_rejected(project):
    graph = MixGraph()
    graph.add(MasterNode())
    graph.add(BusNode("a", []))
    with pytest.raises(Exception) as excinfo:
        graph.add(BusNode("a", []))
    assert excinfo.value.code == "graph_duplicate_node"
    graph.connect("a", MASTER)
    with pytest.raises(Exception) as excinfo:
        graph.connect("a", MASTER)
    assert excinfo.value.code == "graph_duplicate_edge"


def test_unknown_bus_reference_rejected(project):
    with pytest.raises(Exception) as excinfo:
        _graph_with_tracks(project, {"music": ["drums", "ghost"]})
    assert excinfo.value.code == "graph_unknown_node"


# -- resampling ----------------------------------------------------------
def test_resample_identity_is_exact():
    signal = np.arange(4800, dtype=np.float64)[:, None] / 4800.0
    result = resample_array(signal, 48000, 48000)
    assert result.shape == signal.shape
    assert float(np.max(np.abs(result - signal))) == 0.0


def test_resample_decimates_half():
    signal = np.arange(96000, dtype=np.float64)[:, None]
    result = resample_array(signal, 96000, 48000)
    assert result.shape[0] == resample_duration_frames(96000, 96000, 48000)
    assert result.shape[0] == 48000


def test_resample_blockwise_matches_whole():
    rng = np.random.default_rng(3)
    signal = rng.standard_normal((44100, 2))
    resampler = LinearResampler(signal, 44100, 48000)
    whole = resampler.convert_all()
    chunks = []
    position = 0
    while position < whole.shape[0]:
        count = min(256, whole.shape[0] - position)
        chunks.append(resampler.convert(count, position, window_start=0))
        position += count
    assert float(np.max(np.abs(whole - np.concatenate(chunks)))) < 1e-9


def test_empty_source_and_zero_length():
    empty = np.zeros((0, 2))
    assert resample_array(empty, 96000, 48000).shape == (0, 2)
    assert LinearResampler(empty, 96000, 48000).convert(10).shape == (10, 2)


def test_trim_pad_and_tail_padding():
    signal = np.ones((5, 2))
    assert trim_or_pad(signal, 2, None).shape == (3, 2)
    padded = trim_or_pad(signal, 2, 5)
    assert padded.shape == (5, 2)
    assert np.all(padded[3:] == 0.0)
    assert pad_tail(signal, 8).shape == (8, 2)
    assert pad_tail(signal, 3).shape == (3, 2)


# -- limiter -------------------------------------------------------------
def test_limiter_caps_peak_and_counts_clips():
    signal = np.zeros((2000, 2))
    signal[500:510] = 2.5
    limiter = Limiter(48000)
    out = limiter.process_block(signal.copy())
    assert float(np.max(np.abs(out))) <= 10 ** (-0.5 / 20.0) + 1e-9
    stats = limiter.stats()
    assert stats["peak_in_dbfs"] > 0
    assert stats["max_reduction_db"] > 0


def test_limiter_is_block_size_independent():
    rng = np.random.default_rng(11)
    signal = rng.standard_normal((30000, 2)) * 0.4
    signal[10000:10050] *= 6.0
    whole = Limiter(48000).process_block(signal.copy())
    for size in (256, 1024, 777):
        limiter = Limiter(48000)
        parts = []
        position = 0
        while position < signal.shape[0]:
            count = min(size, signal.shape[0] - position)
            parts.append(limiter.process_block(signal[position:position + count].copy()))
            position += count
        close = np.concatenate(parts)
        assert float(np.max(np.abs(whole - close))) == 0.0


def test_limiter_passes_quiet_signal_untouched():
    signal = np.full((500, 2), 0.1)
    out = Limiter(48000).process_block(signal.copy())
    assert np.allclose(out, signal, rtol=0.0, atol=1e-12)


def test_limiter_zero_length_block_is_a_noop():
    limiter = Limiter(48000)
    out = limiter.process_block(np.zeros((0, 2)))
    assert out.shape == (0, 2)
    assert limiter.stats()["frames"] == 0


def test_meter_tracks_peak_rms_and_clips():
    meter = Meter()
    meter.add(np.array([[0.5, -0.5], [1.0, 0.0]]))
    snapshot = meter.snapshot()
    assert snapshot.frames == 2
    assert snapshot.peak_db == pytest.approx(0.0)
    assert snapshot.clip_count == 1
    expected_rms = np.sqrt((0.25 + 0.25 + 1.0) / 4)
    assert snapshot.rms_db == pytest.approx(20 * np.log10(expected_rms))


# -- session integration -------------------------------------------------
def test_session_reuses_graph_for_offline_then_stream(project):
    graph = _graph_with_tracks(project, {"music": ["drums", "bass"]}, {"music": -2.0})
    offline = OfflineEngine(project, graph).render()
    stream = StreamingEngine(project, graph, block_size=256).render()
    assert float(np.max(np.abs(offline.audio - stream.audio))) <= 1e-5


def test_bus_gain_attenuates_its_members_only(project):
    graph = _graph_with_tracks(project, {"music": ["drums", "bass"]}, {"music": -12.0})
    with MixSession(project, graph) as session:
        session.timeline_frames()
        attenuated = session._paths["drums"]
        untouched = session._paths["fx"]
    assert attenuated == pytest.approx(10 ** (-12.0 / 20.0))
    assert untouched == pytest.approx(1.0)


def test_continuity_reports_every_track(project):
    with MixSession(project) as session:
        session.timeline_frames()
        jumps = session.check_continuity(256)
    assert set(jumps) == {"drums", "bass", "pad", "fx"}
    assert all(value < 0.01 for value in jumps.values())


def test_silence_window_is_zeroed(project):
    with MixSession(project) as session:
        frames = session.timeline_frames()
        pad = session._runtimes["pad"]
        # Silence windows are master-timeline seconds; probe inside [0.5, 0.62).
        start = int(0.53 * session.rate)
        block = session._track_block(pad, start, 200)
    assert np.allclose(block, 0.0, atol=1e-12)


def test_timeline_covers_longest_track(project):
    with MixSession(project) as session:
        frames = session.timeline_frames()
        expected = max(
            rt.offset_frames + session.track_frames(name)
            for name, rt in session._runtimes.items()
        )
    assert frames == expected
    assert frames > 0
