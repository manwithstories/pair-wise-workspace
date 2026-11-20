# mediaqc（C++ 广播 PCM/WAV 批量核验与再封装）

区域广播播出前的多声道 PCM/WAV 批量核验工具：直接解析 RIFF/WAV 头并以
字节偏移定位损坏，把不同采样率映射到纳秒统一采样时钟，按显式轨道图排序
再封装，并输出与时间线对齐的机器可读 JSON 清单。纯本地运行，不联网，不
修改源素材，所有样例由固定种子脚本生成。

> 本目录同时保留历史 Python 项目 `src/media_batch/`（见仓库根 `README.md`），
> 与本 C++ 工程互不依赖；C++ 源码位于 `include/media/`、`src/`、`app/`。

## 技术栈与依赖

C++20、FFmpeg 7.x（libavformat / libavcodec / libavutil / libswresample）、
Catch2 3.x、CMake ≥ 3.28。本机为 Homebrew 安装的 FFmpeg 9.x，使用的
libav* API 与 7.x 兼容，支持 macOS arm64 与 x86_64。

```bash
brew install cmake ffmpeg catch2
```

## 构建、测试与验收

```bash
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build --parallel
ctest --test-dir build --output-on-failure
bash scripts/make_fixtures.sh build/fixtures
build/mediaqc batch --input build/fixtures --output build/out \
    --target-bitrate 4608000 --keyframe-gap-ms 250 \
    --damage-policy silence --resume
build/mediaqc verify --manifest build/out/manifest.json
```

### 退出码

| 码 | 含义 |
|----|------|
| 0 | 批次完成且清单核验通过，无损坏 |
| 2 | 输入存在**已记录但可恢复**的损坏（截断、CRC 页损坏、索引重建等） |
| 3 | 输入目录无可用 `.wav` |
| 4 | 轨道图依赖错误（缺失依赖、环、重复区段） |
| 5 | 不可恢复输入；只写 `failure-summary.json`，不生成完整清单 |
| 6 / 64 | 运行期异常 / 参数用法错误 |

### 终端指标（验收固定字段）

```
tracks=4 packets=264 damaged_ranges=6 truncated_bytes=7
target_bitrate=4608000 actual_bitrate=4608000 bitrate_deviation_pct=0
peak_dbfs=... rms_dbfs=...
manifest_alignment_max_ns=...
```

- 完整样例码率偏差 ≤ 1%；跨采样率转换的时间对齐误差 < 20834 ns
  （48 kHz 一个采样周期）。
- 损坏样例的 `damaged_ranges` 与 `fixture-params.json` 的生成参数一致
  （`damaged_6ch.wav` 恰好 2 个 CRC 损坏区段）。

## 模块划分

| 文件 | 职责 |
|------|------|
| `include/media/timeline.hpp` | 统一采样时钟、区段边界、统一错误模型（带源字节偏移） |
| `include/media/track_graph.hpp` | 输入块/轨道/依赖；缺失依赖、环、重复区段检测与拓扑排序 |
| `src/probe.cpp` | RIFF 头直解（字节偏移）、libavformat 包扫描、索引页侧车 |
| `src/damage_scan.cpp` | 截断、CRC 页损坏、关键帧间隔、解码错误与索引顺序重建 |
| `src/repack.cpp` | 容差内 PCM 直传或 libswresample 转换；静音/截断策略 |
| `src/manifest.cpp` | 流式峰值/RMS 计量与对齐时间线的 JSON 清单 |
| `src/checkpoint.cpp` | 断点恢复，校验源大小、mtime 与参数摘要 |
| `src/pipeline.cpp` | 单轨流水线装配 |
| `src/fs_util.cpp` | 路径逃逸防护、原子替换、slice-by-8 CRC32 |

## 设计要点

- **统一时间基准**：所有时间保留整数采样位置，跨采样率换算用精确有理数
  并保留余数；`TimelineMapper` 跟踪逐包最大残差，保证累计误差小于一个输出
  采样周期，纳秒映射不做浮点累积。
- **字节级损坏定位**：RIFF 块逐一遍历，短读与字段越界都记录观察到的字节
  偏移；索引页（`.wav.idx.json`）携带每页 CRC32，损坏精确定位到页偏移与
  影响采样范围。
- **索引恢复**：侧车缺失或不连续时，从数据块起点（最后可解码边界）顺序
  扫描重建；没有任何完整帧可解码时判为不可恢复，仅输出失败摘要。
- **截断处理**：仅保留最后一个完整采样帧，丢弃字节数入清单，偏移指向
  缺口下一字节。
- **再封装策略**：同布局且码率在容差内直接复用 PCM；需要采样率/声道转换
  时走 libswresample。损坏区段按 `silence`（填零）或 `truncate`（切除）
  处理，清单记录策略名与区段边界。
- **断点恢复**：每完成一轨原子写检查点；恢复时校验源文件大小、修改时间
  与参数摘要，任一变化即废弃旧检查点并打印原因。
- **安全**：`safe_join` 拒绝绝对路径与 `..` 逃逸并校验符号链接；输出目录
  先做可写探测；所有写盘经 `*.tmp.<pid>` / `*.part.<pid>` 临时文件再
  `rename` 原子替换。

## 性能与内存

200 MB 六声道 PCM（约 1748 万采样帧）在 Apple Silicon 上端到端约 0.64 s
（≈312 MB/s，超过 200 MB/s 目标）；200 MB 输入与小体积输入的峰值常驻内存
均约 17 MB，不随输入总时长增长——所有扫描、计量与再封装均按有界块流式
处理，索引只保留页级元数据。

## 样例（固定种子 12785）

| 文件 | 内容 |
|------|------|
| `normal_6ch.wav` | 48 kHz 六声道 3 s，索引完整 |
| `truncated_6ch.wav` | 尾部物理截断 7 字节，头部仍声明原始长度 |
| `damaged_6ch.wav` | 第 4、17 索引页各翻转 1 字节（2 个损坏区段） |
| `missing_index_6ch.wav` | 无索引侧车，触发顺序扫描重建 |
| `fixture-params.json` | 种子、页大小、截断字节数、损坏页号等参数 |
