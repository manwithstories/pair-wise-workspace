# fMP4 Guard

面向云游戏试玩片段的本地 fMP4 检查与重打包工具。工程只用 C++17 和标准库，不依赖外部媒体文件或第三方解析库。

## 构建与测试

```sh
./build.sh
```

也可以手动分两步：

```sh
cmake -S . -B build -DCMAKE_BUILD_TYPE=RelWithDebInfo
cmake --build build --parallel
ctest --test-dir build --output-on-failure
```

构建产物位于 `build/`，不会进入交付内容。

## 演示

```sh
./build/fmp4gen --out fixtures/a --fragments 12 --drop 5,9 --dup 3 --tfdt mixed --seed 7
./build/fmp4tool validate fixtures/a/input.mp4 --json out/before.json
./build/fmp4tool repack fixtures/a/input.mp4 -o out/repacked.mp4 --tfdt-version 1 --renumber
./build/fmp4tool validate out/repacked.mp4 --json out/after.json
./build/fmp4tool compare out/before.json out/after.json
```

前一次校验会报告 `duplicate=[3]`、`missing=[5,9]` 并返回 1；重打包后的校验应连续并返回 0。`compare` 输出同时包含改前/改后的序号状态、初始化兼容性和每轨时间戳偏差。

`--fragments N` 表示目标为最终写出 N 个物理分片；生成器会在内部提前序列号，以补偿 `--drop` 和 `--dup`，因此示例中仍写出 12 个分片，但序列范围到 13。

## 子命令

```text
fmp4tool probe FILE [--json PATH]
fmp4tool validate FILE [--json PATH] [--init INIT_FILE]
fmp4tool repack INPUT -o OUTPUT [--tfdt-version 1] [--renumber]
fmp4tool compare LEFT RIGHT [--json PATH]
```

- `probe`：输出轨道初始化信息、分片序号及 `moof`/`mdat` 偏移。
- `validate`：检查序号重复、乱序、缺失，比较 `trex` 默认累计时长与 `tfdt`，并可和 `--init` 给出的初始化段逐字段比对。
- `repack`：保留初始化段字节，按索引重建 `moof`，统一输出 `tfdt` v1，重算 `trun.data_offset`，并流式复制样本到新的 `mdat`。
- `compare`：接受两份 JSON 报告，也接受两份媒体文件；JSON 模式给出改前/改后视图。

退出码：

- `0`：检查通过。
- `1`：检查未通过。
- `2`：参数或 IO 错误。
- `3`：box 或媒体结构解析失败。

## 关键行为

- 支持 32 位 `size`、64 位 `largesize`（`size == 1`）以及 `size == 0` 延伸到文件/父 box 尾部。
- 所有 offset 使用 `uint64_t`，元数据按 seek 读取；不会把 `mdat` 载入内存。
- box 嵌套深度最多 32 层，截断和越界错误包含字节偏移。
- 分片索引同时解析 `tfdt` v0/v1、`trun` v0 无符号和 v1 有符号 `data_offset`，并按 flags 跳过或读取样本级可选字段。
- `validate` 的缺失项既给出完整序号数组，也给出缺失区间和前后 `moof` 偏移。
- 每轨输出 `deviation_max_ticks`、`deviation_mean_ticks`、`deviation_max_ms`、`deviation_mean_ms`。
- 重打包采用稳定的 canonical `moof` 布局，因此二次重打包字节一致。

## 目录

```text
src/box_reader.*     大端读取与边界检查
src/box_tree.*       通用 box 树
src/init_segment.*   moov/trak/mvex/trex 初始化信息
src/fragment_index.* mfhd/tfdt/tfhd/trun 分片索引
src/validate.*       序号与解码偏差校验
src/repack.*         确定性重打包
src/report.*         JSON/文本报告
src/compare.*        JSON 报告比较
tools/mkfixture.cpp  带缺陷夹具 CLI
```

## 夹具选项

`fmp4gen` 还支持 `--trun mixed|0|1`、`--negative-offset`、`--large-mdat`、`--reorder`、`--codec`、`--timescale`、`--default-duration` 和 `--samples`，用于覆盖混合版本、负偏移、64 位 box 头和乱序场景。
