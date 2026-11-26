# medcheck

多机位演出回传的本地访问单元、时码、传输窗口、分层恢复、字幕与 GOP 边界核查工具。纯 Go 标准库流水线加 `github.com/asticode/go-astisub/v0.45.0`，无外部服务。

## 验收

```bash
go mod tidy
go build ./...
go test ./...

for c in clean damaged recover; do
  go run ./cmd/medcheck demo --case "$c" --seed 17
done
```

也可运行：

```bash
./scripts/acceptance.sh
```

脚本会额外验证 recover 复跑的逐片段哈希一致，以及退出码 `2/3/4`。

## 终端状态码

| 退出码 | 含义 |
|---:|---|
| 0 | 报告完成；非严格模式下即使有坏段也输出完整报告 |
| 2 | 命令行参数错误 |
| 3 | `--strict` 模式检测到不可恢复坏段 |
| 4 | 本地索引、字幕或其他文件读写失败 |

常用参数：

```bash
go run ./cmd/medcheck demo --case damaged --seed 17 --strict
go run ./cmd/medcheck demo --case recover --seed 17 --workdir /tmp/medcheck --json
```

## 稳定输出字段

- `roundtrip=100.00%`：48 条 SRT 与 WebVTT 字幕的文本和毫秒边界均往返一致。
- `keyframe_hit=12/12`：12 个 GOP/片段均有关键帧。
- `cues=48/48`：48 条有效字幕全部保留。
- damaged：`duplicates=6`、`reordered=9`、`burst_lost=4`、`bad_crc=1`、`damaged_segments=1`。
- recover：`recovered=4`，并输出 12 条 `segment_hash segment=NN sha256=<64-hex>` 和汇总 `segment_sha256=<64-hex>`。

相同 `--seed` 的逐片段哈希可重复；指定 `--workdir` 时，索引先写临时文件、fsync 后原子 rename。已存在且校验通过的索引会在复跑时显示 `index resumed=true`。

## 包结构与数据流

```text
采集包头 ingest/packet.go
  -> 按 camera/stream/seq 去重、重排、缺口与背压 ingest/window.go
  -> 1x/2x/4x 扇出和带宽选择 fanout/rate.go
  -> 交叉 XOR  interleaving 独立恢复 recover/layer.go
  -> 共享 fault/model.go 错误区间
  -> media/unit.go 访问单元边界/CRC/关键帧/GOP 头解析与 magic resync
  -> subtitle/codec.go + parse.go 容错 SRT/WebVTT 和 astisub 往返
  -> index/gop.go GOP 索引、片段哈希、原子 checkpoint
  -> boundary/check.go 字幕/GOP/分层边界交叉核验
  -> pipeline/run.go 编排，cmd/medcheck/main.go 终端出口
```

`clock/base.go` 提供 24、25、30、30000/1001 有理数帧率换算、DF 跳号校验、跨午夜 wrap、时码倒退检测和 LTC/VITC 容差对齐。

## 夹具覆盖

样例由 `pipeline/fixture.go` 用固定 seed 确定性生成，覆盖：

- 带宽从 5 Mbps 骤降到 700 kbps；
- 6 个重复包、9 次相邻乱序；
- 连续 4 包突发丢失；
- 包头坏 CRC；
- 非法 DF 时码、跨午夜和倒退时码；
- 无时间行、倒序时间、空文本截断和重叠字幕；
- 截断访问单元头后的 magic resync；
- 故障向字幕重叠区间和 GOP/片段边界传播。

采集、时基和恢复子系统均有独立测试；`pipeline` 和 `cmd/medcheck` 有端到端及退出码测试。

## 资源边界

- 输入上限：`media.MaxInputBytes = 2 GiB`。
- 单条字幕上限：`subtitle.MaxCueBytes = 1 MiB`。
- 重排窗口容量有界，满窗对非连续包返回 `ErrBackpressure`。
- 访问单元和传输包均只解析固定包头并跳过/交给恢复层处理载荷，不执行媒体解码。
- 输出目录中的 `out/` 已被 git 忽略；仓库不提交二进制、临时索引或密钥文件。
