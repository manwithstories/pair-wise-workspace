# subtool

海外剧集字幕的本地化时间轴工具。支持 SRT、ASS/SSA、WebVTT 的解析、格式转换、编码嗅探、有理数帧率换算、逐条往返校验与 JSON/终端报告。全程本地运行，不依赖外部服务，也不处理视频画面。

## 构建与快速验收

```sh
go build -o subtool ./cmd/subtool

./subtool fixtures gen --out testdata --count 50000 --seed 7
./subtool probe --in testdata/mixed_gb18030.srt --json testdata/probe.json
./subtool frame --in testdata/ep01.ass --fps 24000/1001 --json testdata/frames.json
./subtool convert --in testdata/ep01.ass --in-fps 24000/1001 \
  --out testdata/ep01.vtt --out-fps 30000/1001 --json testdata/convert.json
./subtool roundtrip --in testdata/stress_50k.srt --via vtt \
  --in-fps 25 --out-fps 30000/1001 --tolerance-ms 0 \
  --json testdata/roundtrip.json
```

要求 Go 1.22+，依赖仅为 `golang.org/x/text v0.16.0`。

## 命令

### fixtures gen

```sh
./subtool fixtures gen --out testdata --count 50000 --seed 7
```

生成 SRT、ASS、WebVTT、UTF-8 BOM、GB18030、Big5、Shift-JIS、重叠、无效条目、绘图指令和四类非法输入夹具。同 seed 产物逐字节一致。`stress_50k.srt` 与 `mixed_gb18030.srt` 按 `--count` 生成。

### probe

```sh
./subtool probe --in FILE [--json FILE]
```

输出格式、编码名、置信度、总条数、重叠组与无效条目。编码顺序为 BOM、UTF-8、GB18030、Big5、Shift-JIS。置信度低于 `0.7` 时写入 `encoding_low_confidence`，仍继续产出并返回 2。

### frame

```sh
./subtool frame --in FILE --fps 24000/1001 [--json FILE]
```

逐条输出 `index`、`in_ms`、`out_ms`、`in_frame`、`out_frame`。帧率以 `NUM/DEN` 有理数保存，帧号用整数四舍五入，不做浮点累计。

### convert

```sh
./subtool convert --in FILE --out FILE --in-fps 24000/1001 \
  --out-fps 30000/1001 [--speed-follow] [--json FILE]
```

默认 `align_only`：只解释/对齐帧号，不缩放墙钟时间。加 `--speed-follow` 后按输入/输出 fps 比例缩放时间轴并对齐目标帧边界。目标格式由输出扩展名推断。

### roundtrip

```sh
./subtool roundtrip --in FILE --via srt|ass|vtt \
  --in-fps 25 --out-fps 30000/1001 --tolerance-ms 0 \
  [--speed-follow] [--json FILE]
```

写入中转格式后立即重新解析，逐条比较起止毫秒、文本和帧号。VTT/SRT 保持毫秒精度；ASS 按 10ms 量化，`quantized_max_ms` 应为 0..5，且不视为往返错误。

## 数据规则

- 所有时间在内存中统一为毫秒整数，区间为左闭右开 `[in_ms, out_ms)`。
- 稳定按起始时间排序；连接重叠条目写入 `overlap_group`，全部保留。
- `end_ms <= start_ms` 标记 `invalid_cue` 并计入 dropped。
- ASS 绘图指令输出到 SRT/VTT 时标记 `unsupported_ass_drawing` 并计入 dropped，禁止静默删除。
- 超过 99 小时、SRT 截断、缺 `[Events]`、Dialogue 字段不足均返回稳定错误码和已解析条数。
- 单文件上限 200MB，超限直接拒绝。

## 退出码

| 退出码 | 含义 |
|---:|---|
| 0 | 全部通过 |
| 1 | 往返超差、文本不一致或有未说明丢条 |
| 2 | 编码低置信度，但已产出 |
| 3 | 输入非法、解析中断或 I/O 失败 |
| 4 | 参数错误 |

## 目录

```text
cmd/subtool/              子命令、参数校验、退出码
internal/subtitle/        Track/Cue、三种解析器、序列化器、时间码
internal/encsniff/        BOM/UTF-8/多编码嗅探与置信度
internal/timebase/        有理数帧率与两种换算模式
internal/verify/          逐条漂移、文本与 dropped 校验
internal/report/          JSON 与终端表格
internal/fixtures/        确定性夹具生成
```

运行测试：

```sh
go test ./...
go test -race ./...
go vet ./...
```

生成物 `testdata/` 与二进制 `subtool` 已在 `.gitignore` 中排除。
