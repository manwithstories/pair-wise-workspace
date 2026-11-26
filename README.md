# mediad — 弱网互动音视频时间线修复与恢复核验

纯本地 Go 1.25.6 / Cobra v1.8.1 命令行项目。MP4/MOV 盒扫描与 `stts`、`ctts`、`stss`、`stco/co64`、`stsz`、`elst` 结构读写使用 `github.com/abema/go-mp4 v1.7.3`。不依赖解码器、在线服务或设备权限。

## 一键验收

```bash
brew install go@1.25
export PATH="$(brew --prefix go@1.25)/bin:$PATH"
go version # 必须为 go1.25.6
go mod download
go mod tidy
go build ./...
go test ./...

go run ./cmd/mediad selftest \
  --seed 42 --duration 60s --loss 0.03 --burst 7 \
  --out "$TMPDIR/mediad-run"

go run ./cmd/mediad verify \
  --input "$TMPDIR/mediad-run/repaired.mp4"
```

也可运行 `./scripts/acceptance.sh`。

固定种子下，`repaired.mp4` 的 `output_sha256`、指标和终端汇总可重复。当前 60 秒样例的达标范围为：

- `sync_max_ms <= 20`
- `loss_recovery_pct >= 99`
- `latency_p95_ms <= 180`
- `index_seek_miss = 0`
- `boundary_mismatch = 0`

## 退出码

编译后的二进制保留精确退出码（`go run` 失败时外层 `go` 命令可能统一返回 1）：

- `0`：处理和全部阈值达标。
- `2`：输入损坏到不可恢复，例如盒边界越界且无法 salvage。
- `3`：处理完成，但独立核验或指标阈值未达标。
- `1`：内部错误。

## 常用命令

```bash
# 生成正常/故障/恢复样例，输出 repaired.mp4、索引和 JSON 报告
go run ./cmd/mediad selftest --seed 42 --duration 60s \
  --loss 0.03 --burst 7 --out ./mediad-run

# 独立解析 repaired.mp4，重建索引并交叉校验边界
go run ./cmd/mediad verify --input ./mediad-run/repaired.mp4
go run ./cmd/mediad verify --input ./mediad-run/repaired.mp4 --json
```

`selftest` 会分别汇总：

- `normal_samples`：`normal.mp4`、QuickTime brand 的 `normal.mov`。
- `damaged_samples`：组合故障和六类独立故障。
- `recovered_samples`：每个样例对应的修复输出与恢复统计。

## 自生成故障

夹具不引用外部素材，覆盖：

1. 截断 `mdat`：严格扫描报告首个异常盒偏移，salvage 模式钳制到 EOF。
2. 时间戳倒退/缺失：损坏 `ctts`/`stts` 语义，统一共享时钟后修复。
3. 样本偏移损坏：改写 `stco` 表项，恢复和边界核验都会受到影响，随后重建偏移。
4. 音频采样率/声道数冲突：保留原值与修复依据；证据不足时轨道标记失败。
5. 色彩元数据冲突：按分辨率等证据统一；HDR/位深等不可推断冲突标记轨道失败。
6. `stss` 缺失：从同步样本 marker 重建随机访问索引。

突发丢包使用确定性 PRNG 和连续 `burst` 模型。丢包先尝试重传，再从最近的已验证同步点恢复；报告丢包、恢复、未恢复包和重传次数。未验证索引不会写入 `.index.json`。

## 数据流与包结构

```text
盒边界扫描 internal/mp4/scan.go
  → MP4 时间表/偏移解析 internal/mp4/read*.go
  → stts/ctts/elst 修复 internal/mp4/repair.go
  → 共享有理数时钟 internal/clock/fanout.go
  → 音频/视频元数据核验 internal/audio internal/video
  → 多码率共同关键帧 internal/ladder/fanout.go
  → 单轨检查点与突发恢复 internal/recover/track.go
  → 随机访问索引 internal/index/rebuild.go
  → 边界交叉核验 internal/verify/boundary.go
  → 指标 internal/metrics/measure.go
  → 确定性夹具 internal/fixture
  → Cobra 入口 cmd/mediad/main.go
```

共享模型位于 `internal/media`：`Rational/Clock` 提供无浮点有理数换算，`Fault` 携带轨道、盒类型、字节区间、时间跨度、原值和修复依据。

## 安全边界

- 单文件上限 `2 GiB`，轨道上限 `32`，盒嵌套深度上限 `64`。
- 扫描阶段拒绝小于盒头、64 位长度溢出和越过文件末尾的盒尺寸，并报告首个异常字节。
- 所有计数和区间运算先做 `int64`/`uint64` 溢出检查。
- 大表项数量按 2 GiB 输入做防御性上限；元数据 JSON box 限制为 1 MiB。
- 核验只读取固定同步 marker 前缀，不调用媒体解码器。
- 仓库不提交构建产物、密钥或连接信息。
