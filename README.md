# media_batch

本地多机位现场采集整理工具。把带丢包、乱序的 RTP/RTCP 包流重排、对齐到统一
纳秒时间线、按关键帧切片、分析静音并跨机位去重，产出可交付剪辑团队的片段包。
纯本地、无网络/设备/外部素材依赖，所有样例数据由确定性生成器合成。

## 依赖

- C++20 编译器、CMake 3.28+
- FFmpeg 7.x（实际使用 libavformat / libavcodec / libswresample / libavutil；
  在 FFmpeg 9 上同样编译通过）
- Catch2 3.7+

```sh
brew install cmake ffmpeg catch2
```

## 构建与验收

```sh
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j2
ctest --test-dir build --output-on-failure
./build/media_batch selftest --seed 417 --json
```

自检退出码：`0` 指标达标，`1` 指标未达标，`2` 参数或构建配置错误。

## 命令

```sh
media_batch selftest [--seed N] [--json] [--out DIR] [--loss P]
media_batch process  --seed N [--json] [--out DIR] [--loss P]
media_batch help
```

- `--json`：在逐片段行与摘要行之外额外打印批次 JSON 摘要；配合 `--out` 时
  通过 `summary.json.tmp` 原子 rename 落盘，中断批次不会被当作完整交付。
- `--loss P`：覆盖合成丢包概率，用于复现 roundtrip 跌破 0.99 的退出码 1。

## 模块与数据流

```
采集容器 -> RTP 重排 -> 统一时间线 -> 轨道图排序 -> 切片 -> 静音分析 -> 指纹去重
capture   rtp_ingest   timeline      track_graph   segmenter silence   fingerprint
```

| 文件 | 职责 |
| --- | --- |
| `common.hpp` | 纳秒常量、重排窗口、错误模型、RTP 模数工具 |
| `capture.*` | 自描述小端二进制容器读写，截断头尾可恢复 |
| `rtp_ingest.*` | 按 SSRC/序列号抖动缓冲，回绕、丢包、重复、乱序深度 |
| `timeline.*` | RTCP SR 锚定纳秒时钟，缺 SR 估算并标注误差上界，漂移判定 |
| `track_graph.*` | 轨道依赖图、环检测、有界线程池、失败下游跳过 |
| `segmenter.*` | 关键帧切片，索引缺页重建，截断/缺口元数据 |
| `silence.*` | libswresample 转 FLT 后分声道 RMS 静音检测 |
| `fingerprint.*` | 规范化字节 SHA-256 + 分块指纹，精确合并/候选报告 |
| `fixture.*` | splitmix64 确定性夹具：乱序/丢包/损坏/截断/缺索引 |
| `pipeline.*` | 端到端编排、逐轨隔离、原子交付 |
| `main.cpp` | CLI、逐片段摘要、指标判定与退出码 |

## 关键设计约束

- 单轨损坏不阻断其他轨道；超大包（>16 KiB）与越界时间戳被拒绝。
- 重排窗口固定 64，超出窗口的迟到包标记为不可恢复缺口。
- 跨轨漂移超限时保留轨道但禁止静默合并；哈希相同才自动合并引用，
  哈希不同但指纹相近仅报告候选，绝不自动删除。
- 相同输入与参数产生相同排序、时间边界、片段哈希与摘要（确定性重跑）。
