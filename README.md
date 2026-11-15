# 时间线轨道模型与重叠消解器

离线多机位套底工具：统一时间线模型、扫描线冲突检测、优先级消解、
帧计划渲染与多格式互转。纯终端，无外部服务依赖。

## 环境

Python 3.11+、OpenTimelineIO 0.17.0、pytest 8.x。

## 模块

- `src/timeline/model.py` — 唯一事实源：Fraction 帧率、Clip/TimeRange/Track
- `src/timeline/scan.py` — 扫描线重叠检测（零长、相邻、同轨/跨轨冲突）
- `src/timeline/resolve.py` — 优先级消解，迭代至不动点（上限 32 轮）
- `src/formats/v1.py` / `v2.py` — 扁平版 / 带 source_range 与优先级版本
- `src/formats/otio_bridge.py` — 与 OTIO 双向转换、往返等价比对
- `src/render/plan.py` — 逐帧 active clip 计划与 plan 哈希
- `src/cli.py` — import / resolve / render / diff 子命令

## 用法

```bash
python tests/gen_fixtures.py                 # 生成 fixtures/
python -m src.cli resolve --input fixtures/tl_v2.json --format v2 \
    --policy priority --out resolved.json --json
python -m src.cli render --input resolved.json --fps 25 --out plan.json
python -m src.cli diff --a fixtures/tl_v1.json --b fixtures/tl_v2.json --format v1
pytest -q
```

## 退出码

- 0 正常；1 有未消解冲突或音视时长偏差超 2 帧；2 输入非法（结构化错误码，
  JSON 输出到 stderr，不抛异常栈）。

## 规则摘要

- 半开区间判定重叠，首尾相接不算冲突；零长片段单独计数、不参与消解。
- 跨视频轨：priority 大者胜，同级轨道索引小者胜；同轨：起点早者胜。
- 被压片段裁头/裁尾/中段劈开/整段让位，均记录原因；音频轨只检测不消解。
