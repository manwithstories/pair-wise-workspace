# rtspreplay — RTSP 取流自研网关（演示/自检）

设备侧 RTSP 取流的控制面状态机与媒体面重组实现。两种传输（UDP 单播、
TCP 交错）共用同一个会话状态机实例，仅替换传输实现。仅连接
`127.0.0.1` 的本地模拟服务，不访问任何外部目标。

## 构建

```sh
go build ./...
```

## 自检（验收）

```sh
go run ./cmd/rtspreplay selftest --transport both \
    --faults drop=4,dup=2,reorder=3 --packets 500 --out out/report.json
```

终端打印单行 JSON，例如：

```json
{"modes":{"udp":{"transitions":6,"gaps":4,"frames":120,"duplicates":2},"tcp":{"transitions":6,"gaps":4,"frames":120,"duplicates":2}}}
```

## 对接本地模拟服务

```sh
go run ./cmd/mockrtsp -addr 127.0.0.1:8554 -faults drop=4,dup=2,reorder=3 &
go run ./cmd/rtspreplay stream --url rtsp://127.0.0.1:8554/live --transport tcp
```

## 退出码

- `0`：指标在预算内（`--min-frames` / `--max-gaps`）
- `1`：内部错误（连接失败、非法迁移、媒体通道中断等）
- `2`：缺口或帧数越界

## 架构

| 模块 | 职责 |
| --- | --- |
| `cmd/rtspreplay` | 子命令（`selftest`/`stream`）、参数、退出码 |
| `cmd/mockrtsp` | 本地模拟服务，注入丢包/重复/乱序，只监听 127.0.0.1 |
| `internal/session` | 会话状态机：INIT..TEARDOWN、合法迁移、迁移计数、超时守卫 |
| `internal/rtsp` | 传输抽象（统一 CSeq、RTP 投递）、UDP 偶奇端口与 RTCP、交错 `$` 帧拼包、摘要认证 |
| `internal/sdp` | control 绝对/相对地址还原、payload type 与 clock rate |
| `internal/rtp` | 按 seq/时间戳重组（回绕、重复、窗口内乱序）、缺口统计、H.264 成帧 |
| `internal/stats` | 指标汇总，JSON 落盘 |
| `internal/client` | 入口：选传输驱动状态机 OPTIONS→PLAY，汇总指标 |
| `internal/mocksrv` | 模拟服务实现：摘要认证、SDP、流生成与故障注入 |

关键行为：

- 非法迁移被拒绝；SETUP 未成功时 FSM 不存在到 PLAYING 的边，PLAY 发不出去。
- 任何异常（含服务端中途断开、状态超时）一律收敛到 TEARDOWN 并关闭端口，
  未完成帧丢弃，已封帧与统计照常输出。
- DESCRIBE 401 时按摘要认证重试一次，CSeq 递增、nonce 一致。
- 缺口超过阈值（默认 0）丢弃当前不完整帧并计数，已封帧不回退。
