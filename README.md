# media-audit

六机位电竞现场采集素材的本地快速后期审计工具。读取 MP4/MOV，量化判断素材能否
进入剪辑、各机位如何对齐、哪些区段需要重采或人工复核。纯本地运行，无外部服务依赖。

## 技术栈

- C++20、CMake ≥ 3.28
- FFmpeg 7.x（libavformat / libavcodec / libavutil / libswresample）
- Catch2 3.x
- macOS（Darwin）本地终端

## 安装与构建

```bash
brew install cmake ffmpeg catch2
cmake -S . -B build -DCMAKE_PREFIX_PATH="$(brew --prefix)"
cmake --build build
```

## 生成自测夹具

夹具全部由代码确定性生成（seed 可复现），不引用任何外部素材：

```bash
./build/media-audit fixtures --seed 42
# 或
./scripts/make_fixtures.sh 42 ./generated
```

生成六类素材：

| 文件 | 覆盖能力 |
| --- | --- |
| `scene_normal.mp4` | 基准 CFR 素材 |
| `scene_vfr.mp4` | 可变帧率，帧间隔分布统计 |
| `scene_drift.mp4` | 音频渐进漂移（double 累加注入） |
| `scene_wrap.mp4` | ctts 注入 2³¹ 级 PTS 回绕 |
| `scene_corrupt.mp4` | 精确 NAL 损坏包（forbidden bit + 片身翻转） |
| `scene_truncated.mp4` | faststart 文件截断 |

## 运行测试

```bash
ctest --test-dir build --output-on-failure
```

## 端到端分析

单文件：

```bash
./build/media-audit analyze \
  --input ./generated/scene_corrupt.mp4 \
  --memory-mib 64 --jsonl ./out.jsonl --seed 42
```

目录批处理（单文件失败不影响其余，结果逐行写入 JSONL）：

```bash
./build/media-audit analyze --input ./generated --jsonl ./out.jsonl
```

### 输出指标（JSON / JSONL 字段）

- `av_sync_error_ms`：校正后音画同步误差 P95（ms）
- `recovery_rate_pct`：可解码损坏后包数 / 预期包数
- `damage_ranges`：损坏区间边界（包序号、字节偏移、时间、错误类别、恢复包数）
- `gop_interval_p95_ms`：关键帧间隔 P95
- `pipeline_latency_p95_ms`：首包读取至末包结果落盘的端到端延迟
- `peak_memory_mib`：进程驻留内存峰值

### 验收阈值（自生成样例）

| 指标 | 目标 |
| --- | --- |
| 同步误差 | ≤ 20 ms |
| 损坏包恢复率 | ≥ 90% |
| P95 延迟 | ≤ 2 s |
| 内存峰值 | ≤ 64 MiB |

阈值可用 `--max-sync-ms` / `--min-recovery-pct` / `--max-latency-ms` 覆盖。

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 分析完成且达到阈值 |
| 2 | 参数或输入错误 |
| 3 | 分析完成但指标未达阈值 |
| 4 | 内部处理错误 |

## 架构

```
scripts/make_fixtures.sh        确定性夹具生成入口
src/
  main.cpp                命令解析、批处理、退出码
  media_clock.hpp         统一时间基准、回绕展开、漂移/异常状态
  error_model.hpp/.cpp    跨模块错误分类、损坏区间、恢复记录、阈值判定
  media_types.hpp         包/帧/间隔统计轻量结构
  probe.hpp/.cpp          容器、轨道、码流结构探测
  gop_analyzer.hpp        关键帧间隔与 VFR 帧间隔分布
  damage_locator.hpp      损坏区间定位与恢复计数状态机
  stream_pipeline.hpp/.cpp 有界队列、背压、取消令牌、RSS 采样
  sync_recovery.hpp/.cpp  音画漂移联合估计与同步恢复指标
  fixture_gen.hpp/.cpp    夹具编码与二进制故障注入
  analyzer.hpp/.cpp       双线程流式分析引擎
  report.hpp/.cpp         JSON / JSONL 与终端摘要
tests/
  test_clock.cpp          时钟回绕与边界路径
  test_pipeline.cpp       管线、损坏定位、同步恢复
```

数据流向：生成脚本/真实素材 → 探测与分析模块（共用媒体时钟与错误模型）→
有界流式管线（背压、取消）→ 损坏定位与同步恢复 → JSON 与终端摘要。

## 约束

- 仅支持本地 MP4/MOV，不上传素材、不访问网络
- 磁盘输出不得覆盖输入（同路径写入被拒绝）
- 内存上限同时约束队列字节数与条目数，处理大文件时固定内存生效

---

# relay_timeline（Python 边缘分片统一时间线重建）

区域直播分发场景下，将同一节目经 HLS 与 DASH 分片跨边缘节点传输后，重建视频、
音频与事件标记轨的统一时间线，输出离屏帧序列与质量/对齐统计。纯本地、纯终端，
无图形界面、无外部服务依赖。

## 技术栈与安装

- Python 3.12、PyAV 13.x（FFmpeg 解封装/解码）、NumPy 2.x、soundfile 0.12.x
- 全部依赖仅安装于项目根目录 `.venv`：

```bash
python3.12 -m venv .venv
.venv/bin/python -m pip install "av>=13,<14" "numpy>=2,<3" "soundfile>=0.12,<0.13"
.venv/bin/python -m pip install -e .
```

## 模块划分（src/relay_timeline）

| 模块 | 职责 |
| --- | --- |
| `clock.py` | 有理数时基、33 位 PTS 回绕展开、线性漂移最小二乘估计 |
| `manifest.py` / `dash_manifest.py` | 本地 HLS/DASH 清单解析、越界防护、序号连续性、输入摘要 |
| `streamio.py` | 取消令牌、有界队列背压、视频/音频数据项 |
| `decode.py` | PyAV 流式解码、跨分片时钟归一化与回绕定位、截断检测 |
| `audio.py` | 音轨对齐、段内漂移回归、峰值/RMS 统计与 PCM 落盘 |
| `compose.py` | 多轨时间线合成、重复/倒退/断档定位、fail/gap-fill/skip 策略 |
| `render.py` | 离屏帧原子导出、时间索引、像素哈希、PSNR |
| `checkpoint.py` | 原子检查点、输入摘要校验、孤儿帧清理 |
| `stats.py` | 码率偏差、PSNR、清单对齐误差等汇总 |
| `pipeline.py` | 阶段串联、错误传播、检查点续跑 |
| `fixtures.py` | 确定性合成夹具（回绕/漂移/VFR/乱序/零长/截断/慢消费者） |
| `cli.py` | `demo` / `verify` 终端入口与退出码（0/2/3） |

## 验收演示

```bash
.venv/bin/python -m relay_timeline.cli demo --format hls,dash --duration 12 \
  --drift-ppm 85 --wrap-at 8589934500 --vfr --queue-size 4 --out .artifacts/demo
.venv/bin/python -m relay_timeline.cli verify --input .artifacts/demo \
  --target-bitrate 1800000
```

成功打印 `segments=... frames=... audio_rms=... bitrate_* ... quality_psnr_db=...
manifest_alignment_ms=... peak_memory_mb=... frame_hashes=...`，帧数约 250–400、
码率偏差 < 8%、对齐误差 < 40 ms，哈希数等于导出帧数，退出码 0。

故障与续跑：

```bash
.venv/bin/python -m relay_timeline.cli demo --fault truncated --queue-size 1 \
  --out .artifacts/broken     # 报告首个坏分片、恢复策略与错误计数
# 再次运行同一输出目录即按检查点续跑，摘要与干净运行一致
```

退出码：`0` 验收通过；`2` 输入/时序校验失败；`3` 不可恢复的处理或写入错误。

## 测试

```bash
.venv/bin/python -m unittest tests.test_pipeline -v
```

覆盖：回绕前后、漂移渐变、VFR、路径越界、截断/零长/缺失、fail/skip/gap-fill、
检查点续跑逐帧一致性与输入篡改检测。
