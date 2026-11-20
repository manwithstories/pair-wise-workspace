# relay — 多机位帧序列分片与表示层对齐

音乐节多机位后期流水线：把各机位采集的图像帧序列重排、按关键帧分片、
生成 360p/720p/1080p 对齐的表示层，并输出可往返校验的分片清单。
纯终端、纯本地，无外部服务。

## 构建与测试

```sh
brew install cmake ffmpeg catch2 openssl@3
cmake -S . -B build -DCMAKE_PREFIX_PATH="$(brew --prefix ffmpeg);$(brew --prefix catch2);$(brew --prefix openssl@3)"
cmake --build build -j4
ctest --test-dir build --output-on-failure
```

要求 C++20、CMake 3.28+、FFmpeg 7+（libavformat/libavcodec/libswresample）、
Catch2 3.x、OpenSSL 3.x。macOS arm64/amd64 均可。

## 演示

```sh
./build/relay demo --seed 42 --loss-burst 5 --bandwidth 1800,700,2400 \
  --segment-ms 2000 --ladder 360p,720p,1080p --verify
```

样例数据在 `./relay-work` 现场生成：每个机位的 PNG 帧序列（含机位、序号、
时间戳、像素格式、曝光元数据与 SHA-256）、到达日志（重复/乱序/突发丢失）、
时间戳回绕与序号重置。输出包括：

```
roundtrip_rate=1.000
keyframe_hit_rate=1.000
aligned_cut_error_frames=0
segment_hashes=<每段 64 位十六进制 SHA-256,...>
duplicates=…  reordered=…  lost_burst=…  peak_buffer_bytes=…
```

中断与恢复：

```sh
./build/relay demo --work ./relay-work --interrupt-after-segment 2
./build/relay resume --work ./relay-work --verify
```

`resume` 读取 `run.config` 复现原参数，复用已完成的阶段；恢复后的
`manifest_digest` 与不中断运行完全一致。

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 完成且清单往返校验通过（`--interrupt-after-segment` 正常结束也是 0） |
| 2 | 参数错误（未知选项、非法码率阶梯、非法数值） |
| 3 | 输入损坏或依赖不可恢复（SHA-256 失败、空/受阻轨道、清单校验失败、检查点损坏） |

## 架构

| 模块 | 职责 |
| --- | --- |
| `include/relay/types.hpp` | 统一微秒时间基准、轨道/包/错误模型、256 MiB 单轨缓存上限 |
| `src/fixture.cpp` | 本地样例生成：libavcodec 编码 PNG，写帧头与到达日志 |
| `src/frame_reader.cpp` | 解析帧流，逐帧 SHA-256 + libavformat/libavcodec 解码核对；失败定位机位与帧号 |
| `src/packet_reorder.cpp` | 32 位时间戳解包、去重、乱序窗口、迟到丢弃、纪元切分、缺口标记 |
| `src/timeline.cpp` | 拓扑排序轨道依赖图；空轨道/受阻轨道不外发分片 |
| `src/segmenter.cpp` | 关键帧切片、边界误差 ≤1 源帧、末段状态、不可寻址标记 |
| `src/ladder.cpp` | 滑动窗口带宽估计、共享切点、缓存上限与降级原因 |
| `src/json.cpp`, `src/manifest.cpp` | 严格 JSON、确定性序列化、未知字段保留、原子写入与往返校验 |
| `src/checkpoint.cpp`, `src/stage_cache.cpp` | 阶段级检查点、输入摘要失效判定、读/重排阶段缓存 |
| `src/pipeline.cpp` | 按依赖顺序调度各阶段，恢复可重试阶段，汇总报告 |
| `app/main.cpp`, `src/cli.cpp` | `demo` / `resume` 终端入口与退出码 |

数据流：帧解析与校验 → 包重排与缺失标注 → 统一时间线/轨道图 →
分片与码率对齐 → 清单与逐片段哈希。各模块共用同一 `Micros` 时间基准
与 `ErrorSink` 错误模型。

## 约束

- 只处理自有帧序列与本地生成数据，不联网、不启用设备采集
- 单轨缓存上限 256 MiB（`kMaxTrackCacheBytes`）；重排窗口按时间字节数统计
- 校验或清单往返失败时不发布清单，退出码 3
- 关键帧缺失分片标记 `unseekable`，不生成伪造索引
