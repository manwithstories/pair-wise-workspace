# segmanifest

`segmanifest` 是一个只依赖 Go 标准库的终端工具，用来把多档转码分片整理为可校验、可下发的清单。计划阶段会把各档独立切片边界吸附到共同时间网格，记录补偿量，并按同向连续超限规则产生不连续点和重排点。

## 构建与测试

```sh
go build -o bin/segmanifest ./cmd/segmanifest
go test ./...
go test -race ./...
```

完整演示也可直接运行 `./scripts/demo.sh`。

## 快速演示

```sh
./bin/segmanifest gen --out testdata/setA --seed 7 --representations 3 \
  --segments 120 --offset-ms -37 --jitter-ms 1.6
./bin/segmanifest plan --in testdata/setA --out out/setA.smf \
  --target 4s --tolerance 2ms
./bin/segmanifest emit --manifest out/setA.smf --rep r1080 --out out/paths.txt
./bin/segmanifest verify --manifest out/setA.smf --json
```

期望的计划摘要：

```text
representations=3 segments=360 grid_points=121 max_drift_ms=1.43 p95_drift_ms=0.92 resync_points=0 discontinuities=0
```

`emit` 输出 120 行，首行为 `seg/r1080/00001.m4s`。`verify --json` 中关键字段为 `expanded_paths=360 duplicate_paths=0 holes=0 overlap_ms=0 roundtrip_ok=true`。

## 子命令

- `gen`：生成 `fixture.json`。支持 `--seed`、`--representations`、`--segments`、`--target`、`--offset-ms`、`--jitter-ms`、`--holes`、`--version`。
- `plan`：读取夹具、建立共同网格、对齐、写入清单。支持 `--target`、`--tolerance`、`--template`、`--window`、`--merge`。
- `emit`：按表示 ID 展开分片路径。省略 `--out` 时输出到标准输出。
- `verify`：检查表示、路径唯一性、空洞、重叠、首尾时长和编解码往返。`--json` 输出 JSON；`--strict` 在最大偏差超过阈值时返回 1。
- `stats`：输出每档及全局的 max/mean/p95 偏差和重排点，支持 `--json`。

## 模板

默认模板为：

```text
seg/$RepresentationID$/$Number%05d$.m4s
```

支持的宏：

- `$Number$`、`$Number%05d$`：分片序号，可指定零填充宽度。
- `$Time$`：秒，保留三位毫秒小数，例如 `4.250`。
- `$TimeMS$` 或 `$Time%05d$`：毫秒，可指定零填充宽度。
- `$RepresentationID$`：表示 ID。
- `$Bandwidth$`、`$Bandwidth%08d$`：带宽，可指定零填充宽度。

未知宏、宽度为 0、展开后路径重名都会返回带稳定错误码的结构化错误。

## 窗口与合并

- `plan --window N` 只保留每档最近 N 个分片；分片序号不重排，`window.version` 递增。
- `plan --merge dirA,dirB` 按夹具的 `version` 合并多期目录。重叠分片以新版为准；同一版本中重叠内容不同则报 `merge.content_conflict`。
- 单档 10 万片的本地规划测试约需 0.07 秒，远低于 3 秒约束。

## 清单格式

文件以 `SMF!` magic 和一个版本字节开始，后续为记录序列：

```text
record := payload_length(varint) | type(1) | crc32(payload)(4) | payload
```

记录类型覆盖元数据、表示/分片、不连续点、重排点和偏差统计。解码会拒绝截断文件、CRC 不符、未知版本、超长长度前缀、未知记录类型和无效清单。编码按稳定顺序写入，因此相同参数和输入产生相同字节。

## 退出码

- `0`：成功。
- `1`：校验失败或 `--strict` 超阈值。
- `2`：参数错误或夹具/输入错误。
- `3`：清单解码失败。
- `4`：对齐、写入等子任务失败。

夹具和 `bin/`、`out/`、`testdata/` 均为生成物，已由 `.gitignore` 排除。
