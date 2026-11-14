from __future__ import annotations

import json

import numpy as np
import av

from framefix.cli import main


def test_scan_cli_json_and_clean_exit(bframes_path, capsys):
    code = main(["scan", str(bframes_path), "--json"])
    captured = capsys.readouterr()
    payload = json.loads(captured.out)
    assert code == 0
    assert payload["decoded_frames"] == 72
    assert payload["b_frames"] > 0
    assert payload["type_mismatch"] == 0
    assert payload["exit_code"] == 0


def test_scan_cli_missing_input_is_exit_2(tmp_path, capsys):
    code = main(["scan", str(tmp_path / "missing.mp4"), "--json"])
    assert code == 2
    assert "cannot open" in capsys.readouterr().err


def test_scan_cli_unsupported_codec_is_exit_2(tmp_path, capsys):
    path = tmp_path / "mpeg4.mp4"
    with av.open(str(path), "w") as container:
        stream = container.add_stream("mpeg4", rate=10)
        stream.width = 64
        stream.height = 64
        stream.pix_fmt = "yuv420p"
        frame = av.VideoFrame.from_ndarray(
            np.full((64, 64, 3), 90, dtype=np.uint8), format="rgb24"
        )
        frame.pts = 0
        for packet in stream.encode(frame):
            container.mux(packet)
        for packet in stream.encode():
            container.mux(packet)
    code = main(["scan", str(path), "--json"])
    assert code == 2
    assert "unsupported video codec" in capsys.readouterr().err


def test_repair_cli_exit_zero(broken_path, tmp_path):
    output = tmp_path / "fixed.mp4"
    report = tmp_path / "report.json"
    code = main(
        [
            "repair",
            str(broken_path),
            "--out",
            str(output),
            "--report",
            str(report),
        ]
    )
    assert code == 0
    assert output.exists() and report.exists()

