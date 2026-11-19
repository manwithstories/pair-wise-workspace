# fielddeliver

音乐节多机位现场交付流水线：裸码流解析、时间戳修复、随机访问索引重建、
音频归一、色彩元数据裁决、拓扑调度与可恢复检查点。

## 验收命令（项目根目录，macOS）

```bash
brew install ffmpeg
python3.12 -m venv .venv
.venv/bin/python -m pip install -e '.[test]'
.venv/bin/python -m fielddeliver demo --seed 37 --out /tmp/fielddeliver-demo
.venv/bin/python -m pytest -q
.venv/bin/python -m fielddeliver verify --manifest /tmp/fielddeliver-demo/manifest.json --json
```

所有时间戳统一为微秒（us）整数；模块仅通过 `model.py` 共享时间基准与错误模型。

## 模块架构

| 模块 | 职责 |
|------|------|
| `model.py` | 微秒时间基准、错误分类/退出码、轨道状态、清单与结果模型、原子写、路径约束 |
| `probe.py` | 清单校验、annex-B/ivf 裸码流扫描、时间戳回退/回绕/重复/丢包修复与显式缺口 |
| `index.py` | 随机访问点重建、旧索引越界失效重扫、逐项字节/PTS 跳转校验 |
| `audio.py` | 重采样、声道布局映射与下混、声明策略下的边界补齐、空音轨骨架 |
| `color.py` | inherit/override/error 三策略色彩元数据裁决与逐字段依据 |
| `timeline.py` | 依赖图拓扑排序、循环检测、统一微秒时间线与显式缺口并集 |
| `scheduler.py` | 拓扑分批并发、缓存/临时预算、指纹化可恢复检查点 |
| `sync.py` | 视频闪光与音频脉冲互相关，实测音画同步误差 |
| `pipeline.py` | 采集描述→解析修复→时间线图→索引/音频→报告的阶段编排 |
| `report.py` | 六项指标聚合、成功/损坏样例分组恢复率、退出码裁决、终端/JSON |
| `fixtures.py` | 成功/突发丢包/截断/编码器重启/空音轨自测样例生成（纯本地、可复现） |
| `cli.py` | `demo` / `verify` 子命令、参数解析与退出码 |

## 数据流

采集清单 → `validate_manifest` 字段/规模/路径校验 → `probe` 扫描与时间修复
→ `timeline` 拓扑图与统一时间线 → 按依赖并发执行视频 `index/seek_verify`
与音频归一/同步实测 → `color` 逐轨裁决 → `report` 聚合指标并原子写出。
每个阶段产出绑定「源文件哈希 + 清单哈希 + 参数指纹」的检查点，续跑时
指纹一致且校验通过才复用，任一变化即失效重跑。

## 输出（工作目录）

- `report.json`：六项指标、时间线、逐轨问题与色彩裁决依据
- `index/<track>.index.json`：可按时间/关键帧序号随机访问的索引
- `audio/<track>.wav`：归一采样率/声道布局后的代理音频
- `checkpoints/`：分阶段可恢复检查点

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 正常完成（含可恢复丢包） |
| 1 | 内部错误（流水线自身缺陷，优先于素材问题） |
| 2 | 输入/清单无效（缺字段、越界路径、超限、色彩 error 策略冲突） |
| 3 | 素材存在不可恢复损坏（截断、无法推断的时间戳缺口、解码失败） |

`UPSTREAM_FAILED` 仅作级联标记，不决定退出码；根因类别在上游轨。

