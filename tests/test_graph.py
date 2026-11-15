"""混音图的静音/独奏语义与拓扑顺序测试。"""

from __future__ import annotations

import unittest

from mixctl.graph import build_graph
from mixctl.project import parse_project


def project_with_modes(modes):
    buses = [
        {"name": "vo", "output": "master"},
        {"name": "bed", "output": "master"},
        {"name": "master", "output": None},
    ]
    tracks = []
    for index, (name, bus, muted, solo) in enumerate(modes):
        tracks.append({
            "name": name, "file": f"{name}.wav", "channels": 1, "bus": bus,
            "gain_db": 0.0, "muted": muted, "solo": solo,
            "automation": [{"time": 0.0, "db": 0.0}],
        })
    return parse_project({
        "target_sr": 48000, "duration": 1.0, "hop": 64,
        "master_bus": "master", "buses": buses, "tracks": tracks,
    })


class GraphSemanticsTest(unittest.TestCase):
    def test_topological_order_terminal_is_master(self):
        project = project_with_modes([
            ("a", "vo", False, False), ("b", "bed", False, False),
        ])
        graph = build_graph(project)
        self.assertEqual(graph.order[-1], "master")
        self.assertLess(graph.order.index("vo"), graph.order.index("master"))
        self.assertLess(graph.order.index("bed"), graph.order.index("master"))

    def test_mute_excludes_track_from_mix(self):
        project = project_with_modes([
            ("a", "vo", True, False), ("b", "bed", False, False),
        ])
        graph = build_graph(project)
        self.assertFalse(graph.is_audible("a"))
        self.assertTrue(graph.is_audible("b"))

    def test_solo_zeros_others_but_keeps_their_metrics(self):
        project = project_with_modes([
            ("a", "vo", False, True),
            ("b", "bed", False, False),
            ("c", "bed", True, False),
        ])
        graph = build_graph(project)
        self.assertEqual(graph.solo_tracks, {"a"})
        self.assertTrue(graph.is_audible("a"))
        # 非独奏轨在混音中归零（无论是否静音）
        self.assertFalse(graph.is_audible("b"))
        self.assertFalse(graph.is_audible("c"))
        # 但图仍保留全部轨道，引擎据此独立统计指标
        self.assertEqual(set(graph.track_bus), {"a", "b", "c"})

    def test_no_solo_all_unmuted_audible(self):
        project = project_with_modes([
            ("a", "vo", False, False), ("b", "bed", False, False),
        ])
        graph = build_graph(project)
        self.assertEqual(graph.solo_tracks, set())
        self.assertTrue(graph.is_audible("a"))
        self.assertTrue(graph.is_audible("b"))


if __name__ == "__main__":
    unittest.main()
