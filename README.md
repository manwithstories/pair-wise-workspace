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
