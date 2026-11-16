# h264gop

面向 Annex B H.264 裸基本流的 GOP 索引工具。项目仅使用 C++17 标准库，自研位读取与
Exp-Golomb 解码，不依赖 FFmpeg 或其他第三方库，也不解码像素。

## 构建与测试

```sh
cmake -S . -B build
cmake --build build -j
ctest --test-dir build --output-on-failure
```

## 命令

```sh
./build/gen_fixtures --out fixtures --cases 6
./build/h264gop probe fixtures/mbaff_multi_slice.h264
./build/h264gop index fixtures --json out/index.json
./build/h264gop verify fixtures/interlaced_mix.h264 --expect-progressive
```

- `probe`：打印序列、SPS/PPS、GOP、图像和片头摘要。
- `index`：递归索引目录中的 `.h264`/`.264` 文件，可选输出完整 JSON；父目录会自动创建。
- `verify --expect-progressive`：遇到 `frame_mbs_only_flag=0` 或场编码图像时退出 2，并打印
  按文件字节位置排序的首个违规偏移。

退出码：`0` 成功；`1` 读写或解析失败；`2` 逐进性校验失败；`3` 用法错误。

## JSON 结构

根对象包含 `files[]`、`sequences[]`、`anomalies[]` 与全局计数。每个序列包含
`sps`、`pps`、`gops[]`；每个 GOP 包含 `start_offset`、帧型计数、片数、隔行标志和
`frames[]`；每个图像包含合并后的 `slices[]`。所有偏移都是十进制文件绝对字节偏移。

## 夹具

`gen_fixtures` 生成六类夹具：

1. `baseline_3byte.h264`：三字节起始码。
2. `baseline_4byte.h264`：四字节起始码。
3. `multislice.h264`：同图多片与 FMO 映射。
4. `mbaff_multi_slice.h264`：MBAFF、4 个 IDR GOP、每 GOP 30 图、多片与 AUD 提示。
5. `interlaced_mix.h264`：顶/底场图像与场标志。
6. `sps_change.h264`：中途更换 SPS，切分新序列。

注意：H.264 每个图像至少需要一个片 NAL。因此 `mbaff_multi_slice.h264` 的 120 个图像不可能
只有 37 个片；该夹具实际输出 `frames=120 i=4 p=52 b=64 slices=137 anomalous=0`。验收描述中的
`slices=37` 与 `frames=120` 在 Annex B 语义下互相矛盾，工具不伪造该计数。
