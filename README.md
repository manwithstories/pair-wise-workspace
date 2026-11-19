# media_repair

互动战术训练回放片段的本地修复工具链：解析带长度前缀的图像包序列，去重与
重排，修复采集时钟，用 FFmpeg 校验可解码性，重建可随机访问的关键帧索引，
并输出逐片段摘要、往返差异与损坏数据的原始字节偏移。

纯终端工具，无图形界面、无网络、无独立服务。所有样例数据由程序按固定随机
种子本地生成。

## 环境依赖

macOS（Apple Silicon 或 Intel），Apple Clang，CMake 3.28+，FFmpeg 7+，
Catch2 3.x：

```sh
brew install cmake ffmpeg catch2
```

## 构建与验收

```sh
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build

./build/media_repair generate --seed 42 --frames 1200 --loss-burst 7 \
  --duplicate-rate 0.08 --reorder-window 16 --out build/sample.mfs
./build/media_repair repair --input build/sample.mfs \
  --segment-frames 120 --json build/result.json
ctest --test-dir build --output-on-failure
./build/media_repair verify --input build/sample.mfs --report build/result.json

./scripts/acceptance.sh        # 含自造损坏样例的完整验收
```

`repair` / `verify` 输出 `roundtrip_match_rate`、`keyframe_index_hit_rate`、
`segment_hash_match_rate`，目标分别为 ≥ 0.99、≥ 0.98、= 1.00；同时输出
`repaired_frames`、`duplicate_packets`、`lost_ranges`、`decode_errors` 与
`first_error_offset`。损坏样例会报告偏移并继续处理后续可恢复片段。

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 验证通过 |
| 1 | 验收指标未达标 / 报告与重跑结果不一致 |
| 2 | 参数错误或输入格式不可恢复（含文件头截断、找不到文件、报告缺字段） |
| 3 | 不可恢复的内部错误（如 I/O 失败、未捕获异常） |

`generate` 的参数错误同样返回 2；写盘失败返回 3。

## 命令行

```
media_repair generate --seed N --frames N --loss-burst N --duplicate-rate R \
                      --reorder-window N --out FILE
                      [--width N] [--height N] [--keyframe-interval N]
                      [--loss-events N] [--timestamp-defects N]
                      [--frame-rate N] [--arrival-gap N ...]
media_repair repair   --input FILE --segment-frames N --json FILE
                      [--max-segments N] [--max-frame-bytes N]
                      [--max-total-bytes N] [--reorder-window N]
media_repair verify   --input FILE --report FILE
media_repair inspect  --input FILE
media_repair damage   --input FILE --kind KIND --out FILE
```

`--kind` 取 `checksum|length|header|tail|empty|magic|required`，从未损坏的
片段派生出对应损坏样例，并回报被破坏记录的原始偏移（验收脚本以此为基准）。

- `generate`：本地合成片段。丢帧按 `--loss-events × --loss-burst` 注入；
  乱序位移不超过重排窗口，重复按概率触发，另含到达停顿与时钟缺陷。
- `repair`：完整流水线，写出精简终端摘要与完整 JSON 报告。
- `verify`：从源文件重跑一遍，与报告中的指标逐项比对，防止陈旧报告通过。
- `inspect`：快速列出记录数与损坏记录偏移，便于定位原始字节位置。

## 传输格式（MFS1）

所有整数为大端。文件由 32 字节文件头与记录序列组成。

```
文件头 (32B): magic "MFS1" | u16 version | u16 reserved | u32 flags
              | u64 stream_id | u32 nominal_hz | u32 reserved | u32 reserved

记录: magic "MFP1" | u32 body_length | body

body: u16 meta_version | u16 meta_flags | u64 frame_seq
      | u32 capture_clock | u32 capture_hz | u32 width | u32 height
      | u8 codec | u8 field_count | u16 reserved
      | field[field_count] | u32 payload_length | payload | sha256[32]

field: u16 id | u8 kind (0=required, 1=optional) | u8 reserved | u32 length
       | value[length]
```

- 已知字段：`0x0001` camera_id、`0x0002` link_type、`0x0003` capture_note、
  `0x0004` arrival_us。
- **未知必需字段**（kind=0 且 id 未识别）导致整包拒绝，记录偏移；
  **未知可选字段**原样保留，解析后重新编码逐字节一致。
- `capture_clock == 0` 或 `meta_flags` 置位 `kFlagClockMissing`（bit 1）表示
  时间戳缺失；bit 0 为关键帧标记。
- 每条记录的 `sha256` 覆盖 payload。校验失败只丢弃该记录。

## 流水线

1. **解析**（`packet_reader` / `metadata_codec`）：长度前缀记录、版本化元数据、
   校验和。魔法错位、长度越界、截断记录都会记录偏移并在下一条记录边界重同步，
   因此损坏记录之后的片段仍然可恢复。单帧大小与累计字节数有硬上限。
2. **重排去重**（`reorder_buffer`）：按序号去重，连续前缀立即释放；乱序在窗口内
   吸收，超窗空洞放弃并记录序列区间，迟到包按原始偏移报 `out_of_window`。
   窗口依据到达间隔相对标称帧间隔自适应扩张，平时指数衰减回基准；待缓冲包数与
   已释放序号簿记均有上限。
3. **时间戳修复**（`timestamp_repair`）：从观测间隔稳健估计帧节拍；32 位时钟按
   半周期就近解卷绕；重复/倒退钉在期望槽位；缺失值由相邻有效帧按序列距离插值；
   真实丢帧间隔被保留。输出严格递增的微秒时间线，`original_ts_us` 保留原值。
   丢失区间按前后有效帧插值估算，不伪造帧内容。
4. **解码变换**（`frame_transform`）：每帧独立 FFmpeg 解码上下文，统一转 RGB24
   后取内容摘要，并核对声明分辨率；解码失败只记 `decode_error` 并继续。
5. **索引**（`keyframe_index`）：片段自最近关键帧开始，索引项含起止时间、90 kHz
   pts、来源偏移与逐片段摘要；`seek` 二分定位并回传时间基供校验。片段数超限时
   截断并标记，已建片段仍可查询。
6. **校验**（`pipeline`）：逐记录重编码与源字节比对（往返差异），逐片段重算摘要，
   对每个已解码帧做索引查询并核对时间基。

时间修复与索引共用 `Rational` 时间基准与可携带 `offset` 的错误模型
（`RepairError` / `MediaError`）。

## 源码结构

```
include/            公共接口
  media_types.hpp     帧、时间基准、索引项、错误类型
  byte_io.hpp         大端读写
  sha256.hpp          摘要
  metadata_codec.hpp  包体元数据编解码
  packet_reader.hpp   记录框架与重同步
  packet_writer.hpp   记录写入（往返编码）
  reorder_buffer.hpp  有界重排
  timestamp_repair.hpp时间戳修复与丢失区间估算
  frame_transform.hpp FFmpeg 解码/PNG 编码
  keyframe_index.hpp  随机访问索引
  pipeline.hpp        端到端流水线
  damage_builder.hpp  损坏样例构造器
  json_report.hpp     JSON 摘要
  sample_generator.hpp/rng.hpp  确定性样例生成
src/                对应实现，外加 main.cpp（CLI）
tests/              Catch2 测试（正常/损坏/边界路径）
scripts/acceptance.sh  生成样例并汇总验收结果
```

## 资源约束与确定性

- 默认单帧 ≤ 8 MiB、累计 ≤ 256 MiB、片段 ≤ 64、重排窗口 ≤ 256；
  均可用命令行参数进一步收紧。
- 生成器使用 xorshift64*，同一种子在不同机器上产生相同字节
  （`seed reproducibility` 用例逐字节比对）。
- 构建产物位于 `build/`，已被 `.gitignore` 排除。
