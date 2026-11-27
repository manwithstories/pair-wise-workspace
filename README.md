# tscheck — 多人节奏手游音视频接入的 Go 媒体处理内核

面向蜂窝/Wi-Fi 切换场景的 MPEG-TS 处理内核：三档码率支路在共同 27 MHz
时间基准下维持节拍，断线后从可信检查点恢复。终端入口仅用于生成样例、
运行检查与打印核验数据。

## 构建与验收

```sh
go mod tidy && go build ./... && go test ./...
go run ./cmd/tscheck selfcheck --seed 41 --bytes 2097152 --out /tmp/tscheck-run
# 或：scripts/selfcheck.sh
```

自检覆盖正常三码率、故障注入（PCR 回绕、连续计数跳变、PSI 版本更新与
错误 CRC、截断、VFR）、断点恢复三条路径，输出 JSON 核验数据：
`damage_offset_error_bytes`（≤187）、`resume_ms`（2 MiB 样例 <500）、
`boundary_delta_ticks`（合格为 0）、`sha256`、`repeat_sha256_equal`。

退出码：0 全部通过；2 参数或输入无效；3 存在不可恢复区段或边界冲突。

## 子命令

- `gen`：按种子生成确定性样例流（`--faults cc,crc,wrap,bump,truncate`）。
- `check`：扫描单个 TS 文件，输出包/PSI/时钟/GOP 分析 JSON。
- `selfcheck`：三路径自检，打印核验 JSON。

## 架构

```
internal/model     27 MHz 时间基准、跨模块错误分类
internal/ts        188 字节包扫描（astits 解析）、PAT/PMT 与版本/CRC
internal/clock     PCR/PTS 33 位回绕展开、有界漂移估算
internal/analyze   轨道结构、PES 组装、H.264 IDR 与关键帧分位数
internal/fanout    三码率并行支路、共享时钟边界汇合
internal/recovery  可信边界检查点与幂等断点续跑
internal/fixture   确定性样例与故障注入（固定种子逐字节一致）
cmd/tscheck        终端入口
```

数据流：解析子系统（包、PSI、统一错误模型）→ 时钟子系统（展开后的媒
体时间）→ 分析与支路调度；各码率支路独立保存检查点并恢复，汇合模块
在共享时钟下交叉核验关键帧边界（差异超过一帧即冲突）。

## 约束

- Go 1.25.6（go.mod 固定 `go 1.25.6`，禁止更换）。
- MPEG-TS 解复用与包解析使用 github.com/asticode/go-astits v1.16.0。
- 单 PES 缓冲上限 1 MiB，并发支路上限 3，拒绝超界长度与无效时间戳。
- 样例数据随代码生成，不引用外部文件；输出写入 /tmp。
