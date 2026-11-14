# rtpcap — 本地 RTSP/RTP 采集留证工具

单进程、纯回环的 RTSP/RTP 采集与可复算证据报告工具。模拟服务端在本机
`127.0.0.1` 上脚本化推流，客户端通过显式状态机完成握手，按 TCP interleaved
或双端口 UDP 收流，重组 RTP 帧并落盘，输出文本与 JSON 报告以及逐帧索引。

## 目录结构

- `cmd/rtpcap`：`capture` / `verify` 子命令与退出码。
- `cmd/genfixtures`：重新生成 `testdata/scenarios` 下的场景与期望值（期望值由
  真实重组器离线复算，保证夹具与实现不漂移）。
- `internal/rtsp/statemachine.go`：显式握手状态机，非法迁移计入失败次数。
- `internal/rtsp/client.go`：OPTIONS→TEARDOWN 握手编排与 Digest 鉴权。
- `internal/rtsp/transport_tcp.go` / `transport_udp.go`：两种收包路径，同一
  `Transport` 接口，输出字段完全一致。
- `internal/rtp/`：乱序重排、重复丢弃、16 位序号回绕、窗口封顶、缺口统计与组帧。
- `internal/mockserver/`：脚本化 RTSP 服务端，可注入丢包/乱序/重复/迟到/断流/401。
- `internal/scenario/`：场景定义、确定性 RTP 数据与发送计划。
- `internal/store/`：帧落盘与 `index.jsonl` 续采索引。
- `internal/report/`：JSON/文本报告与期望校验。

## 常用命令

```sh
go build ./...

# 生成夹具（随代码提交，通常无需重跑）
go run ./cmd/genfixtures

# 两种传输采集
go run ./cmd/rtpcap capture --scenario testdata/scenarios/lossy.json \
  --transport tcp --out out/tcp --report out/tcp/report.json
go run ./cmd/rtpcap capture --scenario testdata/scenarios/lossy.json \
  --transport udp --out out/udp --report out/udp/report.json

# 报告 + 索引 + 期望三方校验
go run ./cmd/rtpcap verify --report out/tcp/report.json \
  --index out/tcp/index.jsonl --expect testdata/scenarios/lossy.expect.json
```

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 采集成功 / 校验通过 |
| 2 | 参数或场景文件错误 |
| 3 | 报告与索引/期望不一致 |
| 4 | 握手或传输失败（含 401、读超时、断流） |
| 5 | 监听地址非 `127.0.0.1` 被拒绝 |

## lossy 场景验收指标

状态迁移固定 6 次（Init、Options、Describe、Setup、Play、Pause、Teardown）；
`rtp_received≈1192`、`rtp_duplicates=9`、`rtp_reordered=14`、3 段缺口共 17 包、
`max_reorder_depth=7`、重组 396 帧、约 1.50 MiB，首末序号 1000..2199。
TCP 与 UDP 报告除 `transport` 字段外逐字段一致。

## 续采语义

输出目录中的 `index.jsonl` 记录每帧序号区间与摘要。重开同一目录时，客户端
从索引末帧的下一序号续传，帧索引续写、不重复不丢帧；进程内断流则按重组器
前沿自动重连续传。
