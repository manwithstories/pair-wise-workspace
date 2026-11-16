"""程序化测试夹具生成器。

生成一份自描述的时间线 JSON: 素材仅携带生成 spec(由 renderer 物化),
不引用任何仓库外文件, 不含随机量。夹具覆盖:
- 渐变背景、半透明径向精灵、椭圆遮罩三种素材;
- source-over/add/multiply/screen 四种混合;
- 缩放、旋转、负坐标画布外裁剪、相邻半开区间边界;
- 帧对齐音频事件 + 一个半帧偏移事件(验证 16666us 级对齐误差)。
"""

from __future__ import annotations

import json

CANVAS_WIDTH = 320
CANVAS_HEIGHT = 180
FPS = 30
SAMPLE_RATE = 48000
FRAME_COUNT = 120          # 30fps 下 4 秒
HERO_SIZE = 96
PARTICLE_SIZE = 24
TINT_SIZE = (160, 90)


def _sources() -> list[dict]:
    return [
        {
            "id": "bg_gradient",
            "kind": "gradient",
            "width": CANVAS_WIDTH,
            "height": CANVAS_HEIGHT,
            "spec": {
                "color_a": [18, 24, 48],
                "color_b": [210, 120, 60],
            },
        },
        {
            "id": "tint_gradient",
            "kind": "gradient",
            "width": TINT_SIZE[0],
            "height": TINT_SIZE[1],
            "spec": {
                "color_a": [40, 90, 160],
                "color_b": [180, 220, 255],
            },
        },
        {
            "id": "hero_sprite",
            "kind": "sprite",
            "width": HERO_SIZE,
            "height": HERO_SIZE,
            "spec": {
                "center_rgb": [255, 214, 90],
                "center_alpha": 1.0,
                "edge_alpha": 0.1,
            },
        },
        {
            "id": "particle_sprite",
            "kind": "sprite",
            "width": PARTICLE_SIZE,
            "height": PARTICLE_SIZE,
            "spec": {
                "center_rgb": [120, 230, 255],
                "center_alpha": 0.9,
                "edge_alpha": 0.0,
            },
        },
        {
            "id": "hero_mask",
            "kind": "mask",
            "width": HERO_SIZE,
            "height": HERO_SIZE,
            "spec": {"rx_ratio": 0.85, "ry_ratio": 1.0},
        },
    ]


def _layers() -> list[dict]:
    # 英雄精灵分两段相邻半开区间 [0,60) 与 [60,120), 边界帧 60 不重复。
    return [
        {
            "id": "background", "source_id": "bg_gradient",
            "frame_in": 0, "frame_out": FRAME_COUNT,
            "track": 0, "x": 0, "y": 0, "scale": 1.0, "rotation": 0.0,
            "alpha": 1.0, "blend": "source-over",
        },
        {
            "id": "tint_panel", "source_id": "tint_gradient",
            "frame_in": 10, "frame_out": 110,
            "track": 3, "x": 90, "y": 50, "scale": 1.2, "rotation": 15.0,
            "alpha": 0.5, "blend": "multiply",
        },
        {
            "id": "hero_a", "source_id": "hero_sprite",
            "frame_in": 0, "frame_out": 60,
            "track": 1, "x": 40, "y": 42, "scale": 1.0, "rotation": 0.0,
            "alpha": 0.95, "blend": "source-over",
            "mask_source_id": "hero_mask",
        },
        {
            "id": "hero_b", "source_id": "hero_sprite",
            "frame_in": 60, "frame_out": 120,
            "track": 1, "x": 184, "y": 42, "scale": 1.0, "rotation": 0.0,
            "alpha": 0.95, "blend": "source-over",
            "mask_source_id": "hero_mask",
        },
        {
            # 负坐标: 左半部分落在画布外, 验证相交矩形裁剪。
            "id": "particle_sweep", "source_id": "particle_sprite",
            "frame_in": 20, "frame_out": 80,
            "track": 2, "x": -10, "y": 12, "scale": 1.5, "rotation": 0.0,
            "alpha": 0.8, "blend": "add",
        },
        {
            "id": "glow_pass", "source_id": "particle_sprite",
            "frame_in": 60, "frame_out": 120,
            "track": 4, "x": 250, "y": 130, "scale": 2.0, "rotation": 45.0,
            "alpha": 0.7, "blend": "screen",
        },
    ]


def _audio_events() -> list[dict]:
    # 帧边界事件误差为 0; 半帧偏移事件 1/60s => 16666us (< 16700us)。
    aligned_frames = (0, 24, 48, 72, 96)
    events = [
        {
            "id": f"tone_{idx}",
            "start": f"{frame}/{FPS}",
            "duration": "1/10",
        }
        for idx, frame in enumerate(aligned_frames)
    ]
    events.insert(1, {
        "id": "tone_half_offset",
        "start": "1/60",
        "duration": "1/20",
    })
    return events


def build_fixture() -> dict:
    """构造夹具时间线 dict(可直接喂给 timeline.parse_timeline)。"""
    return {
        "canvas": {"width": CANVAS_WIDTH, "height": CANVAS_HEIGHT},
        "timeline": {
            "fps": FPS,
            "sample_rate": SAMPLE_RATE,
            "frame_count": FRAME_COUNT,
        },
        "sources": _sources(),
        "layers": _layers(),
        "audio_events": _audio_events(),
    }


def write_fixture(path: str) -> None:
    """把夹具 JSON 写到指定路径(只写 JSON, 不产生任何媒体文件)。"""
    with open(path, "w", encoding="utf-8", newline="\n") as handle:
        json.dump(build_fixture(), handle, ensure_ascii=False, indent=2)
        handle.write("\n")


if __name__ == "__main__":  # 允许 python -m fixtures.generate <path>
    import sys

    if len(sys.argv) != 2:
        raise SystemExit("用法: python -m fixtures.generate <输出路径>")
    write_fixture(sys.argv[1])
