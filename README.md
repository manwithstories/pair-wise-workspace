# mp4pack

面向无人机弱网回传的 fMP4 归档工具。项目只依赖 C++17 标准库，自行解析 ISO BMFF box，
不转码、不联网，也不启动外部服务。

## 构建与演示

```bash
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j
python3 scripts/gen_fixtures.py --out fixtures

./build/mp4pack index fixtures/drone_a --out out/index.json
./build/mp4pack validate --index out/index.json --init fixtures/drone_a/init.mp4 --out out/validate.json
./build/mp4pack repack --index out/index.json --init fixtures/drone_a/init.mp4 --out out/repacked.mp4
./build/mp4pack report --index out/index.json --repacked out/repacked.mp4
```

演示夹具包含 12 个分片文件、11 个唯一 mfhd 序号、一个重复分片、3/4 文件乱序和缺失序号 7。
报告预期包含：

```text
segments_found=12
segments_after_repack=11
missing_sequences=[7]
seq_contiguous=false
tracks=2
video.max_deviation_ms=0.000
audio.max_deviation_ms=0.333
verdict=missing_segment
```

## 子命令

- `index <directory> --out <index.json>`：扫描目录中的 `init.mp4` 和分片，建立可复用的 JSON 索引。
- `validate --index <index.json> --init <init.mp4> --out <validate.json>`：逐项比较初始化段并检查分片问题。
- `repack --index <index.json> --init <init.mp4> --out <repacked.mp4>`：输出单初始化段、连续序号和单调 tfdt 的成片。
- `report --index <index.json> --repacked <repacked.mp4> [--out <report.json>]`：输出终端摘要，可选写稳定顺序 JSON。

## 处理规则

- 盒解析检查越界、零长度、64 位扩展长度、uuid 头和截断输入，错误信息带绝对字节偏移。
- 文件名按自然序扫描；mfhd 序号用于去重、乱序检测和断档报告。
- tfdt v0 结合上一片末 DTS 做 32 位回绕修正；tfdt v1 直接使用 64 位值。
- trun v0 的合成时间偏移按无符号读取，v1 按有符号读取，负值保留。
- 样本时长依次回落到 `tfhd` 默认值和 `trex` 默认值；仍缺失时分片标记为不可索引。
- 重打包按源序号排序，重写 mfhd 序号和 tfdt，按新 moof 长度重算所有 trun 数据偏移；
  tfdt 超出 32 位时自动升级到 v1。
- 输出先写临时文件，任何失败都会删除半成品。

## 退出码

| 退出码 | 含义 |
|---:|---|
| 0 | 全部通过 |
| 2 | 分片缺失、重复、乱序或不可索引 |
| 3 | 初始化段不兼容 |
| 4 | 盒解析或格式错误 |
| 5 | 输出写入失败 |

## 测试

```bash
ctest --test-dir build --output-on-failure
```

测试脚本会现场生成 v0/v1、回绕、正负合成偏移、缺片、重复、乱序、64 位盒长、uuid、
`avcC/hvcC` 和格式错误夹具，并核对 JSON、重打包结构与退出码。
