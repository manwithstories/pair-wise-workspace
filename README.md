# h264_audit

纯 C++17 的 Annex B H.264 裸码流审计工具。项目不依赖第三方库，不调用外部编解码器，也不解码或重建像素。`fixture_gen` 以程序方式生成多分片、PAFF/MBAFF、长 GOP、恢复点和截断尾部样本。

## 构建

```sh
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j
```

## 使用

```sh
./build/fixture_gen --out fixtures --frames 240 --slices 3 --interlaced-pair 2
./build/h264_audit scan fixtures/mixed_1080p.h264
./build/h264_audit scan fixtures/mixed_1080p.h264 --json out/report.json
./build/h264_audit gops fixtures/interlaced_paff.h264 --recovery-point --min-gop 8
./build/h264_audit selftest
tests/run_all.sh
```

`scan` 输出版本参数、显示尺寸、帧类型计数、分片数统计和 GOP 表。`gops` 支持恢复点边界与最小组长过滤。两者都支持 `--strict` 和 `--json <path>`。

## 模块

- `src/bit_reader.*`：去除防竞争字节，统一实现 `u(n)`、`ue(v)`、`se(v)` 和越界状态。
- `src/nal_split.*`：块式扫描三/四字节起始码，记录 NAL 的绝对文件偏移。
- `src/param_sets.*`：SPS/PPS、裁剪、色度/位深、POC 参数和 VUI 时序。
- `src/slice_header.*`：片头、场标志、POC 字段、参考列表与参考标记。
- `src/picture_order.*`：POC 类型 0/1/2、frame_num 回绕和 IDR 参考重置。
- `src/access_unit.*`：AUD/首宏块/类型边界、片字段一致性和场对合并。
- `src/gop_index.*`：IDR 或恢复点 GOP 边界、原始 NAL 区间与显示序帧类型串。
- `src/stats.*`、`src/report.*`：统计、终端表格和固定键序 JSON。
- `tools/make_fixtures.cpp`：生成全部样例，不读取外部素材。

## 退出码

- `0`：处理成功且无告警。
- `1`：参数错误、文件不可读或 JSON 无法写入。
- `2`：码流结构错误、末尾截断告警或严格模式不一致。
- `3`：自检断言失败。

JSON 顶层键固定为 `sps`、`pps`、`frames`、`gops`、`warnings`。每行 GOP 包含起始绝对偏移、字节长度、NAL 序号区间和帧类型串。
