# mediacore_audit — 离线多轨 HLS/DASH 媒体审计

纯本地、无外部服务依赖的多轨分片审计工具。统一有理数时间基准、
FFmpeg 码流探测、有界并发依赖图、音频重采样规划、色彩元数据核验、
逐片段 SHA-256 与可定位可恢复的错误模型。

## 依赖

```sh
brew install cmake ffmpeg catch2
```

- C++20、CMake ≥ 3.28
- FFmpeg（libavformat / libavcodec / libswresample / libavutil）
- Catch2 3.x

## 构建

```sh
cmake -S . -B build -DCMAKE_PREFIX_PATH="$(brew --prefix)"
cmake --build build
```

支持 Apple Silicon 与 Intel；不访问网络，样例媒体由本机 ffmpeg 生成。

## 生成样例

```sh
./build/fixture_gen --out build/fixtures
```

生成内容：

- `master.m3u8` / `video.m3u8` / `audio.m3u8`：健康 HLS 多轨分片
- `broken.mpd`：DASH 清单，含尾部损坏分片与声明但缺失的分片
- `boundary.m3u8`：无法整除到统一时基的边界时间戳
- `zero.m3u8` / `empty.ts`：零长度区段
- `tracks.json`：逻辑轨道与期望规格

## 运行审计

恢复模式（跳过损坏区段，标记轨道不完整）：

```sh
./build/media_audit \
  --manifest build/fixtures/master.m3u8 \
  --tracks build/fixtures/tracks.json \
  --recover --json
```

严格模式（遇到可定位媒体错误即停）：

```sh
./build/media_audit \
  --manifest build/fixtures/broken.mpd \
  --tracks build/fixtures/tracks.json \
  --strict --json
```

### 退出码

| 码 | 含义 |
|----|------|
| 0  | 严格校验通过 / 恢复模式成功完成 |
| 2  | 检测到可定位媒体错误 |
| 3  | 参数或构建配置错误 |

### 关键指标

- `roundtrip_consistency`：清单规范化往返的字段一致率（≥0.99）
- `keyframe_index_hit_rate`：关键帧索引命中率（≥0.95）
- `segment_sha256_count`：成功计算的逐片段哈希数（≥4）
- `corrupt_ranges`：定位到的损坏/零长度区段数（≥1）
- `incomplete_tracks`：不完整轨道数（≥1）

重复运行相同输入产生字节一致的 JSON 摘要（键有序、无时间戳随机性）。

## 测试

```sh
ctest --test-dir build --output-on-failure
```

9 个 Catch2 测试套件覆盖：有理数时间、错误模型、路径约束、JSON
往返、HLS/DASH 解析、FFmpeg 探测、多轨时间线、依赖图、音频规划、
色彩元数据、SHA-256 与端到端流水线。

## 架构与模块

```
include/mediacore/      公开接口
  time.hpp              整数刻度有理数时基，精确换算，拒绝静默舍入
  error.hpp             可定位、可恢复错误模型与错误码
  manifest.hpp          HLS/DASH 领域模型、解析与往返比较
  probe.hpp             libavformat 流/时间戳/关键帧探测
  timeline.hpp          分片到显式多轨时间线的精确映射
  track_graph.hpp       五阶段依赖 DAG、环路检测、确定性拓扑
  normalize.hpp         libswresample 重采样规划与声道布局核验
  metadata.hpp          色彩范围/原色/传递函数/矩阵核验
  sha256.hpp             无依赖、有界内存的 SHA-256
  pipeline.hpp          依赖顺序编排与汇总指标
src/                    实现（每个源文件 <300 行）
app/                    media_audit 与 fixture_gen 命令入口
tests/                  Catch2 测试与夹具生成器
```

数据流：清单解析 → 统一时间线 → 探测（结果与错误区段写入轨道图）→
依赖图决定音频转换与校验顺序 → 机器可读摘要与逐片段哈希。

### 安全与资源边界

- 所有清单引用路径与输出路径都被约束在指定输入/输出目录内，
  拒绝 `..` 越界与远程 scheme。
- 有界线程池（`--workers`），无关轨道可并发、输出顺序确定。
- 单分片扫描包数与文件哈希块大小均有上限，限制单轨内存占用。
- 未知声道布局或声道数不匹配时保留错误，不猜测声道映射。
