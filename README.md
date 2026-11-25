# mediad

纯本地、终端形态的多轨媒体时间线修复与随机访问索引内核。所有夹具（三档 DASH、带两处
`EXT-X-DISCONTINUITY` 的 HLS、fMP4、RTP 轨迹、截断 box、缺字段清单、冲突包和丢包样例）
都由代码确定性生成，不读取外部素材或网络服务。

## 固定工具链

```bash
go install golang.org/dl/go1.25.6@latest
go1.25.6 download
```

`go.mod` 固定 `go 1.25.6` 与 `toolchain go1.25.6`。Go 1.25 的 `go mod tidy` 会删除与
`go` 指令完全相同的冗余 `toolchain` 行；验收脚本会在所有检查通过后把该固定行写回。

## 验收

```bash
go1.25.6 mod tidy
go1.25.6 build ./...
go1.25.6 test ./...

go1.25.6 run ./cmd/mediad demo --scenario clean --seed 73 --out "$TMPDIR/pf-clean"
go1.25.6 run ./cmd/mediad demo --scenario damaged --seed 73 --out "$TMPDIR/pf-clean"
go1.25.6 run ./cmd/mediad demo --scenario missing-init --seed 73 --out "$TMPDIR/pf-blocked"
```

也可运行：

```bash
./scripts/acceptance.sh
```

clean/damaged 的指标行：

```text
roundtrip=100.00%
keyframe_hit=100.00%
segments=12
hash_match=12/12
duplicate=24
burst_loss=4
bandwidth_steps=5
```

missing-init 稳定报告拓扑序中的首个阻塞节点：

```text
first_blocked=repair:track-02:init-segment
```

重复执行相同 clean 命令时，12 个片段的 SHA-256 保持逐片段一致；DAG 状态和已存在产物会
被跳过，而不是重复计数。

## 退出码

程序自身的契约是：

- `0`：完成，或按规则完整/部分恢复；
- `2`：输入损坏且拒绝（截断 box、缺字段、同序号异载荷冲突、不可恢复缺口）；
- `3`：依赖阻塞（例如缺少初始化片段）；
- `4`：参数、路径越界、超过 64 MiB 或本地读写错误。

注意：`go run` 会把子进程的非零退出码包装成 `go run` 自身的 `1` 并打印
`exit status N`；命令 stdout 仍打印准确的 `exit_code=N`。脚本会额外 `go build`
出直接二进制来校验上述精确退出码。

## 模块

- `internal/media`：有理数时基、tick 转换、32/16 位回绕、discontinuity 与 tfdt 回退证据、结构化错误码。
- `internal/manifest`：DASH/HLS 解析、规范化、确定性序列化、往返与首个不一致字段定位。
- `internal/ingest`：基于 `pion/rtp` 的 RTP 解析/序列化、去重、冲突检测、回绕重排和明确缺口。
- `internal/repair`：基于 `go-mp4` 的 fMP4 box 生成/校验、共同时钟对齐、带宽相关有界窗口和片段修复。
- `internal/index`：可随机访问索引、关键帧定位和结构化越界错误。
- `internal/flow`：稳定拓扑 DAG、输入哈希幂等键、重试、阻塞传播和断点续跑。
- `internal/store`：根目录 confinement、防路径穿越、64 MiB 限制、原子落盘和 4 并发信号量。
- `internal/metric`：SHA-256 与确定性输入哈希。
- `internal/fixture`：全部确定性样例。
- `internal/pipeline`：演示用例编排。
