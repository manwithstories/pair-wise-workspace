# relay — 多机位帧序列中继处理内核

现场演出结束后，把多台相机采集的带元数据/校验和的帧序列，快速、可核验地
交付给剪辑与归档团队。`relay` 是纯终端、无外部服务依赖的可复用处理内核，
完成多格式解复用、机位→轨道映射、时间线对齐、有背压流转封装与可恢复执行。

## 技术栈与约束

- Go 1.25.6（`go.mod` 固定 `go 1.25.6`），核心仅依赖标准库。
- 终端框架 `github.com/spf13/cobra v1.8.1`。
- PNG/JPEG 解码校验 `github.com/disintegration/imaging v1.6.2`（只解码核验，绝不重编码）。
- 仅处理自生成本地样例，不申请设备权限，支持 macOS arm64 与 x86_64。

## 架构与统一模型

```
MFS/1 长度前缀流 ┐
                 ├─ demux ─ media.Frame(统一帧, 纳秒时间, SHA256+CRC32C)
MTA/1 TAR 归档   ┘                         │
                                          ▼
                       mapping(机位→轨道, 时钟校正, 幂等/冲突账本)
                                          │
                                          ▼
                timeline(每轨水位=最大事件时间−250ms, 迟到重算, 超窗隔离)
                                          │
                                          ▼
                  flow(字节额度有界管道, 慢消费者背压, 取消释放)
                                          │
                                          ▼
        remux(MFS/1 或 MTA/1 无损输出 + 逐帧来源清单) → 独立重读核验
                                          │
                          checkpoint(输入偏移/水位/已提交输出长度/账本)
```

| 包 | 职责 |
| --- | --- |
| `internal/media` | 统一帧模型、纳秒时间基准 `TimeNS`、统一错误 `Fault`、线格式编解码与校验 |
| `internal/demux` | MFS/1（小端长度前缀）与 MTA/1（TAR 成员）解复用，逐记录全量校验 |
| `internal/mapping` | 机位→稳定轨道、时钟偏移校正、同序号幂等/冲突判定 |
| `internal/timeline` | 每轨水位、窗内迟到帧按 (时间,轨道,序号) 确定性重排、超窗隔离 |
| `internal/flow` | 泛型有界管道，字节额度硬上限，背压与取消释放，峰值统计 |
| `internal/checkpoint` | 临时文件 + fsync + 原子改名，自校验和，输入/映射指纹，输出截断 |
| `internal/remux` | 双格式无损转封装、来源清单、imaging 图像核验、独立重读 |
| `internal/fixture` | 确定性自生成样例与全部故障夹具 |
| `internal/pipeline` | 生产者/消费者双 goroutine 编排、安全点检查点、确定性恢复 |
| `internal/cli`、`cmd/relay` | Cobra 命令适配器与终端入口 |

## 记录线格式（MFS/1 与 MTA/1 共用）

42 字节头 + 载荷 + 36 字节尾。头含版本、编码、机位、宽高、序号、事件时间
（纳秒）、载荷长度、标志、魔数 `MF01`；尾含载荷 SHA-256 与覆盖头+载荷的
CRC32C。MFS/1 在记录前加 4 字节小端长度前缀；MTA/1 把每条记录作为一个 TAR
常规成员。越界长度、未知版本、不完整记录、哈希/CRC 不符都会被拒绝，并在
首个错误处报告 `stage / byte_offset / camera / sequence / offset_error`。

## 命令与退出码

| 退出码 | 含义 |
| --- | --- |
| 0 | 完成 |
| 2 | 已定位的媒体损坏 |
| 3 | 参数错误 |
| 4 | 检查点/输入/映射校验不符 |
| 5 | 可恢复的模拟中断 |

```bash
go mod tidy
go build ./...
go test ./...

D="$(mktemp -d)"
go run ./cmd/relay fixture --dir "$D" --frames 10000 \
    --faults crc,truncated,late,duplicate,slow,interrupt
go run ./cmd/relay scan --in "$D/bad-crc.mfs";            # 退出码 2
go run ./cmd/relay run --in "$D/live.mfs" --out "$D/out.mta" \
    --map "$D/map.json" --checkpoint "$D/run.ckpt" \
    --max-buffer 64MiB --interrupt-after 6000             # 退出码 5
go run ./cmd/relay resume --checkpoint "$D/run.ckpt" \
    --in "$D/live.mfs" --out "$D/out.mta" --map "$D/map.json" --max-buffer 64MiB
go run ./cmd/relay digest --in "$D/live.mfs" --out "$D/in.payload"
go run ./cmd/relay digest --in "$D/out.mta"  --out "$D/out.payload"
shasum -a 256 "$D/in.payload" "$D/out.payload"            # 两条摘要相同
```

> 提示：`go run` 会把子进程退出码包装为 1；验收精确退出码时先
> `go build -o relay ./cmd/relay` 再执行二进制。

## 关键正确性保证

- **无损**：转封装只原样保留图像载荷并重算线格式校验，独立重读逐帧比对
  帧数、轨道、载荷 SHA-256 与图像可解码性。
- **不丢帧**：有界管道以字节额度背压，慢归档盘只会阻塞生产者；取消即释放
  额度，峰值缓冲不超过 `--max-buffer`（验收 ≤ 64MiB / 67108864 字节）。
- **确定性**：去重账本仅在帧落盘后固化；检查点记录 `safeOffset`，恢复时
  截断未提交输出尾部并从该偏移重放，回溯区在途帧重新参与时间线重算。
- **不静默覆盖**：同机位同序号哈希不同即隔离并记录原因，哈希相同才幂等忽略。
- **校验不符即停**：输入或映射指纹与检查点不一致时返回退出码 4，不覆盖产物。
- **可复现**：相同输入与映射重复运行，载荷摘要与来源清单摘要逐字节一致。
