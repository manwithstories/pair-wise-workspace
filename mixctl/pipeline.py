"""顶层渲染流水线：建图、引擎调度、WAV 写出与报告组装。"""

from __future__ import annotations

import json
import os
from pathlib import Path

import numpy as np
import soundfile as sf

from .automation import Automation
from .engine_common import RenderSession
from .engine_offline import render_offline
from .engine_stream import render_stream
from .errors import DivergenceError
from .graph import build_graph
from .project import Keyframe, Project, load_project


def _atomic_write_json(path: Path, payload: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    with tmp.open("w", encoding="utf-8") as handle:
        json.dump(payload, handle, ensure_ascii=False, indent=2, sort_keys=False)
        handle.write("\n")
    os.replace(tmp, path)


def write_wav_24bit(path: str | Path, samples: np.ndarray, sample_rate: int) -> None:
    output = Path(path)
    output.parent.mkdir(parents=True, exist_ok=True)
    tmp = output.with_suffix(output.suffix + ".tmp")
    clamped = np.clip(samples, -1.0, 1.0).astype(np.float64)
    sf.write(str(tmp), clamped, sample_rate, format="WAV", subtype="PCM_24")
    os.replace(tmp, output)


def _automation_report(session: RenderSession) -> dict:
    any_curve = next(
        (rt.automation for rt in session.track_runtimes.values()
         if rt.automation is not None),
        None,
    )
    if any_curve is None:
        curve = Automation(
            [Keyframe(0.0, 0.0)],
            sample_rate=session.target_sr,
            hop=session.project.hop,
        )
    else:
        curve = any_curve
    return curve.control_report(session.total_samples)


def build_report(
    project: Project, session: RenderSession, engine_name: str
) -> dict:
    for runtime in session.track_runtimes.values():
        runtime.finalize_auto_points()
    tracks = []
    for track in project.tracks:
        runtime = session.track_runtimes[track.name]
        ratio = runtime.resampler.ratio
        entry = {
            "name": track.name,
            "source_sr": ratio.source_sr,
            "target_sr": ratio.target_sr,
            "ratio": ratio.ratio_text,
            **runtime.metrics.to_report(),
        }
        tracks.append(entry)
    master = session.master_metrics.to_report()
    return {
        "engine": engine_name,
        "target_sr": project.target_sr,
        "duration": project.duration,
        "samples": session.total_samples,
        "master": master,
        "tracks": tracks,
        "automation": _automation_report(session),
    }


def run_render(
    project_path: str,
    out_wav: str,
    report_path: str | None,
    engine: str = "offline",
    chunk: int = 777,
) -> dict:
    project = load_project(project_path)
    graph = build_graph(project)
    session = RenderSession(project, graph)
    if engine == "offline":
        master = render_offline(session)
        write_wav_24bit(out_wav, master, project.target_sr)
    elif engine == "stream":
        render_stream(session, chunk=chunk, output_path=out_wav,
                      sample_rate=project.target_sr)
    else:  # pragma: no cover - argparse 已限制取值
        raise ValueError(f"未知引擎: {engine}")
    report = build_report(project, session, engine_name=engine)
    if report_path is not None:
        _atomic_write_json(Path(report_path), report)
    return report


def compare_wav(a_path: str, b_path: str) -> dict:
    try:
        data_a, sr_a = sf.read(a_path, dtype="float64", always_2d=True)
        data_b, sr_b = sf.read(b_path, dtype="float64", always_2d=True)
    except Exception as exc:
        from .errors import AudioIOError

        raise AudioIOError(f"compare 读取 WAV 失败: {exc}") from exc
    if data_a.shape != data_b.shape or sr_a != sr_b:
        raise DivergenceError(
            f"形状或采样率不一致: {data_a.shape}@{sr_a} vs {data_b.shape}@{sr_b}"
        )
    diff = np.abs(data_a.astype(np.float64) - data_b.astype(np.float64))
    max_diff = float(diff.max()) if diff.size else 0.0
    return {
        "identical": bool(np.array_equal(data_a, data_b)),
        "max_abs_diff": max_diff,
        "samples": int(data_a.shape[0]),
    }
