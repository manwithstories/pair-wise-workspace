from __future__ import annotations

import copy

import pytest


def test_valid_manifest_and_graph_order(
    show_root, write_manifest, invoke, show_manifest_data
) -> None:
    path = write_manifest(show_manifest_data, "valid.json")
    code, payload = invoke("validate", path)
    assert code == 0
    assert payload["ok"] is True
    assert payload["tracks"] == 8
    assert payload["nodes"] == 28
    assert any(item["kind"] == "short_tail_padded" for item in payload["warnings"])
    assert any(item["kind"] == "long_track_truncated" for item in payload["warnings"])


@pytest.mark.parametrize(
    ("mutation", "message"),
    [
        (lambda data: data["tracks"][0].__setitem__("file", "missing.wav"), "missing audio file"),
        (lambda data: data["tracks"].append(copy.deepcopy(data["tracks"][0])), "duplicate track id"),
        (lambda data: data["tracks"][0].__setitem__("gain_db", float("nan")), "must be finite"),
        (lambda data: data["nodes"][0].__setitem__("type", "mystery"), "unknown node type"),
        (lambda data: data.__setitem__("sample_rate", 0), "sample_rate must be a positive integer"),
        (lambda data: data["buses"][0].__setitem__("ceiling", 1.5), "ceiling must be in (0, 1]"),
    ],
)
def test_manifest_rejections_exit_2(
    mutation, message, show_manifest_data, write_manifest, invoke
) -> None:
    data = copy.deepcopy(show_manifest_data)
    mutation(data)
    path = write_manifest(data, "invalid_manifest.json")
    code, payload = invoke("validate", path)
    assert code == 2
    assert payload["error"]["type"] == "manifest"
    assert message in payload["error"]["message"]


def test_graph_cycle_names_nodes(show_manifest_data, write_manifest, invoke) -> None:
    data = copy.deepcopy(show_manifest_data)
    gain_node = next(node for node in data["nodes"] if node["id"] == "host.gain")
    gain_node["input"] = "host.pan"
    path = write_manifest(data, "cycle.json")
    code, payload = invoke("validate", path)
    assert code == 2
    assert payload["error"]["type"] == "graph"
    assert set(payload["error"]["details"]["node_ids"]) >= {"host.gain", "host.pan"}


def test_graph_dangling_port_names_node(show_manifest_data, write_manifest, invoke) -> None:
    data = copy.deepcopy(show_manifest_data)
    gain_node = next(node for node in data["nodes"] if node["id"] == "host.gain")
    gain_node.pop("input")
    path = write_manifest(data, "dangling.json")
    code, payload = invoke("validate", path)
    assert code == 2
    assert payload["error"]["type"] == "graph"
    assert payload["error"]["details"] == {"node_id": "host.gain", "port": "in"}


def test_graph_unknown_output_node(show_manifest_data, write_manifest, invoke) -> None:
    data = copy.deepcopy(show_manifest_data)
    data["output_node"] = "missing.limiter"
    path = write_manifest(data, "bad_output.json")
    code, payload = invoke("validate", path)
    assert code == 2
    assert "does not exist" in payload["error"]["message"]


def test_canonical_graph_is_generated_without_nodes(
    show_root, show_manifest_data, write_manifest, invoke
) -> None:
    data = copy.deepcopy(show_manifest_data)
    data.pop("nodes")
    data.pop("output_node")
    path = write_manifest(data, "canonical.json")
    code, payload = invoke("validate", path)
    assert code == 0
    assert payload["nodes"] == 8 * 3 + 4
    assert payload["tracks"] == 8
