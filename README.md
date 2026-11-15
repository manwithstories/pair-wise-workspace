# rcctl

`rcctl` 是一个纯本地、确定性的离线码率控制与质量回归工具。它不调用外部转码器，
样例 YUV420 素材由固定种子在运行时生成，输出计划、追加 journal、checkpoint 和 JSON 报告。

## 快速开始

```bash
go run ./cmd/rcctl plan --clip news --frames 240 --gop 20 --target-kbps 1200 --out work/plan.json
go run ./cmd/rcctl run --plan work/plan.json --workers 3 --journal work/journal.log
go run ./cmd/rcctl resume --plan work/plan.json --journal work/journal.log
go run ./cmd/rcctl verify --report work/report.json --max-dev-pct 5 --max-psnr-std 1.5
```

`run`/`resume` 默认把报告写到 journal 同目录的 `report.json`，也可用 `--report` 指定。
演示中断恢复时，在 `run` 执行期间发送 `kill -9`，再运行 `resume`；journal 中已经出现的
`segment_done`/`checkpoint` 段不会再次编码。

## 子命令

- `plan`：按 GOP 切段，以 q=32 做首遍复杂度统计，再按复杂度与硬切权重分配段目标码率。
- `run`：并发执行段任务；每帧由 VBV 缓冲余量回调量化步长，段级搜索靠近目标字节数。
- `resume`：校验 journal 与 plan 一致后，只执行未完成段。
- `verify`：检查每段最大绝对码率偏差及跨段 PSNR 标准差，列出所有越界项。
- `report`：从 plan 与 journal 重新生成 JSON 报告并打印终端表。

`plan` 和 `run` 接受 `--fps`、`--width`、`--height`、`--seed` 等可选参数。默认源为
176x144、30 fps；宽度和高度必须是 16 的倍数。

## 退出码

| 退出码 | 含义 |
| --- | --- |
| 0 | 成功或阈值通过 |
| 1 | I/O 或运行时错误 |
| 2 | verify 阈值不达标，或存在重试耗尽的段 |
| 3 | 参数、plan/report 格式非法 |
| 4 | journal 与 plan 不一致 |

使用 `go run` 时，Go 工具本身可能把程序的非零状态折叠为 1；脚本中需要精确退出码时，
请先 `go build -o rcctl ./cmd/rcctl` 再运行二进制。

## 实现要点

- `internal/source`：固定种子合成运动、暗场和每 60 帧硬切；帧访问无共享可变状态。
- `internal/codec`：8x8 正交 DCT、量化/反量化、zigzag RLE、varint 熵编码；段首帧内、
  后续帧间预测，并输出每帧字节数与重建帧。
- `internal/ratecontrol`：四帧容量 VBV、量化倍率反馈，以及保持总预算的复杂度/硬切分配器。
- `internal/state`：严格状态迁移；每段完成立即 fsync journal，并追加含完成结果的 checkpoint。
- `internal/scheduler`：有界 worker 池、指数退避重试；单段失败不阻塞其他段。
- `internal/metrics`：逐帧亮度 PSNR、段内均值/标准差和跨段聚合。
- `internal/report`：JSON 落盘、终端表、偏差与 PSNR 标准差校验。

journal 的损坏行会被跳过并在 stderr 告警。目标低于内核最小码率时，量化器触顶会被记录为
`"capped": true`，任务继续执行，最终由 `verify` 判断是否可交付。

## 测试

```bash
go test ./...
go test -race ./...
```

测试覆盖 DCT 往返、确定性合成、状态机、journal 坏行与 checkpoint 回放、指数重试、
VBV/分配器、PSNR、报告阈值以及小规模 plan/run/journal 集成流程。
