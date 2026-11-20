# media_tool — 多声道 PCM/WAV 增量转封装工具

把比赛采集端产生的 PCM/WAV 多声道原始采样块，整理为适合移动网络低延迟
传输的媒体包（WAV / Matroska）与带时间戳的清单。纯本地、纯终端、无外部
服务依赖；全程 **PCM 直通，不做解码/重编码**，样本字节不发生改变。

## 技术栈

- C++20、CMake ≥ 3.31
- FFmpeg ≥ 7.1（libavformat / libavcodec / libavutil / libswresample）
- Catch2 ≥ 3（单元与端到端测试，夹具由 C++ 自生成）

安装依赖（macOS）：

```sh
brew install ffmpeg catch2 cmake
```

## 一键验收

```sh
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j
./build/media_tool selftest --generate-fixtures --queue-blocks 8 \
    --target-kbps 256 --out build/demo
ctest --test-dir build --output-on-failure
```

或直接：

```sh
./scripts/verify.sh
```

成功输出包含：

```
files=4 recovered=2 rejected=1 queue_peak_blocks<=8 ...
target_kbps=256 actual_kbps=256±2 bitrate_error_pct<=1.0
peak_abs=32767 rms=... clipped_samples=...
manifest_start_sample=0 manifest_end_sample=... alignment_error_samples=0
```

退出码约定：

| 码 | 含义 |
|----|------|
| 0 | 构建、处理与统计全部通过 |
| 1 | 验收不符 |
| 2 | 参数或依赖错误 |
| 3 | 输入损坏且无法恢复 |

## 架构

数据由解复用器逐块交给轨道映射器，再交给转封装器；统一时间基准与错误
模型贯穿全部模块。

```
include/mediacore/
  time.hpp          统一采样刻度（SampleTick）与时间范围
  error.hpp         跨模块错误码、Result<T>、可恢复/不可恢复分类
  pcm.hpp           PCM 格式、音频块（含 Normal/Corrupt/Gap/Duplicate 分类）
  path_guard.hpp    工作区内路径校验，拒绝路径穿越/符号链接逃逸
  wav_demuxer.hpp   增量 WAV 解复用、索引扫描重建、尾部与中段恢复
  bounded_queue.hpp 固定容量阻塞队列（背压与取消原语）
  track_mapper.hpp  声道布局驱动的命名轨道映射与去交错
  remux_muxer.hpp   FFmpeg WAV/Matroska 封装，PCM 直通，原子临时文件
  quality.hpp       连续性/峰值/RMS/削波/字节与码率偏差统计
  manifest.hpp      JSON 清单（有效/跳过范围、轨道时间线、质量统计）
  remux_pipeline.hpp 分阶段流水线与批次隔离
  fixture.hpp       确定性样例夹具生成器（不引用外部文件）

src/                各模块实现
app/main.cpp        终端命令入口（selftest / process）
tests/              Catch2 单元与端到端测试
scripts/verify.sh   一键构建+验收
```

### 流水线与背压

解复用线程把音频块推入固定容量 `BoundedBlockQueue`；队列满时阻塞上游，
慢速写入下内存占用严格有界，`cancel()` 后所有阻塞立即解除、生产者及时
停止读取。消费线程完成轨道映射、逐轨封装、质量统计与清单写入。

### 恢复模型

- **截断头部 / 不支持格式 / 扫描超限**：不可恢复，拒绝该文件（批次继续）。
- **尾部不完整样本**：保留此前完整样本，丢弃不足一帧的悬空字节。
- **中段损坏（MCOR）/ 时间戳缺口（MGAP）/ 块重复（MDUP）**：输出对应
  分类块，记录跳过采样区间与可恢复错误码，时间线保持单调。
- **索引缺失**：顺序扫描重建，超过 `scanLimitBytes` 返回可诊断错误。
- **写入失败**：清理全部 `.part` 临时文件，不产生看似成功的清单。

清单与质量统计共享同一个有效 `SampleRange`，保证时间线与指标覆盖完全
一致的样本集合。

## 单文件处理

```sh
./build/media_tool process --input path/to/input.wav --out build/out \
    --container wav --queue-blocks 8
```

## 约束与边界

- 仅支持有符号 16bit little-endian PCM（采集端主格式）。
- 所有输入/输出路径限制在指定工作目录内，拒绝 `..` 穿越与符号链接逃逸。
- 支持 macOS arm64 与 x86_64；性能目标为 48kHz/8 声道持续流式处理，
  峰值队列不超过配置容量。
