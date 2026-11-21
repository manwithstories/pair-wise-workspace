# mediaqc — 广播素材修复、再封装与归档核验

`mediaqc` 是一个纯本地运行的命令行工具，面向区域广播机构的日常素材处理：
批量检查纪录片、赛事回放与广告素材，修复时间戳，重建可随机访问的索引，
在中断后安全续跑，并在归档前独立核验输出内容。

所有处理都在本机完成，不联网，只读写用户指定的文件。样例与测试夹具全部
由程序生成，不引用任何外部素材。

## 构建

macOS（Apple Silicon 或 Intel）终端执行：

```sh
brew install ffmpeg cmake catch2
cmake -S . -B build
cmake --build build
ctest --test-dir build --output-on-failure
```

依赖：C++20、CMake ≥ 3.30、FFmpeg 7.x（libavformat / libavcodec /
libswresample）、Catch2 3.x。构建脚本同时识别 `/opt/homebrew`（Apple
Silicon）与 `/usr/local`（Intel）两个 Homebrew 前缀。

## 验收演示

```sh
./build/mediaqc generate --scenario boundary --out /tmp/mediaqc-sample
./build/mediaqc process  --input /tmp/mediaqc-sample --output /tmp/mediaqc-out \
                         --queue-frames 8 --checkpoint /tmp/mediaqc-state
./build/mediaqc verify   --input /tmp/mediaqc-out
```

`process` 打印验收指标：

```
frames_total=10000
frames_repaired=4
first_corrupt_offset=10632
peak_queue_frames=8
resume_ms=0
output_sha256=1ad4b986…ddab2a4   # 64 位十六进制
index_entries=9999
```

### 退出码

| 码 | 含义 |
|----|------|
| 0 | 完成且输出通过校验 |
| 2 | 输入或参数无效 |
| 3 | 发现素材损坏（已生成机器可读错误清单） |
| 4 | I/O 或恢复失败（含检查点与源数据不一致） |

## 子命令

| 命令 | 用途 |
|------|------|
| `generate --scenario NAME --out DIR [--frames N] [--seed N]` | 生成指定场景的交付样例（`normal`、`boundary`、`wraparound`、`duplicate`、`bad-checksum`、`out-of-order`、`truncated-tail`、`slow-consumer`）。 |
| `process --input DIR --output DIR [--queue-frames N] [--queue-bytes N] [--segment-frames N] [--checkpoint FILE] [--force] [--stop-after N] [--json]` | 修复、再封装并写索引；`--stop-after` 用于演示中断续跑。 |
| `verify --input DIR [--json]` | 独立复核输出：逐段摘要、索引偏移越界与整体摘要一致性。 |
| `query --input DIR [--track ID] [--time-ns N \| --frame N]` | 按时间或源帧号随机定位索引条目。 |

## 输入格式

一次交付是一个目录：

```
<delivery>/
  delivery.json                 # 版本化清单
  <track>.frames.jsonl          # 每帧一行元数据
  <track>.mkv                   # 该轨道的媒体负载
```

`delivery.json` 声明 `schema_version`、生产者与轨道列表。每个轨道给出
`id`、`kind`、`codec`、像素/采样参数、帧率 `rate`、设备时钟 `clock`
（`{"kind":"ticks","hz":90000,"bits":32}` 或 `{"kind":"ns"}`）、
`depends_on` 依赖、`required_fields`/`optional_fields` 以及两个文件名。
未知字段原样保留。

每帧元数据必须包含 `seq`、`capture_ticks`（缺失写 `null`）与 `sha256`
（64 位十六进制），可选 `keyframe`、`pix_fmt`、`width`、`height` 与任意
拍摄元数据。缺失必需字段报错，未知可选字段保留。

## 处理模型

```
帧读取器 → 校验器 → 统一时间线 → 轨道图 → 再封装/索引写入器
```

1. **轨道依赖图**（`src/track_graph.cpp`）以声明顺序打破平局的稳定拓扑序
   决定处理顺序；拒绝环路、缺失依赖、重复轨道，并报告首个出错轨道。
2. **时间线**（`src/timeline.cpp`）把每个设备时钟映射到统一纳秒栅格：
   - 32 位计数回绕按半程判定解卷；
   - 倒退或重复时钟强制前进一格并记录原因；
   - 缺失的采集时间按配置帧率推导，并在下一带时间戳帧重新锚定；
   - 设备时钟与声明帧率偏差超过容差时标记 `drift`，输出始终取栅格值，
     保证严格递增。
   每条被修改的时间戳都记录原值、修复值与原因。
3. **校验器**（`src/checksum.cpp`）逐帧比对 SHA-256；重复序号按幂等规则
   只保留首个并记录，损坏帧与尾部截断进入按偏移稳定排序的错误清单。
4. **再封装**（`src/index_writer.cpp`）原样流拷贝（不重编码）写入按轨分段
   的自包含 MP4，测量并回填每个输出包的字节偏移，生成 `index.json`：每条
   源帧记录时间、段号、偏移、大小、关键帧与源校验和。

### 背压与内存

读取与写入之间用有界缓冲（帧数上限 `--queue-frames` 与字节上限
`--queue-bytes` 双重约束）。缓冲满时读取阶段阻塞等待消费，队列峰值因此
不超过配置上限；10,000 帧样例下 `--queue-frames 8` 实测峰值恒为 8。
输入大小不设固定上限，但队列内存始终受这两个参数约束。

### 输出布局

```
<output>/
  index.json                    # 可随机访问索引
  manifest.json                 # 溯源清单
  media/<track>.segNNNNN.mp4    # 按轨分段的独立可寻址容器
```

分段让续跑变得诚实：中断后留下的是若干个已经关闭、可独立摘要与定位的
片段，而不是无法追加的半截容器。

## 检查点与续跑

`--checkpoint FILE` 记录输入摘要、配置摘要、每轨已消费帧数、每条轨迹时间线
状态以及每个已提交段的摘要。恢复时：

* 输入摘要、配置摘要必须匹配；
* 每个已提交段必须仍然存在且摘要一致，否则拒绝（不重用不完整输出）；
* 所属运行已完成的检查点一律拒绝；
* 成功完成后检查点被消费，避免被再次复用。

`resume_ms` 打印续跑起点校验到完成所用的整数毫秒。

## 错误模型

每个错误（`include/media/types.hpp` 的 `Error`）包含稳定错误码、人类可读
消息、轨道、以及可定位的字节偏移。错误清单按偏移与错误码稳定排序，
类别区分 `damage` / `order` / `truncation` / `io`，全部可机器读取。

## 测试

`tests/` 覆盖时间线修复、依赖图、交付清单与帧元数据解析、样例生成可复现性、
校验与索引偏移、边界场景到退出码的端到端流水线，以及真实二进制的命令行
契约。所有样例由 `tests/fixture_factory.*` 与 `src/generate.cpp` 现场生成。
