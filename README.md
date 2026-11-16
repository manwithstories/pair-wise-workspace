# 离线帧序列合成器 (compositor)

为游戏剧情过场、技能演示与赛事高光制作**可重复验证**的离线帧序列:
把带时间码的背景、角色立绘、粒子贴图、字幕条与音效事件合成为帧摘要,
用于定位闪帧、混合错误与音画漂移。纯本地、无网络、无 GUI, 只落盘 JSON/JSONL。

## 技术栈

Python 3.11+、Pillow 10.x、NumPy 1.26.x, 其余仅标准库
(argparse / json / hashlib / wave / fractions / unittest)。

```
pip install -r requirements.txt
```

## 快速演示

```bash
python -m compositor.cli generate-fixture --out /tmp/timeline_fixture.json
python -m compositor.cli render \
    --timeline /tmp/timeline_fixture.json \
    --fps 30 --sample-rate 48000 \
    --output /tmp/frame_digest.jsonl
python -m unittest discover -v
```

`frame_digest.jsonl` 含 120 行帧摘要 + 1 行全局摘要:
每帧带 `frame_index`、64 位十六进制 `pixel_sha256`、`opaque_pixels`、
`transparent_pixels`、`audio_start_sample`、`av_sync_error_us`;
末行 `kind=summary` 携带 `summary_sha256` 与 `max_av_sync_error_us`
(30fps 夹具最大 16666us, 不超过半帧 16700us)。

## 模块划分

| 模块 | 职责 |
|---|---|
| `compositor/types.py` | 有理数时间换算、Source/Layer/AudioEvent/Timeline、错误类型、摘要数据类 |
| `compositor/timeline.py` | JSON 解析与收集式校验(错误位置/类型/可继续条目数) |
| `compositor/blend.py` | source-over / add / multiply / screen, 预乘<->非预乘, 透明零污染 |
| `compositor/renderer.py` | 素材物化、缩放旋转、遮罩、(track,order) 稳定排序、画布裁剪 |
| `compositor/audio.py` | 确定性内存 PCM、事件采样范围、末帧时间与音画对齐误差 |
| `compositor/digest.py` | 像素 SHA256、透明统计、链式全局摘要 |
| `compositor/exporter.py` | 复用 renderer 编排, 逐帧写 JSONL(不复制像素逻辑, 不写图像) |
| `compositor/cli.py` | 终端入口、JSON envelope、退出码契约 |
| `fixtures/generate.py` | 程序化夹具(渐变/精灵/遮罩/音频事件), 无外部文件 |
| `tests/` | 29 个 unittest: 时间边界、混合、异常恢复、同步误差、确定性 |

## 关键约定

- **时间**: 全程 `fractions.Fraction` 精确换算, 24/30/60 fps 与整数采样率;
  拒绝负时长、非单调时间码、越界帧号。
- **半开区间**: 图层活跃区间 `[frame_in, frame_out)`, 边界帧不重复。
- **排序**: 同帧按 `(track, 声明顺序)` 稳定排序; 负坐标图层按相交矩形裁剪,
  无数组越界。
- **确定性**: 无随机量, 重复运行的像素哈希与 `summary_sha256` 逐字节一致。
- **不落盘媒体**: 只写 JSON/JSONL, 不产生 PNG/WAV/中间帧; PCM 仅存内存。

## 退出码

| 码 | 含义 |
|---|---|
| 0 | 全部成功 |
| 2 | 输入或时间线校验失败 |
| 3 | 渲染或音频处理失败 |
| 4 | 摘要写入失败 |

## 性能

单帧内存随画布像素线性增长(同时只持有一张 float64 画布 + 素材缓存);
120 帧 320x180 夹具端到端渲染约 0.8 秒, 远低于 30 秒预算。
