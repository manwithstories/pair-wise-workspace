"""流水线阶段：音频归一与色彩裁决（从主编排拆出，保持单批可审）。"""

from __future__ import annotations

from pathlib import Path

from .audio import AudioSpec, normalize_audio, read_audio, write_audio
from .color import ColorMetadata, reconcile
from .digest import read_json, write_json
from .errors import ErrorBook


def run_audio_stage(workdir: str, sample_rate: int, layout: str,
                    errors: ErrorBook) -> dict:
    """归一工作目录内的音频，写出标准化 wav，返回转换摘要。"""
    samples, src_rate = read_audio("fixtures/audio.wav", workdir)
    result = normalize_audio(
        samples,
        src_rate,
        AudioSpec(sample_rate, layout),
        source="fixtures/audio.wav",
        errors=errors,
    )
    write_audio("normalized/audio.wav", workdir, result.samples, sample_rate)
    return {
        "source_rate": src_rate,
        "target_rate": sample_rate,
        "layout": layout,
        "source_frames": result.source_frames,
        "output_frames": result.output_frames,
        "conversions": result.conversions,
    }


def run_color_stage(workdir: str, representations: int,
                    errors: ErrorBook) -> dict:
    """汇总各表示层色彩元数据并裁决，缺省按显式策略补齐。"""
    observations: dict[str, ColorMetadata] = {}
    for i in range(representations):
        try:
            data = read_json(f"fixtures/rep{i}.color.json", workdir)
            observations[f"rep{i}"] = ColorMetadata.from_dict(data)
        except FileNotFoundError:
            observations[f"rep{i}"] = ColorMetadata()
    chosen = reconcile(observations, errors=errors)
    write_json("normalized/color.json", workdir, chosen.to_dict())
    return chosen.to_dict()
