# subtool — 字幕时间轴重采样与互转校验批处理器

把 SRT / WebVTT / ASS / TTML 多来源稿收敛到有理数（`math/big.Rat`）毫秒
时间轴，逐条打印起止帧号、往返偏差与丢条数，异常定位到行号。纯终端、
纯本地，无外部服务依赖。

## 构建与测试

```sh
go mod tidy && go test ./...
```

第三方依赖（`golang.org/x/text`）整体在 `vendor/`，离线可构建。

## 命令

```sh
# 生成多编码/坏时码夹具并跑 4x3=12 条互转通路
go run ./cmd/subtool selftest --out ./data --seed 20261115 --cues 2400

# 单文件/目录批量转换 + 帧号映射 + 反向校验 + JSON 报告
go run ./cmd/subtool convert --in ./data/mixed.srt --from srt --to vtt \
  --src-fps 24000/1001 --dst-fps 30000/1001 --round nearest \
  --overlap layer --jobs 4 --report ./reports/report.json

# 全互转矩阵校验，--json 写 stdout 供管道串联
go run ./cmd/subtool verify --in ./data --matrix all --max-drift-ms 1 --json
```

## 说明

- 帧率：`24`、`25`、`24000/1001`、`30000/1001`、`50`、`60`；29.97/59.94
  支持丢帧时码（`timeline.DropTimecode`）。
- `--round`：`floor|nearest|ceil`；`end_frame >= start_frame`，不足一帧
  标 `SUBFRAME` 不丢弃；跨帧率吸附偏差单列 `frame_snap_drift_ms`。
- `--overlap`：`layer`（双说话人双轨并存）/`trim`（裁剪，记录
  `overlap_trimmed_ms`）/`split`（边界切分）；同帧相接不合并。
- 编码嗅探：BOM → UTF-8 校验 → UTF-16 无 BOM 启发 → GB18030 回退，
  输出 `detected_encoding` 与 `encoding_confidence`，不可解码字节按行隔离。
- 隔离区：`>24h`、负值、零长、结束早于开始、缺字段、坏时码，均附
  错误码与行号；单文件失败不拖垮批次。
- 退出码：`0` 通过，`2` 参数/路径越界，`3` 隔离超阈值（`--max-quarantined`），
  `4` 反向校验失败，`5` IO 错误。`go run` 会把非零退出码打印为
  `exit status N`，直接运行编译后的二进制可拿到原始退出码。
- 输出白名单：转换产物只写 `--out` 目录内；`--report` 为显式授权的
  JSON 落盘路径。

## 结构

`cmd/subtool` 只编排；`internal/model` 单向被依赖；`internal/parse` 与
`internal/serialize` 互为反向校验；`internal/verify` 回读比对；
`internal/report` 出终端表格与 JSON；`internal/fixture` 生成样例。
