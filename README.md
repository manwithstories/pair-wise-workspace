# aoa — 一阶环境声编码/解码验证工具

纯本地、离线的一阶 Ambisonics（FOA）工具链：由显式种子生成可复现的球面声场
样本，完成 ACN/SN3D、ACN/N3D、FuMa 三种约定下的编码、归一化互转、扬声器与
双耳虚拟扬声器双路径渲染，并将能量分布、电平与串扰指标以 JSON 输出给自动化
流水线。仅依赖 Python ≥3.11 与 NumPy ≥1.26。

## 安装与验收

```bash
python -m pip install -e .
python aoa_cli.py verify --frames 48000 --seed 17 --norm-in fuma \
    --norm-out acn-sn3d --layout ring8 --regularization 1e-6 --json
python -m unittest discover -v
```

## 子命令

- `demo`：短块、固定种子的端到端人类可读演示。
- `encode`：单声道 WAV + 方位/俯仰角 → 4 通道 B-format WAV。
- `decode`：4 通道 B-format WAV → 扬声器布局 WAV（三策略解码矩阵）。
- `verify`：夹具 → 编码 → 归一化 → 双路径渲染 → 指标门禁，输出 JSON。

## 退出码

| 码 | 含义 |
|---|---|
| 0 | 全部阈值通过 |
| 2 | 参数或输入非法（越界方向、未知约定/布局、帧数不一致、采样率不匹配） |
| 3 | 数值稳定性或归一化校验失败（往返误差超限、条件数过高、NaN/Inf、削波写出） |
| 4 | 渲染指标超限（能量守恒误差 > 1 dB、串扰高于 -18 dB） |

## 结构

`aoa/types.py` 数据结构 · `aoa/harmonics.py` 一阶实球谐基 ·
`aoa/normalization.py` 约定互转 · `aoa/encoder.py` 编码 ·
`aoa/decoder.py` 解码矩阵（最小二乘/正则化/能量保持） ·
`aoa/render_paths.py` 扬声器与双耳路径 · `aoa/metrics.py` 指标 ·
`aoa/fixtures.py` 种子化夹具与 PCM16 · `aoa_cli.py` 命令入口 ·
`tests/test_pipeline.py` 自动化测试
