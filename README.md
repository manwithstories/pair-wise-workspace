# h264scan

`h264scan` 是纯本地、无第三方运行时依赖的 H.264 Annex B 码流体检工具。
它按 NAL 流式扫描输入，输出 SPS 关键字段、GOP 字节区间、帧类型统计、
场配对结果与完整性警告，并支持 JSON/CSV 落盘。

## 构建

```sh
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j
```

要求 C++17、CMake 3.20+、GCC 11+ 或 Clang 14+。仅使用 C++ 标准库。

## 命令

生成内置合成夹具（24 帧/GOP，默认 4 GOP）：

```sh
./build/h264scan synth --out samples/case_a.h264 \
  --gops 4 --slices 3 --interlaced pair --seed 7
```

分析单个文件并写 JSON：

```sh
./build/h264scan analyze samples/case_a.h264 --json out/case_a.json --verbose
```

递归分析目录并写 CSV 汇总：

```sh
./build/h264scan analyze samples --recursive --csv out/summary.csv
```

严格模式会在任意警告出现时返回退出码 1：

```sh
./build/h264scan analyze samples/case_a.h264 --strict
```

也可以一次执行 `./scripts/acceptance.sh` 完成构建、生成与验收检查。

`analyze` 支持多个文件或目录。单文件 JSON 的根节点就是报告；多文件 JSON
使用 `{"files": [...]}` 包装。

## 退出码

| 退出码 | 含义 |
|---:|---|
| 0 | 成功 |
| 1 | `--strict` 且存在警告 |
| 2 | 输入/读取/输出损坏或 I/O 失败 |
| 3 | 未找到有效 SPS |
| 4 | `synth` 参数非法 |

## JSON 字段

主要结构如下（数值为合成样例的典型值）：

```json
{
  "file": "samples/case_a.h264",
  "sps": {
    "profile_idc": 100,
    "level_idc": 41,
    "chroma_format_idc": 1,
    "width": 1280,
    "height": 720,
    "frame_mbs_only_flag": false,
    "log2_max_frame_num_minus4": 0,
    "pic_order_cnt_type": 0
  },
  "gops": [
    {
      "start_offset": 12403,
      "byte_length": 30592,
      "frames": 24,
      "i": 1,
      "p": 8,
      "b": 15,
      "field_pairs": 12,
      "slice_count": 72
    }
  ],
  "totals": {
    "frames": 96,
    "gop_count": 4,
    "slice_count": 288,
    "warnings": 0
  }
}
```

`start_offset` 指向 GOP 首条 NAL 起始码的第一个字节；`byte_length` 从该
偏移覆盖到 GOP 内最后一条 slice 的数据末尾。每个 GOP 的 `i + p + b`
等于 `frames`。

CSV 为文件级汇总，列为：

```text
file,nal_count,gop_count,frames,slice_count,i,p,b,field_pairs,missing_slices,incomplete_frames,warnings
```

## 实现要点

- Annex B 扫描支持三字节/四字节起始码与 trailing zero bytes，跨读块边界工作。
- 每个 NAL 在回调处理完后释放，不把整文件读入内存；重组器只保留有限活动帧窗口。
- 位读取器处理防竞争字节、`ue(v)`/`se(v)` 溢出与读越界；缩放矩阵增量和 POC 差值复用同一解码器。
- slice 按 PPS、`frame_num`、场标记与 POC 分组；上下场配对为一帧，并累计 `field_pairs`。
- GOP 在 IDR 或 POC 回退/帧号重置处切分。
- 未解码熵编码数据，因此缺片检测结合 `first_mb_in_slice` 覆盖检查与当前码流观测到的每帧最大 slice 数；发现缺口会增加 `missing_slices`、标记 `incomplete_frames` 并产生警告。
- 未知 NAL 类型跳过并计数；头部解析失败按截断/损坏 NAL 记警告，非严格模式继续分析。

## 验收

```sh
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release && cmake --build build -j
./build/h264scan synth --out samples/case_a.h264 --gops 4 --slices 3 --interlaced pair --seed 7
./build/h264scan analyze samples/case_a.h264 --json out/case_a.json --verbose
./build/h264scan analyze samples --recursive --csv out/summary.csv
./build/h264scan analyze samples/case_a.h264 --strict
```
